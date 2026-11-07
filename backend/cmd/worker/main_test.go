package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

// fakeProcessor returns a fixed result per request ID and advances the handler's clock by cost
// for every call, so tests can control how much of the deadline each record uses.
type fakeProcessor struct {
	errs  map[string]error
	calls []reservations.QueueMessage
	clock *time.Time
	cost  time.Duration
	ctxs  []context.Context
}

func (f *fakeProcessor) Process(ctx context.Context, msg reservations.QueueMessage) (reservations.Recorded, error) {
	f.calls = append(f.calls, msg)
	f.ctxs = append(f.ctxs, ctx)
	*f.clock = f.clock.Add(f.cost)
	if err := f.errs[msg.RequestID]; err != nil {
		return reservations.Recorded{}, err
	}
	return reservations.Recorded{Reservation: reservations.Reservation{
		RequestID: msg.RequestID, TrainerID: msg.TrainerID, CardID: msg.CardID, Status: reservations.StatusConfirmed,
	}}, nil
}

type harness struct {
	h     *handler
	proc  *fakeProcessor
	clock *time.Time
	logs  *strings.Builder
}

func newHarness() *harness {
	clock := time.Now()
	proc := &fakeProcessor{errs: map[string]error{}, clock: &clock}
	logs := &strings.Builder{}
	h := &handler{
		log:  slog.New(slog.NewJSONHandler(logs, nil)),
		proc: proc,
		now:  func() time.Time { return clock },
	}
	return &harness{h: h, proc: proc, clock: &clock, logs: logs}
}

// ctxWithin returns a context whose deadline is d after the harness clock.
func (hs *harness) ctxWithin(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), hs.clock.Add(d))
	t.Cleanup(cancel)
	return ctx
}

func record(t *testing.T, messageID, requestID string) events.SQSMessage {
	t.Helper()
	body, err := json.Marshal(reservations.QueueMessage{RequestID: requestID, TrainerID: "trainer-a", CardID: "card-001"})
	if err != nil {
		t.Fatal(err)
	}
	return events.SQSMessage{MessageId: messageID, Body: string(body), Attributes: map[string]string{"ApproximateReceiveCount": "1"}}
}

func failedIDs(resp events.SQSEventResponse) []string {
	ids := []string{}
	for _, f := range resp.BatchItemFailures {
		ids = append(ids, f.ItemIdentifier)
	}
	return ids
}

func equal(a, b []string) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func TestHandleSuccessfulBatchReportsNoFailures(t *testing.T) {
	hs := newHarness()
	ev := events.SQSEvent{Records: []events.SQSMessage{record(t, "m1", "r1"), record(t, "m2", "r2")}}

	resp, err := hs.h.handle(hs.ctxWithin(t, 10*time.Second), ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.BatchItemFailures) != 0 || len(hs.proc.calls) != 2 {
		t.Fatalf("failures = %v, calls = %d", failedIDs(resp), len(hs.proc.calls))
	}
	// An empty list (not null) tells Lambda the whole batch succeeded.
	b, _ := json.Marshal(resp)
	if string(b) != `{"batchItemFailures":[]}` {
		t.Fatalf("response = %s", b)
	}
	if !strings.Contains(hs.logs.String(), `"outcome":"CONFIRMED"`) {
		t.Fatalf("outcome not logged: %s", hs.logs.String())
	}
}

func TestHandleReportsOnlyFailedRecords(t *testing.T) {
	hs := newHarness()
	hs.proc.errs["r2"] = fmt.Errorf("%w: transaction conflict", reservations.ErrRetryable)
	hs.proc.errs["r3"] = fmt.Errorf("%w: card missing", reservations.ErrPermanent)
	ev := events.SQSEvent{Records: []events.SQSMessage{
		record(t, "m1", "r1"), record(t, "m2", "r2"), record(t, "m3", "r3"), record(t, "m4", "r4"),
	}}

	resp, err := hs.h.handle(hs.ctxWithin(t, 10*time.Second), ev)
	if err != nil {
		t.Fatalf("a per-record failure must not fail the invocation: %v", err)
	}
	if got := failedIDs(resp); !equal(got, []string{"m2", "m3"}) {
		t.Fatalf("failures = %v, want [m2 m3]", got)
	}
	if len(hs.proc.calls) != 4 {
		t.Fatalf("calls = %d, want every record processed", len(hs.proc.calls))
	}
	logs := hs.logs.String()
	if !strings.Contains(logs, `"level":"WARN","msg":"message will be retried"`) ||
		!strings.Contains(logs, `"level":"ERROR","msg":"message failed permanently"`) {
		t.Fatalf("retryable must log WARN and permanent ERROR: %s", logs)
	}
}

