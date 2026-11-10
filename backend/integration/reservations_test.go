//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Acceptance row 1: stock is never oversold. 20 unique requests compete for 10 cards.
func TestOversell(t *testing.T) {
	h.requireFailMode(t, "")
	ctx := testContext(t, 4*time.Minute)
	const card, stock, requests = "card-001", 10, 20
	h.setStock(t, ctx, card, stock)

	ids := make([]string, requests)
	for i := range ids {
		ids[i] = newRequestID()
	}
	h.forEach(requests, func(i int) {
		if err := h.submit(ctx, h.tokenA, ids[i], card); err != nil {
			t.Error(err)
		}
	})
	if t.Failed() {
		t.FailNow()
	}

	outcomes := make([]reservation, requests)
	h.forEach(requests, func(i int) {
		r, err := h.awaitOutcome(ctx, h.tokenA, ids[i])
		if err != nil {
			t.Error(err)
			return
		}
		outcomes[i] = r
	})
	if t.Failed() {
		t.FailNow()
	}

	counts := map[string]int{}
	for _, o := range outcomes {
		counts[o.Status]++
	}
	if counts["CONFIRMED"] != stock || counts["SOLD_OUT"] != requests-stock {
		t.Fatalf("outcomes = %v, want %d CONFIRMED and %d SOLD_OUT", counts, stock, requests-stock)
	}

	h.awaitIdle(t, ctx)
	if got := h.stock(t, ctx, card); got != 0 {
		t.Fatalf("stock = %d, want 0 (never negative, never left over)", got)
	}
	rows := h.rowsByRequestID(t, ctx)
	for _, id := range ids {
		if len(rows[id]) != 1 {
			t.Errorf("request %s has %d rows, want 1", id, len(rows[id]))
		}
	}
}

