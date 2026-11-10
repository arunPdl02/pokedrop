package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)

func id(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

// run builds a run of n requests that were all accepted, with confirmed outcomes for the first
// `confirmed` of them and SOLD_OUT for the rest.
func run(n, confirmed int) ([]submit, []row) {
	var submits []submit
	var rows []row
	for i := range n {
		submits = append(submits, submit{
			RequestID: id(i), CardID: "card-001", Status: 202,
			SubmittedAtMs: base.UnixMilli() + int64(i)*10, RespondedAtMs: base.UnixMilli() + int64(i)*10 + 100, DurationMs: 100,
		})
		status := statusSoldOut
		if i < confirmed {
			status = statusConfirmed
		}
		rows = append(rows, row{TrainerID: "t", RequestID: id(i), CardID: "card-001", Status: status, UpdatedAt: base.Add(2 * time.Second)})
	}
	return submits, rows
}

func failed(ev evaluation) []string {
	var names []string
	for _, c := range ev.Invariants {
		if !c.Pass {
			names = append(names, c.Name)
		}
	}
	return names
}

func TestEvaluateAcceptsTheAcceptanceRun(t *testing.T) {
	submits, rows := run(200, 100)
	ev := evaluate(evalInput{Stock: 100, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0})
	if !ev.Pass || !ev.Clean || ev.Confirmed != 100 || ev.SoldOut != 100 || ev.Accepted != 200 {
		t.Fatalf("got %+v (failed: %v)", ev, failed(ev))
	}
}

func TestEvaluateAcceptsAnUndersubscribedDrop(t *testing.T) {
	submits, rows := run(8, 8)
	ev := evaluate(evalInput{Stock: 10, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 2})
	if !ev.Pass || ev.Confirmed != 8 || ev.SoldOut != 0 {
		t.Fatalf("got %+v (failed: %v)", ev, failed(ev))
	}
}

func TestEvaluateCatchesEachWayTheInvariantsCanBreak(t *testing.T) {
	cases := map[string]struct {
		mutate func(*evalInput)
		want   string // the check that must fail
	}{
		"oversold: more CONFIRMED than stock": {func(in *evalInput) {
			in.Rows[100].Status = statusConfirmed // 101 CONFIRMED for a stock of 100
		}, "CONFIRMED equals min(stock, outcomes)"},
		"negative stock":                  {func(in *evalInput) { in.FinalStock = -1 }, "stock is never negative"},
		"a decrement was lost or doubled": {func(in *evalInput) { in.FinalStock = 1 }, "final stock equals stock minus CONFIRMED"},
		"the card was not reseeded":       {func(in *evalInput) { in.FinalStock = 40 }, "final stock equals stock minus CONFIRMED"},
		"a request has two rows": {func(in *evalInput) {
			dup := in.Rows[3]
			dup.TrainerID = "someone-else"
			in.Rows = append(in.Rows, dup)
		}, "no request ID has two rows"},
		"an accepted request has no outcome": {func(in *evalInput) { in.Rows = in.Rows[1:] }, "every accepted request has an outcome"},
		"a message is in the DLQ":            {func(in *evalInput) { in.DLQDepth = 1 }, "dead letter queue is empty"},
		"a row for the wrong card":           {func(in *evalInput) { in.Rows[5].CardID = "card-002" }, "outcomes are valid and for the requested card"},
		"an unknown status":                  {func(in *evalInput) { in.Rows[5].Status = "PENDING" }, "outcomes are valid and for the requested card"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			submits, rows := run(200, 100)
			in := evalInput{Stock: 100, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0}
			tc.mutate(&in)
			ev := evaluate(in)
			if ev.Pass {
				t.Fatal("the run passed although an invariant was broken")
			}
			found := false
			for _, name := range failed(ev) {
				found = found || name == tc.want
			}
			if !found {
				t.Fatalf("check %q did not fail; failed checks: %v", tc.want, failed(ev))
			}
		})
	}
}

func TestEvaluateReportsNotAcceptedRequestsWithoutFailingTheInvariants(t *testing.T) {
	submits, rows := run(10, 5)
	submits[8].Status, submits[9].Status = 429, 0
	rows = rows[:8] // the two requests that were not accepted left no outcome
	ev := evaluate(evalInput{Stock: 5, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0})
	if !ev.Pass {
		t.Fatalf("invariants failed: %v", failed(ev))
	}
	if ev.Clean || ev.Accepted != 8 || ev.NotAccepted[429] != 1 || ev.NotAccepted[0] != 1 {
		t.Fatalf("got %+v, want not clean with a 429 and a 0 counted", ev)
	}
}

