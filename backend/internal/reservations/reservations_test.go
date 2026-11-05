package reservations

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStatusValuesMatchAPIContract(t *testing.T) {
	if StatusConfirmed != "CONFIRMED" || StatusSoldOut != "SOLD_OUT" {
		t.Fatalf("status values drifted from docs/api.md: %q, %q", StatusConfirmed, StatusSoldOut)
	}
}

func TestStatusIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		StatusConfirmed: true,
		StatusSoldOut:   true,
		"QUEUED":        false,
		"":              false,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("Status(%q).IsTerminal() = %v, want %v", s, got, want)
		}
	}
}

func TestCardJSONShape(t *testing.T) {
	b, err := json.Marshal(Card{CardID: "card-001", Name: "Ember Drake", Rarity: "rare", Available: 12})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"cardId":"card-001","name":"Ember Drake","rarity":"rare","available":12}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestReservationJSONHidesTrainerID(t *testing.T) {
	r := Reservation{
		RequestID: "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b",
		TrainerID: "trainer-sub",
		CardID:    "card-001",
		Status:    StatusConfirmed,
		UpdatedAt: time.Date(2026, 11, 5, 20, 15, 30, 0, time.UTC),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"requestId":"3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b","cardId":"card-001","status":"CONFIRMED","updatedAt":"2026-11-05T20:15:30Z"}`
	if string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}
