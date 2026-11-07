package reservations

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCatalog struct {
	cards     []Card
	exists    map[string]bool
	err       error
	existsErr error
}

func (f *fakeCatalog) Cards(context.Context) ([]Card, error) { return f.cards, f.err }
func (f *fakeCatalog) CardExists(_ context.Context, id string) (bool, error) {
	return f.exists[id], f.existsErr
}

type fakeQueue struct {
	sent []QueueMessage
	err  error
}

func (f *fakeQueue) Enqueue(_ context.Context, m QueueMessage) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

// fakeOutcomes is keyed by (trainer, request) exactly like the real table, so a lookup can only
// succeed with the owner's ID.
type fakeOutcomes struct {
	items map[[2]string]*Reservation
	err   error
	gets  [][2]string
}

func (f *fakeOutcomes) Get(_ context.Context, trainerID, requestID string) (*Reservation, error) {
	f.gets = append(f.gets, [2]string{trainerID, requestID})
	if f.err != nil {
		return nil, f.err
	}
	return f.items[[2]string{trainerID, requestID}], nil
}

func newService() (*Service, *fakeCatalog, *fakeQueue, *fakeOutcomes) {
	c := &fakeCatalog{exists: map[string]bool{"card-001": true}}
	q := &fakeQueue{}
	o := &fakeOutcomes{items: map[[2]string]*Reservation{}}
	return NewService(c, q, o), c, q, o
}

func codeOf(t *testing.T, err error) Code {
	t.Helper()
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want *Error", err)
	}
	return apiErr.Code
}

