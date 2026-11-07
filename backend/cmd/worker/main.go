// Command worker is the Lambda that consumes the reservation queue. It only decodes SQS records,
// hands each one to reservations.Processor, logs the result and reports failed records so SQS
// redelivers them (and, after the retry limit, moves them to the dead letter queue). The rules
// live in internal/reservations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
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
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	proc, err := newProcessor(context.Background(), log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	h := &handler{log: log, proc: proc, now: time.Now}
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

// handle processes the batch in order and reports every record that did not reach a terminal
// outcome. It never returns an error, so one bad record cannot fail the batch.
func (h *handler) handle(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
	resp := events.SQSEventResponse{BatchItemFailures: []events.SQSBatchItemFailure{}}
	deadline, hasDeadline := ctx.Deadline()

	for i, record := range ev.Records {
		if hasDeadline && deadline.Sub(h.now()) < deadlineMargin {
			for _, rest := range ev.Records[i:] {
				resp.BatchItemFailures = append(resp.BatchItemFailures, events.SQSBatchItemFailure{ItemIdentifier: rest.MessageId})
			}
			h.log.Warn("deadline near, returning unprocessed records to the queue",
				"remainingMs", deadline.Sub(h.now()).Milliseconds(), "unprocessed", len(ev.Records)-i)
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

// processRecord reports whether the record reached a terminal outcome and can be deleted.
func (h *handler) processRecord(ctx context.Context, record events.SQSMessage) bool {
	start := h.now()
	log := h.log.With("messageId", record.MessageId, "receiveCount", record.Attributes["ApproximateReceiveCount"])

	var msg reservations.QueueMessage
	if err := json.Unmarshal([]byte(record.Body), &msg); err != nil {
		log.Error("message failed permanently", "reason", "body is not a reservation message", "err", err)
		return false
	}
	log = log.With("requestId", msg.RequestID, "cardId", msg.CardID)

	rec, err := h.proc.Process(ctx, msg)
	durationMs := h.now().Sub(start).Milliseconds()
	switch {
	case err == nil:
		log.Info("reservation processed", "outcome", rec.Reservation.Status, "duplicate", rec.Duplicate, "durationMs", durationMs)
		return true
	case errors.Is(err, reservations.ErrPermanent):
		log.Error("message failed permanently", "err", err, "durationMs", durationMs)
	default:
		log.Warn("message will be retried", "err", err, "durationMs", durationMs)
	}
	return false
}
