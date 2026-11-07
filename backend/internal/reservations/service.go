package reservations

import (
	"context"
	"fmt"
	"sort"
)

// QueueMessage is what POST /reservations puts on the queue and the worker consumes. TrainerID
// is the validated token's sub claim, never a value supplied by the client.
type QueueMessage struct {
	RequestID string `json:"requestId"`
	TrainerID string `json:"trainerId"`
	CardID    string `json:"cardId"`
}

// Catalog is read access to the card catalog.
type Catalog interface {
	// Cards returns every card with its current availability.
	Cards(ctx context.Context) ([]Card, error)
	// CardExists reports whether cardID is in the catalog. It does not look at stock.
	CardExists(ctx context.Context, cardID string) (bool, error)
}

// Queue accepts reservation requests for the worker.
type Queue interface {
	// Enqueue returns nil only once the queue has durably accepted the message.
	Enqueue(ctx context.Context, msg QueueMessage) error
}

// Outcomes reads recorded reservation outcomes. Lookups are keyed by trainer and request ID, so a
// trainer can only ever reach their own records.
type Outcomes interface {
	// Get returns the recorded outcome, or (nil, nil) when none exists for that trainer.
	Get(ctx context.Context, trainerID, requestID string) (*Reservation, error)
}

// Service implements the reservation API rules. The Lambda handler stays a thin adapter on top.
type Service struct {
	catalog  Catalog
	queue    Queue
	outcomes Outcomes
}

// NewService wires a Service to its dependencies.
func NewService(catalog Catalog, queue Queue, outcomes Outcomes) *Service {
	return &Service{catalog: catalog, queue: queue, outcomes: outcomes}
}

// Accepted is the body of a 202: the request was queued, not reserved.
type Accepted struct {
	RequestID string `json:"requestId"`
	Status    Status `json:"status"`
}

// Submit checks the card exists and queues the request on behalf of trainerID. It reports
// success only after the queue has accepted the message; stock is the worker's concern.
func (s *Service) Submit(ctx context.Context, trainerID string, req SubmitRequest) (Accepted, error) {
	if trainerID == "" {
		return Accepted{}, &Error{Code: CodeUnauthorized, Message: "missing trainer identity"}
	}

	exists, err := s.catalog.CardExists(ctx, req.CardID)
	if err != nil {
		return Accepted{}, &Error{Code: CodeInternal, Message: "internal error", Cause: err}
	}
	if !exists {
		return Accepted{}, &Error{Code: CodeCardNotFound, Message: "card not found"}
	}

	if err := s.queue.Enqueue(ctx, QueueMessage{
		RequestID: req.RequestID,
		TrainerID: trainerID,
		CardID:    req.CardID,
	}); err != nil {
		return Accepted{}, &Error{
			Code:    CodeQueueUnavailable,
			Message: "the request was not queued; retry with the same requestId",
			Cause:   err,
		}
	}
	return Accepted{RequestID: req.RequestID, Status: StatusQueued}, nil
}

// Status returns trainerID's recorded outcome for requestID. An unknown, unprocessed or
// malformed ID and another trainer's ID are all the same RESERVATION_NOT_FOUND.
func (s *Service) Status(ctx context.Context, trainerID, requestID string) (*Reservation, error) {
	if trainerID == "" {
		return nil, &Error{Code: CodeUnauthorized, Message: "missing trainer identity"}
	}
	notFound := &Error{Code: CodeReservationNotFound, Message: "reservation not found"}

	id, ok := NormalizeRequestID(requestID)
	if !ok {
		return nil, notFound
	}
	r, err := s.outcomes.Get(ctx, trainerID, id)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "internal error", Cause: err}
	}
	if r == nil {
		return nil, notFound
	}
	if !r.Status.IsTerminal() {
		// Only terminal outcomes are ever written; anything else is corrupt data.
		return nil, &Error{Code: CodeInternal, Message: "internal error", Cause: fmt.Errorf("stored reservation has non-terminal status %q", r.Status)}
	}
	return r, nil
}

// Cards returns the catalog ordered by card ID. The slice is never nil so it serializes as [].
func (s *Service) Cards(ctx context.Context) ([]Card, error) {
	cards, err := s.catalog.Cards(ctx)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "internal error", Cause: err}
	}
	out := make([]Card, len(cards))
	copy(out, cards)
	sort.Slice(out, func(i, j int) bool { return out[i].CardID < out[j].CardID })
	return out, nil
}