func TestHandleReportsMalformedBodiesWithoutProcessing(t *testing.T) {
	hs := newHarness()
	ev := events.SQSEvent{Records: []events.SQSMessage{
		{MessageId: "bad", Body: "not json"},
		record(t, "good", "r1"),
	}}

	resp, _ := hs.h.handle(hs.ctxWithin(t, 10*time.Second), ev)
	if got := failedIDs(resp); !equal(got, []string{"bad"}) {
		t.Fatalf("failures = %v, want [bad]", got)
	}
	if len(hs.proc.calls) != 1 {
		t.Fatalf("calls = %d, want only the valid record", len(hs.proc.calls))
	}
	if !strings.Contains(hs.logs.String(), `"level":"ERROR"`) {
		t.Fatal("malformed body must be logged at ERROR")
	}
}

func TestHandleNearDeadlineReturnsRemainingRecordsUnprocessed(t *testing.T) {
	hs := newHarness()
	hs.proc.cost = 2 * time.Second
	ev := events.SQSEvent{Records: []events.SQSMessage{
		record(t, "m1", "r1"), record(t, "m2", "r2"), record(t, "m3", "r3"), record(t, "m4", "r4"),
	}}

	// 5 s left: m1 starts at 5 s, m2 at 3 s, then only 1 s is left (< 1.5 s margin).
	resp, err := hs.h.handle(hs.ctxWithin(t, 5*time.Second), ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs.proc.calls) != 2 {
		t.Fatalf("calls = %d, want 2 before the margin", len(hs.proc.calls))
	}
	if got := failedIDs(resp); !equal(got, []string{"m3", "m4"}) {
		t.Fatalf("failures = %v, want the unprocessed [m3 m4]", got)
	}
	if !strings.Contains(hs.logs.String(), `"unprocessed":2`) {
		t.Fatalf("early return not logged: %s", hs.logs.String())
	}
}

func TestHandleWithLessThanMarginProcessesNothing(t *testing.T) {
	hs := newHarness()
	ev := events.SQSEvent{Records: []events.SQSMessage{record(t, "m1", "r1"), record(t, "m2", "r2")}}

	resp, _ := hs.h.handle(hs.ctxWithin(t, time.Second), ev)
	if len(hs.proc.calls) != 0 {
		t.Fatalf("calls = %d, want none", len(hs.proc.calls))
	}
	if got := failedIDs(resp); !equal(got, []string{"m1", "m2"}) {
		t.Fatalf("failures = %v", got)
	}
}

func TestHandleBoundsEachRecordByDeadlineMinusMargin(t *testing.T) {
	hs := newHarness()
	ctx := hs.ctxWithin(t, 10*time.Second)
	want, _ := ctx.Deadline()

	if _, err := hs.h.handle(ctx, events.SQSEvent{Records: []events.SQSMessage{record(t, "m1", "r1")}}); err != nil {
		t.Fatal(err)
	}
	got, ok := hs.proc.ctxs[0].Deadline()
	if !ok || !got.Equal(want.Add(-deadlineMargin)) {
		t.Fatalf("record deadline = %v, want %v", got, want.Add(-deadlineMargin))
	}
}

func TestHandleWithoutDeadlineProcessesEverything(t *testing.T) {
	hs := newHarness()
	hs.proc.cost = time.Hour
	ev := events.SQSEvent{Records: []events.SQSMessage{record(t, "m1", "r1"), record(t, "m2", "r2")}}

	resp, _ := hs.h.handle(context.Background(), ev)
	if len(hs.proc.calls) != 2 || len(resp.BatchItemFailures) != 0 {
		t.Fatalf("calls = %d, failures = %v", len(hs.proc.calls), failedIDs(resp))
	}
}