// Acceptance row 2: repeated copies of a request, concurrent or later, leave one reservation and
// one decrement.
func TestDuplicates(t *testing.T) {
	h.requireFailMode(t, "")
	ctx := testContext(t, 4*time.Minute)
	const card, stock, copies = "card-002", 5, 5
	h.setStock(t, ctx, card, stock)
	id := newRequestID()

	h.forEach(copies, func(int) {
		if err := h.submit(ctx, h.tokenA, id, card); err != nil {
			t.Error(err)
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	first, err := h.awaitOutcome(ctx, h.tokenA, id)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "CONFIRMED" {
		t.Fatalf("status = %s, want CONFIRMED (stock was %d)", first.Status, stock)
	}
	h.awaitIdle(t, ctx) // every copy has now been processed

	assertSingleDecrement := func(label string) {
		t.Helper()
		if got := h.stock(t, ctx, card); got != stock-1 {
			t.Fatalf("%s: stock = %d, want %d (one decrement for %d copies)", label, got, stock-1, copies)
		}
		if rows := h.rowsByRequestID(t, ctx)[id]; len(rows) != 1 {
			t.Fatalf("%s: %d rows for the request, want 1: %+v", label, len(rows), rows)
		}
	}
	assertSingleDecrement("after concurrent copies")

	// A copy arriving long after the outcome was stored changes nothing.
	if err := h.submit(ctx, h.tokenA, id, card); err != nil {
		t.Fatal(err)
	}
	h.awaitIdle(t, ctx)
	assertSingleDecrement("after a later copy")

	again, found, err := h.getReservation(ctx, h.tokenA, id)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if again.Status != first.Status || again.UpdatedAt != first.UpdatedAt {
		t.Fatalf("the outcome changed: first %+v, now %+v", first, again)
	}
}

// Part of row 2: restocking must not turn a stored SOLD_OUT into a reservation when the same
// request is sent again.
func TestDuplicateOfSoldOutStaysSoldOut(t *testing.T) {
	h.requireFailMode(t, "")
	ctx := testContext(t, 3*time.Minute)
	const card = "card-003"
	h.setStock(t, ctx, card, 0)
	id := newRequestID()

	if err := h.submit(ctx, h.tokenA, id, card); err != nil {
		t.Fatal(err)
	}
	first, err := h.awaitOutcome(ctx, h.tokenA, id)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "SOLD_OUT" {
		t.Fatalf("status = %s, want SOLD_OUT", first.Status)
	}
	h.awaitIdle(t, ctx)

	h.setStock(t, ctx, card, 3)
	if err := h.submit(ctx, h.tokenA, id, card); err != nil {
		t.Fatal(err)
	}
	h.awaitIdle(t, ctx)

	again, _, err := h.getReservation(ctx, h.tokenA, id)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "SOLD_OUT" || again.UpdatedAt != first.UpdatedAt {
		t.Fatalf("a stored SOLD_OUT changed: first %+v, now %+v", first, again)
	}
	if got := h.stock(t, ctx, card); got != 3 {
		t.Fatalf("stock = %d, want 3 (the resent request must not take stock)", got)
	}
}

// Acceptance row 3: the worker fails after the transaction commits and before it reports success.
// SQS redelivers the message; the retry must find the stored outcome and change nothing.
// Needs `make fail-on STAGE=test MODE=once`.
func TestFailureAfterCommit(t *testing.T) {
	h.requireFailMode(t, "once")
	ctx := testContext(t, 6*time.Minute)
	const card, stock = "card-003", 5
	h.setStock(t, ctx, card, stock)
	id := newRequestID()

	if err := h.submit(ctx, h.tokenA, id, card); err != nil {
		t.Fatal(err)
	}
	// The outcome is committed by attempt 1, which then fails, so a trainer already sees it.
	first, err := h.awaitOutcome(ctx, h.tokenA, id)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "CONFIRMED" {
		t.Fatalf("status = %s, want CONFIRMED", first.Status)
	}
	if got := h.stock(t, ctx, card); got != stock-1 {
		t.Fatalf("stock after attempt 1 = %d, want %d", got, stock-1)
	}

	// Wait for the redelivery (the visibility timeout is 60 s) to be logged as a duplicate. The log
	// evidence is what proves the retry happened and the injected failure was real: without it, an
	// unset flag would let this test pass without testing anything.
	var failed, retried map[string]any
	for failed == nil || retried == nil {
		lines, err := h.workerLines(ctx, id)
		if err != nil {
			t.Fatalf("read worker logs: %v", err)
		}
		for _, l := range lines {
			attempt, _ := l["attempt"].(float64)
			dup, _ := l["duplicate"].(bool)
			switch {
			case attempt == 1 && l["injectedFailure"] == "once" && l["outcome"] == "CONFIRMED" && !dup:
				failed = l
			case attempt >= 2 && dup && l["outcome"] == "CONFIRMED" && l["injectedFailure"] == nil:
				retried = l
			}
		}
		if failed != nil && retried != nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker logs before the deadline: attempt 1 with the injected failure = %v, the duplicate retry = %v", failed != nil, retried != nil)
		case <-time.After(5 * time.Second):
		}
	}

	h.awaitIdle(t, ctx)
	if got := h.stock(t, ctx, card); got != stock-1 {
		t.Fatalf("stock after the retry = %d, want %d: the retry took stock again", got, stock-1)
	}
	if rows := h.rowsByRequestID(t, ctx)[id]; len(rows) != 1 {
		t.Fatalf("%d rows for the request, want 1: %+v", len(rows), rows)
	}
	after, found, err := h.getReservation(ctx, h.tokenA, id)
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if after.Status != first.Status || after.UpdatedAt != first.UpdatedAt {
		t.Fatalf("the retry changed the outcome: first %+v, now %+v", first, after)
	}
}

// Acceptance row 5: another trainer's reservation is concealed, indistinguishable from an unknown
// one. Also checks the authorizer's 401 for missing or garbage tokens.
func TestCrossTrainerRead(t *testing.T) {
	h.requireFailMode(t, "")
	ctx := testContext(t, 3*time.Minute)
	const card = "card-002"
	h.setStock(t, ctx, card, 3)
	id := newRequestID()

	if err := h.submit(ctx, h.tokenA, id, card); err != nil {
		t.Fatal(err)
	}
	mine, err := h.awaitOutcome(ctx, h.tokenA, id)
	if err != nil {
		t.Fatal(err)
	}
	if mine.Status != "CONFIRMED" || mine.RequestID != id || mine.CardID != card {
		t.Fatalf("owner read = %+v", mine)
	}

	theirs, err := h.do(ctx, http.MethodGet, "/reservations/"+id, h.tokenB, nil)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := h.do(ctx, http.MethodGet, "/reservations/"+newRequestID(), h.tokenB, nil)
	if err != nil {
		t.Fatal(err)
	}
	if theirs.Status != http.StatusNotFound || !strings.Contains(string(theirs.Body), "RESERVATION_NOT_FOUND") {
		t.Fatalf("another trainer got %d %s, want 404 RESERVATION_NOT_FOUND", theirs.Status, theirs.Body)
	}
	if unknown.Status != theirs.Status || !bytes.Equal(unknown.Body, theirs.Body) {
		t.Fatalf("a foreign reservation is distinguishable from an unknown one:\n foreign: %d %s\n unknown: %d %s",
			theirs.Status, theirs.Body, unknown.Status, unknown.Body)
	}
	if strings.Contains(string(theirs.Body), mine.CardID) || strings.Contains(string(theirs.Body), "CONFIRMED") {
		t.Fatalf("the 404 leaks the reservation: %s", theirs.Body)
	}

	for name, token := range map[string]string{"no token": "", "garbage token": "not.a.jwt"} {
		resp, err := h.do(ctx, http.MethodGet, "/reservations/"+id, token, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Status != http.StatusUnauthorized {
			t.Errorf("%s: got %d %s, want 401", name, resp.Status, resp.Body)
		}
	}

	h.awaitIdle(t, ctx)
	if rows := h.rowsByRequestID(t, ctx)[id]; len(rows) != 1 {
		t.Fatalf("%d rows for the request, want 1", len(rows))
	}
}