func TestEvaluateCountsARequestQueuedDespiteAnErrorStatus(t *testing.T) {
	// The API answered 503 but the message had already reached the queue: it is stock-consuming
	// work like any other, so it counts toward the totals and is reported separately.
	submits, rows := run(10, 5)
	submits[9].Status = 503
	ev := evaluate(evalInput{Stock: 5, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0})
	if !ev.Pass || ev.RunRows != 10 || ev.RowsForNotAccepted != 1 || ev.Clean {
		t.Fatalf("got %+v (failed: %v)", ev, failed(ev))
	}
}

func TestEvaluateIgnoresOtherRunsRowsExceptForDoubleRows(t *testing.T) {
	submits, rows := run(4, 2)
	// Rows left over from an earlier run (different request IDs) do not count toward this run.
	rows = append(rows, row{TrainerID: "t", RequestID: id(900), CardID: "card-001", Status: statusConfirmed, UpdatedAt: base})
	ev := evaluate(evalInput{Stock: 2, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0})
	if !ev.Pass || ev.Confirmed != 2 || ev.RunRows != 4 {
		t.Fatalf("got %+v (failed: %v)", ev, failed(ev))
	}
	// But a double row anywhere in the table is still a failure.
	rows = append(rows, row{TrainerID: "u", RequestID: id(900), CardID: "card-001", Status: statusConfirmed, UpdatedAt: base})
	if ev := evaluate(evalInput{Stock: 2, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0}); ev.Pass {
		t.Fatal("a double row from an earlier run must still fail")
	}
}

func TestEvaluateLongProblemListsAreShortened(t *testing.T) {
	submits, _ := run(50, 0)
	ev := evaluate(evalInput{Stock: 0, CardID: "card-001", Submits: submits, Rows: nil, FinalStock: 0})
	for _, c := range ev.Invariants {
		if c.Name == "every accepted request has an outcome" && !strings.Contains(c.Detail, "50 problems") {
			t.Errorf("detail = %q", c.Detail)
		}
	}
}

func TestMeasure(t *testing.T) {
	submits := []submit{
		{RequestID: "a", Status: 202, SubmittedAtMs: 1_000_000, RespondedAtMs: 1_000_100, DurationMs: 100},
		{RequestID: "b", Status: 202, SubmittedAtMs: 1_000_500, RespondedAtMs: 1_000_900, DurationMs: 400},
		{RequestID: "c", Status: 429, SubmittedAtMs: 1_000_600, RespondedAtMs: 1_001_000, DurationMs: 400},
	}
	rows := []row{
		{RequestID: "a", UpdatedAt: time.UnixMilli(1_003_000)}, // 3.0 s after its submit
		{RequestID: "b", UpdatedAt: time.UnixMilli(1_005_000)}, // 4.5 s after its submit
	}
	got := measure(submits, rows)

	if got.Submit.N != 2 || got.Submit.P50Ms != 100 || got.Submit.MaxMs != 400 {
		t.Errorf("submit latency counts only accepted requests: %+v", got.Submit)
	}
	if got.TimeToFinal.N != 2 || got.TimeToFinal.MinMs != 3000 || got.TimeToFinal.MaxMs != 4500 {
		t.Errorf("time to final outcome = %+v", got.TimeToFinal)
	}
	// 2 accepted over the 1.0 s from the first send (1_000_000) to the last response (1_001_000).
	if got.SubmitPerSec != 2 {
		t.Errorf("submit throughput = %v, want 2", got.SubmitPerSec)
	}
	// 2 outcomes over the 5.0 s from the first send to the last stored outcome.
	if got.EndToEndPerSec != 0.4 {
		t.Errorf("end-to-end throughput = %v, want 0.4", got.EndToEndPerSec)
	}
	// Last response at 1_001_000, last outcome at 1_005_000.
	if got.DrainSec != 4 {
		t.Errorf("drain = %v, want 4", got.DrainSec)
	}
}

func TestMeasureNeverReportsNegativeDrainAndCountsClockSkew(t *testing.T) {
	submits := []submit{{RequestID: "a", Status: 202, SubmittedAtMs: 5_000, RespondedAtMs: 6_000, DurationMs: 1000}}
	rows := []row{{RequestID: "a", UpdatedAt: time.UnixMilli(4_000)}} // stored "before" it was sent
	got := measure(submits, rows)
	if got.DrainSec != 0 {
		t.Errorf("drain = %v, want 0", got.DrainSec)
	}
	if got.NegativeTimeToFinal != 1 || got.TimeToFinal.MinMs != -1000 {
		t.Errorf("skew must be visible, not hidden: %+v", got)
	}
}

func TestMeasureWithNoOutcomesDoesNotDivideByZero(t *testing.T) {
	got := measure([]submit{{RequestID: "a", Status: 202, SubmittedAtMs: 1000, RespondedAtMs: 1000, DurationMs: 0}}, nil)
	if got.EndToEndPerSec != 0 || got.DrainSec != 0 || got.SubmitPerSec <= 0 {
		t.Errorf("got %+v", got)
	}
}
