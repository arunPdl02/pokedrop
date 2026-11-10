package reservations

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeLedger keeps outcomes keyed by (trainer, request) and stock per card, applying the same
// conditions as the real transaction. errs, when set, are returned (in order) before any write.
type fakeLedger struct {
	stock    map[string]int
	outcomes map[[2]string]Reservation
	errs     []error
	soldErrs []error
	reserves int
	soldOuts int
}

func newFakeLedger(stock map[string]int) *fakeLedger {
	return &fakeLedger{stock: stock, outcomes: map[[2]string]Reservation{}}
}

func (f *fakeLedger) Reserve(_ context.Context, r Reservation) (Recorded, error) {
	f.reserves++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return Recorded{}, err
	}
	key := [2]string{r.TrainerID, r.RequestID}
	if existing, ok := f.outcomes[key]; ok {
		return Recorded{Reservation: existing, Duplicate: true}, nil
	}
	if f.stock[r.CardID] < 1 {
		return Recorded{}, ErrOutOfStock
	}
	f.stock[r.CardID]--
	f.outcomes[key] = r
	return Recorded{Reservation: r}, nil
}

func (f *fakeLedger) RecordSoldOut(_ context.Context, r Reservation) (Recorded, error) {
	f.soldOuts++
	if len(f.soldErrs) > 0 {
		err := f.soldErrs[0]
		f.soldErrs = f.soldErrs[1:]
		return Recorded{}, err
	}
	key := [2]string{r.TrainerID, r.RequestID}
	if existing, ok := f.outcomes[key]; ok {
		return Recorded{Reservation: existing, Duplicate: true}, nil
	}
	f.outcomes[key] = r
	return Recorded{Reservation: r}, nil
}

const (
	trainerA = "trainer-a"
	reqID    = "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b"
)

var fixedNow = time.Date(2026, 11, 7, 20, 15, 30, 999_000_000, time.UTC)

// testProcessor returns a Processor with a fixed clock and a sleep that only counts.
func testProcessor(l Ledger) (*Processor, *[]time.Duration) {
	p := NewProcessor(l, nil)
	p.now = func() time.Time { return fixedNow }
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return p, &slept
}

func msg(requestID, cardID string) QueueMessage {
	return QueueMessage{RequestID: requestID, TrainerID: trainerA, CardID: cardID}
}

func TestProcessConfirmsAndTakesOneUnit(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 2})
	p, _ := testProcessor(l)

	rec, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if err != nil {
		t.Fatal(err)
	}
	want := Reservation{RequestID: reqID, TrainerID: trainerA, CardID: "card-001", Status: StatusConfirmed,
		UpdatedAt: fixedNow.Truncate(time.Second)}
	if rec.Duplicate || rec.Reservation != want {
		t.Fatalf("got %+v, want %+v", rec, want)
	}
	if l.stock["card-001"] != 1 {
		t.Fatalf("stock = %d, want 1", l.stock["card-001"])
	}
}

func TestProcessNormalizesRequestID(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 5})
	p, _ := testProcessor(l)

	if _, err := p.Process(context.Background(), msg("3F2B8C1E-6D4A-4A57-9B0E-1C2D3E4F5A6B", "card-001")); err != nil {
		t.Fatal(err)
	}
	rec, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || l.stock["card-001"] != 4 {
		t.Fatalf("upper and lower case IDs must be one request: dup=%v stock=%d", rec.Duplicate, l.stock["card-001"])
	}
}

func TestProcessDuplicateReturnsOriginalOutcomeWithoutTakingStock(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 5})
	p, _ := testProcessor(l)

	first, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec, err := p.Process(context.Background(), msg(reqID, "card-001"))
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Duplicate || rec.Reservation != first.Reservation {
			t.Fatalf("copy %d: got %+v, want original %+v", i, rec, first.Reservation)
		}
	}
	if l.stock["card-001"] != 4 {
		t.Fatalf("stock = %d, want 4 (one decrement for three copies)", l.stock["card-001"])
	}
}

