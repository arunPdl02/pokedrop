// Package reservations holds the reservation rules: statuses, validation and outcome
// logic. Lambda handlers in cmd/ stay thin and delegate here.
package reservations

import "time"

// Status is the outcome of a processed reservation. The API never returns QUEUED from the
// reservations resource; "queued" is only the response to POST /reservations (see docs/api.md).
type Status string

const (
	// StatusQueued is the response to POST /reservations only: the request is on the queue, not
	// reserved.
	StatusQueued Status = "QUEUED"
	// StatusConfirmed means the worker recorded a successful reservation (terminal).
	StatusConfirmed Status = "CONFIRMED"
	// StatusSoldOut means the worker found insufficient stock (terminal, a normal outcome).
	StatusSoldOut Status = "SOLD_OUT"
)

// IsTerminal reports whether s is a recorded outcome the worker may persist.
func (s Status) IsTerminal() bool {
	return s == StatusConfirmed || s == StatusSoldOut
}

// Card is a catalog entry as exposed by GET /cards. Available is derived from stock and may be
// stale by the time a reservation is processed.
type Card struct {
	CardID    string `json:"cardId"`
	Name      string `json:"name"`
	Rarity    string `json:"rarity"`
	Available int    `json:"available"`
}

// Reservation is a recorded outcome, keyed by trainer and request ID. TrainerID is the
// authenticated token's sub claim and is never serialized to clients.
type Reservation struct {
	RequestID string    `json:"requestId"`
	TrainerID string    `json:"-"`
	CardID    string    `json:"cardId"`
	Status    Status    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
}
