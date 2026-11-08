// Command worker is the Lambda that consumes the reservation queue. It only decodes SQS records,
// hands each one to reservations.Processor, logs the result and reports failed records so SQS
// redelivers them (and, after the retry limit, moves them to the dead letter queue). The rules
// live in internal/reservations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

// deadlineMargin is the time kept in reserve before the invocation deadline. With less than this
// left, the remaining records are reported as failures instead of risking a timeout, which would
// make SQS redeliver the whole batch.
const deadlineMargin = 1500 * time.Millisecond

type processor interface {
	Process(ctx context.Context, msg reservations.QueueMessage) (reservations.Recorded, error)
}

type handler struct {
	log  *slog.Logger
	proc processor
	now  func() time.Time
	// failAfterCommit is the dev-only failure injection; failOff in every other stage.
	failAfterCommit failMode
}

// failMode is the FAIL_AFTER_COMMIT failure injection, which CDK only lets the dev stage set. It
// makes a record fail after its outcome is stored and before the handler reports it done, as if
// the worker died at the worst moment, so SQS redelivers a message whose work already committed.
type failMode string

const (
	failOff failMode = ""
	// failOnce fails a record only when this delivery stored the outcome, so the redelivery finds
	// the duplicate and succeeds.
	failOnce failMode = "once"
	// failAlways fails every delivery, so a committed request reaches the dead letter queue.
	failAlways failMode = "always"
)

func parseFailMode(v string) (failMode, error) {
	switch v {
	case "", "off":
		return failOff, nil
	case string(failOnce), string(failAlways):
		return failMode(v), nil
	}
	return failOff, fmt.Errorf("FAIL_AFTER_COMMIT must be once, always or off, got %q", v)
}

// injectFailure reports whether the failure flag fails this successfully processed record.
func (h *handler) injectFailure(rec reservations.Recorded) bool {
	switch h.failAfterCommit {
	case failAlways:
		return true
	case failOnce:
		return !rec.Duplicate
	}
	return false
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	fail, err := parseFailMode(os.Getenv("FAIL_AFTER_COMMIT"))
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if fail != failOff {
		log.Warn("failure injection enabled: records fail after their outcome is stored", "injectedFailure", string(fail))
	}
	proc, err := newProcessor(context.Background(), log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	h := &handler{log: log, proc: proc, now: time.Now, failAfterCommit: fail}
	lambda.Start(h.handle)
}

func newProcessor(ctx context.Context, log *slog.Logger) (*reservations.Processor, error) {
	tables, err := storage.TableNamesFromEnv()
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	return reservations.NewProcessor(storage.NewLedger(dynamodb.NewFromConfig(cfg), tables), log), nil
}

// Outcomes logged for records that did not reach a terminal reservation status. Terminal records
// log the stored status (CONFIRMED or SOLD_OUT) instead.
const (
	outcomeRetry  = "RETRY"
	outcomeFailed = "FAILED"
)

// handle processes the batch in order and reports every record that did not reach a terminal
// outcome. It never returns an error, so one bad record cannot fail the batch.
func (h *handler) handle(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
	resp := events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{}}
	deadline, hasDeadline := ctx.Deadline()

	for i, record := range ev.Records {
		if hasDeadline && deadline.Sub(h.now()) < deadlineMargin {
			h.log.Warn("deadline near, returning unprocessed records to the queue",
				"remainingMs", deadline.Sub(h.now()).Milliseconds(), "unprocessed", len(ev.Records)-i)
			for _, rest := range ev.Records[i:] {
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: rest.MessageId})
				log, _, _ := h.recordLog(rest)
				log.Warn("message will be retried", "outcome", outcomeRetry, "reason", "deadline", "durationMs", int64(0))
			}
			break
		}

		recordCtx, cancel := ctx, context.CancelFunc(func() {})
		if hasDeadline {
			// Leave the margin for logging and returning the response.
			recordCtx, cancel = context.WithDeadline(ctx, deadline.Add(-deadlineMargin))
		}
		ok := h.processRecord(recordCtx, record)
		cancel()
		if !ok {
			resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: record.MessageId})
		}
	}
	return resp, nil
}

// recordLog decodes the record body and returns a logger carrying the fields every record's log
// line has: messageId, requestId, cardId and attempt (the SQS receive count). requestId and cardId
// are empty when the body cannot be decoded, which is reported as the error.
func (h *handler) recordLog(record events.SQSMessage) (*slog.Logger, reservations.QueueMessage, error) {
	var msg reservations.QueueMessage
	err := json.Unmarshal([]byte(record.Body), &msg)
	if err != nil {
		msg = reservations.QueueMessage{}
	}
	attempt, _ := strconv.Atoi(record.Attributes["ApproximateReceiveCount"])
	log := h.log.With("messageId", record.MessageId, "requestId", msg.RequestID, "cardId", msg.CardID, "attempt", attempt)
	return log, msg, err
}

// processRecord reports whether the record reached a terminal outcome and can be deleted. It logs
// exactly one line for the record, with its outcome and duration.
func (h *handler) processRecord(ctx context.Context, record events.SQSMessage) bool {
	start := h.now()
	log, msg, err := h.recordLog(record)
	if err != nil {
		log.Error("message failed permanently", "outcome", outcomeFailed,
			"reason", "body is not a reservation message", "err", err, "durationMs", h.now().Sub(start).Milliseconds())
		return false
	}

	rec, err := h.proc.Process(ctx, msg)
	durationMs := h.now().Sub(start).Milliseconds()
	switch {
	case err == nil && h.injectFailure(rec):
		log.Warn("injected failure after commit, message will be retried", "outcome", rec.Reservation.Status,
			"duplicate", rec.Duplicate, "injectedFailure", string(h.failAfterCommit), "durationMs", durationMs)
	case err == nil:
		log.Info("reservation processed", "outcome", rec.Reservation.Status, "duplicate", rec.Duplicate, "durationMs", durationMs)
		return true
	case errors.Is(err, reservations.ErrPermanent):
		log.Error("message failed permanently", "outcome", outcomeFailed, "err", err, "durationMs", durationMs)
	default:
		log.Warn("message will be retried", "outcome", outcomeRetry, "err", err, "durationMs", durationMs)
	}
	return false
}