func TestSubmitQueuesOneMessageWithTheCallersIdentity(t *testing.T) {
	svc, _, q, _ := newService()
	got, err := svc.Submit(context.Background(), "sub-A", SubmitRequest{RequestID: validID, CardID: "card-001"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.RequestID != validID {
		t.Fatalf("got %+v", got)
	}
	want := QueueMessage{RequestID: validID, TrainerID: "sub-A", CardID: "card-001"}
	if len(q.sent) != 1 || q.sent[0] != want {
		t.Fatalf("sent %+v, want exactly [%+v]", q.sent, want)
	}
}

func TestSubmitUnknownCardQueuesNothing(t *testing.T) {
	svc, _, q, _ := newService()
	_, err := svc.Submit(context.Background(), "sub-A", SubmitRequest{RequestID: validID, CardID: "card-999"})
	if codeOf(t, err) != CodeCardNotFound {
		t.Fatalf("got %v", err)
	}
	if len(q.sent) != 0 {
		t.Fatalf("queued %+v for an unknown card", q.sent)
	}
}

// Invariant: 202 (a returned Accepted) only after the queue accepted the message.
func TestSubmitQueueFailureIsNeverAccepted(t *testing.T) {
	svc, _, q, _ := newService()
	q.err = errors.New("sqs down")
	got, err := svc.Submit(context.Background(), "sub-A", SubmitRequest{RequestID: validID, CardID: "card-001"})
	if codeOf(t, err) != CodeQueueUnavailable {
		t.Fatalf("got %v", err)
	}
	if got != (Accepted{}) {
		t.Fatalf("a failed enqueue must not return an accepted result: %+v", got)
	}
	if !errors.Is(err, q.err) {
		t.Error("the cause must be kept for logging")
	}
}

func TestSubmitCatalogFailureQueuesNothing(t *testing.T) {
	svc, c, q, _ := newService()
	c.existsErr = errors.New("dynamo down")
	_, err := svc.Submit(context.Background(), "sub-A", SubmitRequest{RequestID: validID, CardID: "card-001"})
	if codeOf(t, err) != CodeInternal || len(q.sent) != 0 {
		t.Fatalf("err=%v sent=%v", err, q.sent)
	}
}

func TestSubmitRequiresIdentity(t *testing.T) {
	svc, _, q, _ := newService()
	_, err := svc.Submit(context.Background(), "", SubmitRequest{RequestID: validID, CardID: "card-001"})
	if codeOf(t, err) != CodeUnauthorized || len(q.sent) != 0 {
		t.Fatalf("err=%v sent=%v", err, q.sent)
	}
}

func TestSubmitSameRequestIDTwiceIsSafeToRepeat(t *testing.T) {
	// The API may accept a resubmission; dedupe is the worker's job (Phase 3). Both messages must
	// carry the identical request ID so the worker can recognize them.
	svc, _, q, _ := newService()
	req := SubmitRequest{RequestID: validID, CardID: "card-001"}
	for i := 0; i < 2; i++ {
		if _, err := svc.Submit(context.Background(), "sub-A", req); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.sent) != 2 || q.sent[0] != q.sent[1] {
		t.Fatalf("sent %+v", q.sent)
	}
}

func confirmed(trainer string) *Reservation {
	return &Reservation{
		RequestID: validID, TrainerID: trainer, CardID: "card-001",
		Status: StatusConfirmed, UpdatedAt: time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC),
	}
}

func TestStatusReturnsTheOwnersReservation(t *testing.T) {
	svc, _, _, o := newService()
	o.items[[2]string{"sub-A", validID}] = confirmed("sub-A")
	got, err := svc.Status(context.Background(), "sub-A", validID)
	if err != nil || got.Status != StatusConfirmed {
		t.Fatalf("got %+v %v", got, err)
	}
}

// Invariant: trainers can only read their own reservations.
func TestStatusOfAnotherTrainersRequestIsNotFound(t *testing.T) {
	svc, _, _, o := newService()
	o.items[[2]string{"sub-A", validID}] = confirmed("sub-A")
	_, err := svc.Status(context.Background(), "sub-B", validID)
	if codeOf(t, err) != CodeReservationNotFound {
		t.Fatalf("got %v", err)
	}
	for _, g := range o.gets {
		if g[0] != "sub-B" {
			t.Fatalf("lookup used trainer %q, want the caller's sub-B", g[0])
		}
	}
}

func TestStatusUnknownAndMalformedIDsAreIndistinguishable(t *testing.T) {
	svc, _, _, o := newService()
	_, unknown := svc.Status(context.Background(), "sub-A", validID)
	_, malformed := svc.Status(context.Background(), "sub-A", "not-a-uuid")
	var a, b *Error
	if !errors.As(unknown, &a) || !errors.As(malformed, &b) || *a != *b {
		t.Fatalf("unknown=%v malformed=%v", unknown, malformed)
	}
	if len(o.gets) != 1 {
		t.Fatalf("a malformed ID must not reach storage, gets=%v", o.gets)
	}
}

func TestStatusNormalizesTheRequestIDBeforeLookup(t *testing.T) {
	svc, _, _, o := newService()
	o.items[[2]string{"sub-A", validID}] = confirmed("sub-A")
	if _, err := svc.Status(context.Background(), "sub-A", "3F2B8C1E-6D4A-4A57-9B0E-1C2D3E4F5A6B"); err != nil {
		t.Fatal(err)
	}
}

func TestStatusStorageProblems(t *testing.T) {
	t.Run("storage error", func(t *testing.T) {
		svc, _, _, o := newService()
		o.err = errors.New("dynamo down")
		_, err := svc.Status(context.Background(), "sub-A", validID)
		if codeOf(t, err) != CodeInternal {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("non-terminal stored status", func(t *testing.T) {
		svc, _, _, o := newService()
		bad := confirmed("sub-A")
		bad.Status = "PENDING"
		o.items[[2]string{"sub-A", validID}] = bad
		_, err := svc.Status(context.Background(), "sub-A", validID)
		if codeOf(t, err) != CodeInternal {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing identity", func(t *testing.T) {
		svc, _, _, o := newService()
		_, err := svc.Status(context.Background(), "", validID)
		if codeOf(t, err) != CodeUnauthorized || len(o.gets) != 0 {
			t.Fatalf("err=%v gets=%v", err, o.gets)
		}
	})
}

func TestCardsAreSortedAndNeverNil(t *testing.T) {
	svc, c, _, _ := newService()
	got, err := svc.Cards(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty catalog: got %#v %v", got, err)
	}

	c.cards = []Card{{CardID: "card-003"}, {CardID: "card-001"}, {CardID: "card-002"}}
	got, err = svc.Cards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"card-001", "card-002", "card-003"} {
		if got[i].CardID != id {
			t.Fatalf("order: got %+v", got)
		}
	}

	c.err = errors.New("dynamo down")
	if _, err := svc.Cards(context.Background()); codeOf(t, err) != CodeInternal {
		t.Fatalf("got %v", err)
	}
}