func TestProcessDuplicateWithDifferentCardKeepsFirstOutcome(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 1, "card-002": 1})
	p, _ := testProcessor(l)

	if _, err := p.Process(context.Background(), msg(reqID, "card-001")); err != nil {
		t.Fatal(err)
	}
	rec, err := p.Process(context.Background(), msg(reqID, "card-002"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation.CardID != "card-001" || l.stock["card-002"] != 1 {
		t.Fatalf("got %+v, card-002 stock %d", rec, l.stock["card-002"])
	}
}

func TestProcessSoldOutIsATerminalOutcome(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 0})
	p, _ := testProcessor(l)

	rec, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicate || rec.Reservation.Status != StatusSoldOut {
		t.Fatalf("got %+v, want new SOLD_OUT", rec)
	}
	if l.stock["card-003"] != 0 {
		t.Fatalf("stock = %d, want 0", l.stock["card-003"])
	}
}

func TestProcessDuplicateOfConfirmedAfterStockRunsOutStaysConfirmed(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 1})
	p, _ := testProcessor(l)

	if _, err := p.Process(context.Background(), msg(reqID, "card-003")); err != nil {
		t.Fatal(err)
	}
	rec, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reservation.Status != StatusConfirmed || !rec.Duplicate {
		t.Fatalf("got %+v, want the original CONFIRMED", rec)
	}
	if l.soldOuts != 0 {
		t.Fatalf("RecordSoldOut called %d times for a duplicate", l.soldOuts)
	}
}

// A concurrent copy of the request records an outcome between our out-of-stock result and our
// SOLD_OUT write. The conditional write returns that outcome instead of overwriting it.
func TestProcessSoldOutLosesToConcurrentOutcome(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 0})
	confirmed := Reservation{RequestID: reqID, TrainerID: trainerA, CardID: "card-003", Status: StatusConfirmed, UpdatedAt: fixedNow}
	p, _ := testProcessor(&raceLedger{fakeLedger: l, before: func() {
		l.outcomes[[2]string{trainerA, reqID}] = confirmed
	}})

	rec, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation != confirmed {
		t.Fatalf("got %+v, want the concurrent CONFIRMED", rec)
	}
}

// raceLedger runs before() just ahead of RecordSoldOut.
type raceLedger struct {
	*fakeLedger
	before func()
}

func (r *raceLedger) RecordSoldOut(ctx context.Context, res Reservation) (Recorded, error) {
	r.before()
	return r.fakeLedger.RecordSoldOut(ctx, res)
}

func TestProcessRetriesRetryableErrorsThenSucceeds(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 1})
	l.errs = []error{
		fmt.Errorf("%w: transaction conflict", ErrRetryable),
		fmt.Errorf("%w: throttled", ErrRetryable),
	}
	p, slept := testProcessor(l)

	rec, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reservation.Status != StatusConfirmed || l.reserves != 3 || len(*slept) != 2 {
		t.Fatalf("got %+v after %d attempts and %d sleeps", rec, l.reserves, len(*slept))
	}
	for _, d := range *slept {
		if d < minBackoff || d > maxBackoff {
			t.Fatalf("backoff %v outside [%v, %v]", d, minBackoff, maxBackoff)
		}
	}
}

func TestProcessGivesUpAfterMaxAttempts(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 1})
	for i := 0; i < maxAttempts; i++ {
		l.errs = append(l.errs, fmt.Errorf("%w: conflict %d", ErrRetryable, i))
	}
	p, _ := testProcessor(l)

	_, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if !errors.Is(err, ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
	if l.reserves != maxAttempts || l.stock["card-001"] != 1 {
		t.Fatalf("attempts = %d, stock = %d", l.reserves, l.stock["card-001"])
	}
}

func TestProcessRetriesSoldOutWrite(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 0})
	l.soldErrs = []error{fmt.Errorf("%w: throttled", ErrRetryable)}
	p, _ := testProcessor(l)

	rec, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reservation.Status != StatusSoldOut || l.soldOuts != 2 {
		t.Fatalf("got %+v after %d sold-out writes", rec, l.soldOuts)
	}
}

func TestProcessStopsRetryingWhenContextEnds(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 1})
	l.errs = []error{fmt.Errorf("%w: conflict", ErrRetryable)}
	p, _ := testProcessor(l)
	p.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

	_, err := p.Process(context.Background(), msg(reqID, "card-001"))
	if !errors.Is(err, ErrRetryable) || l.reserves != 1 {
		t.Fatalf("err = %v after %d attempts, want ErrRetryable after 1", err, l.reserves)
	}
}

