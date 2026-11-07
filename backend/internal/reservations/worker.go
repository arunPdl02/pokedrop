package reservations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Failure classes for processing a queued request. Errors returned by a Ledger or the Processor
// wrap exactly one of these, so callers branch with errors.Is.
var (
	// ErrRetryable means processing the same message again may succeed (conflicts, throttling,
	// timeouts). Retrying is always safe: a repeated request never consumes inventory twice.
	ErrRetryable = errors.New("retryable")
	// ErrPermanent means the message can never succeed (malformed, unknown card).
	ErrPermanent = errors.New("permanent")
	// ErrOutOfStock is returned by Ledger.Reserve when stock is below one and no outcome exists
	// for the request yet. It is not a failure: the Processor turns it into SOLD_OUT.
	ErrOutOfStock = errors.New("out of stock")
)

// Recorded is the outcome stored for a request. Duplicate is true when the outcome already
// existed, so this call changed nothing.
type Recorded struct {
	Reservation Reservation
	Duplicate   bool
}

// Ledger writes reservation outcomes. Both writes are conditional on no outcome existing for
// (TrainerID, RequestID); if one exists it is returned with Duplicate set and nothing changes.
type Ledger interface {
	// Reserve atomically takes one unit of stock and records r as CONFIRMED: both happen or
	// neither does. It returns ErrOutOfStock if stock is below one and no outcome exists.
	Reserve(ctx context.Context, r Reservation) (Recorded, error)
	// RecordSoldOut records r as SOLD_OUT without touching inventory.
	RecordSoldOut(ctx context.Context, r Reservation) (Recorded, error)
}

// Retry policy for retryable ledger errors inside one invocation. After the last attempt the
// error goes back to the caller so SQS redelivers the message.
const (
	maxAttempts = 3
	minBackoff  = 25 * time.Millisecond
	maxBackoff  = 200 * time.Millisecond
)

// Processor turns a queued request into a terminal outcome.
type Processor struct {
	ledger Ledger
	log    *slog.Logger
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

// NewProcessor returns a Processor writing through ledger. A nil log discards output.
func NewProcessor(ledger Ledger, log *slog.Logger) *Processor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Processor{ledger: ledger, log: log, now: time.Now, sleep: sleepCtx}
}

// Process records the outcome of msg and returns it. A request seen before returns its original
// outcome and leaves inventory untouched. Errors wrap ErrRetryable or ErrPermanent.
func (p *Processor) Process(ctx context.Context, msg QueueMessage) (Recorded, error) {
	r, err := reservationFrom(msg, p.now())
	if err != nil {
		return Recorded{}, err
	}

	rec, err := p.retry(ctx, func() (Recorded, error) { return p.ledger.Reserve(ctx, r) })
	if errors.Is(err, ErrOutOfStock) {
		// Sold out is a normal terminal outcome. The write is conditional too, so a concurrent
		// copy of this request that already recorded an outcome wins.
		r.Status = StatusSoldOut
		rec, err = p.retry(ctx, func() (Recorded, error) { return p.ledger.RecordSoldOut(ctx, r) })
	}
	if err != nil {
		return Recorded{}, err
	}

	if rec.Duplicate && rec.Reservation.CardID != r.CardID {
		// The same request ID was reused for another card. The first outcome stands.
		p.log.Warn("request ID reused with a different card",
			"requestId", r.RequestID, "storedCardId", rec.Reservation.CardID, "messageCardId", r.CardID)
	}
	return rec, nil
}

// retry runs op until it succeeds, fails with a non-retryable error, or runs out of attempts.
func (p *Processor) retry(ctx context.Context, op func() (Recorded, error)) (Recorded, error) {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var rec Recorded
		rec, err = op()
		if err == nil || !errors.Is(err, ErrRetryable) || attempt == maxAttempts {
			return rec, err
		}
		if serr := p.sleep(ctx, backoff(attempt)); serr != nil {
			return Recorded{}, fmt.Errorf("%w: %w (gave up waiting: %w)", ErrRetryable, err, serr)
		}
	}
	return Recorded{}, err
}

// backoff is a random wait in [minBackoff, min(minBackoff·2^attempt, maxBackoff)].
func backoff(attempt int) time.Duration {
	upper := min(minBackoff<<attempt, maxBackoff)
	return minBackoff + rand.N(upper-minBackoff+1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// reservationFrom validates a queue message and builds the CONFIRMED record to attempt. The API
// validated the request already; the worker checks again because the queue is a trust boundary.
func reservationFrom(msg QueueMessage, now time.Time) (Reservation, error) {
	if msg.TrainerID == "" {
		return Reservation{}, fmt.Errorf("%w: message has no trainerId", ErrPermanent)
	}
	requestID, ok := NormalizeRequestID(msg.RequestID)
	if !ok {
		return Reservation{}, fmt.Errorf("%w: requestId %q is not a UUID", ErrPermanent, msg.RequestID)
	}
	if msg.CardID == "" || !validCardID(msg.CardID) {
		return Reservation{}, fmt.Errorf("%w: invalid cardId %q", ErrPermanent, msg.CardID)
	}
	return Reservation{
		RequestID: requestID,
		TrainerID: msg.TrainerID,
		CardID:    msg.CardID,
		Status:    StatusConfirmed,
		// Stored as RFC 3339 seconds, so the returned value equals what a later read returns.
		UpdatedAt: now.UTC().Truncate(time.Second),
	}, nil
}