// Giving up mid-backoff must keep both the reason we were retrying and the reason we stopped, and
// must stay retryable so SQS redelivers the message.
func TestProcessContextEndedKeepsBothCauses(t *testing.T) {
	conflict := errors.New("transaction conflict")
	l := newFakeLedger(map[string]int{"card-001": 1})
	l.errs = []error{fmt.Errorf("%w: %w", ErrRetryable, conflict)}
	p, _ := testProcessor(l)
	p.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

	_, err := p.Process(context.Background(), msg(reqID, "card-001"))
	for _, want := range []error{ErrRetryable, conflict, context.DeadlineExceeded} {
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want it to wrap %v", err, want)
		}
	}
	if errors.Is(err, ErrPermanent) {
		t.Error("an interrupted retry must never become permanent")
	}
	if l.stock["card-001"] != 1 || len(l.outcomes) != 0 {
		t.Fatalf("nothing may be written: stock = %d, outcomes = %d", l.stock["card-001"], len(l.outcomes))
	}
}

func TestProcessSoldOutWriteGivesUpAfterMaxAttempts(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 0})
	for i := range maxAttempts {
		l.soldErrs = append(l.soldErrs, fmt.Errorf("%w: throttled %d", ErrRetryable, i))
	}
	p, _ := testProcessor(l)

	_, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if !errors.Is(err, ErrRetryable) || errors.Is(err, ErrOutOfStock) {
		t.Fatalf("err = %v, want ErrRetryable and not ErrOutOfStock", err)
	}
	if l.soldOuts != maxAttempts || len(l.outcomes) != 0 {
		t.Fatalf("sold-out writes = %d, outcomes = %d", l.soldOuts, len(l.outcomes))
	}
}

func TestProcessDoesNotRetryAPermanentSoldOutWrite(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-003": 0})
	l.soldErrs = []error{fmt.Errorf("%w: access denied", ErrPermanent)}
	p, slept := testProcessor(l)

	_, err := p.Process(context.Background(), msg(reqID, "card-003"))
	if !errors.Is(err, ErrPermanent) || l.soldOuts != 1 || len(*slept) != 0 {
		t.Fatalf("err = %v, sold-out writes = %d, sleeps = %d", err, l.soldOuts, len(*slept))
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a finished wait returned %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled wait returned %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a canceled wait must return at once, not wait out the timer")
	}
}

func TestProcessDoesNotRetryPermanentErrors(t *testing.T) {
	l := newFakeLedger(map[string]int{})
	l.errs = []error{fmt.Errorf("%w: card missing", ErrPermanent)}
	p, slept := testProcessor(l)

	_, err := p.Process(context.Background(), msg(reqID, "card-404"))
	if !errors.Is(err, ErrPermanent) || l.reserves != 1 || len(*slept) != 0 {
		t.Fatalf("err = %v, attempts = %d, sleeps = %d", err, l.reserves, len(*slept))
	}
}

func TestProcessRejectsInvalidMessagesWithoutWriting(t *testing.T) {
	cases := map[string]QueueMessage{
		"no trainer":       {RequestID: reqID, CardID: "card-001"},
		"bad request ID":   {RequestID: "not-a-uuid", TrainerID: trainerA, CardID: "card-001"},
		"empty request ID": {TrainerID: trainerA, CardID: "card-001"},
		"empty card":       {RequestID: reqID, TrainerID: trainerA},
		"control char":     {RequestID: reqID, TrainerID: trainerA, CardID: "card\n001"},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			l := newFakeLedger(map[string]int{"card-001": 1})
			p, _ := testProcessor(l)
			_, err := p.Process(context.Background(), m)
			if !errors.Is(err, ErrPermanent) {
				t.Fatalf("err = %v, want ErrPermanent", err)
			}
			if l.reserves != 0 {
				t.Fatal("ledger was called for an invalid message")
			}
		})
	}
}

func TestProcessKeepsOutcomesPerTrainer(t *testing.T) {
	l := newFakeLedger(map[string]int{"card-001": 2})
	p, _ := testProcessor(l)

	if _, err := p.Process(context.Background(), msg(reqID, "card-001")); err != nil {
		t.Fatal(err)
	}
	other := QueueMessage{RequestID: reqID, TrainerID: "trainer-b", CardID: "card-001"}
	rec, err := p.Process(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicate || l.stock["card-001"] != 0 {
		t.Fatalf("another trainer's request with the same ID is a separate request: %+v", rec)
	}
}

func TestBackoffStaysInBounds(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		for i := 0; i < 100; i++ {
			if d := backoff(attempt); d < minBackoff || d > maxBackoff {
				t.Fatalf("backoff(%d) = %v", attempt, d)
			}
		}
	}
}
