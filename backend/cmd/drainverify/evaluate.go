package main

import (
	"fmt"
	"sort"
	"time"
)

const (
	statusConfirmed = "CONFIRMED"
	statusSoldOut   = "SOLD_OUT"
)

// row is one stored reservation outcome.
type row struct {
	TrainerID string
	RequestID string
	CardID    string
	Status    string
	UpdatedAt time.Time
}

// check is one pass/fail statement in the report.
type check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// evalInput is everything the invariant checks need. It is plain data so the checks can be tested
// without AWS.
type evalInput struct {
	Stock      int      // stock the card was seeded with before the run
	CardID     string   // the card every request asked for
	Submits    []submit // what k6 sent
	Rows       []row    // every row in the reservations table
	FinalStock int      // the card's stock after the queue drained
	DLQDepth   int
}

type evaluation struct {
	// Accepted counts requests the API answered with 202.
	Accepted int `json:"accepted"`
	// NotAccepted counts the other requests by HTTP status (0 = no response).
	NotAccepted map[int]int `json:"notAccepted"`
	Confirmed   int         `json:"confirmed"`
	SoldOut     int         `json:"soldOut"`
	// RunRows counts stored outcomes for requests of this run, including requests the API did not
	// answer with 202 but that were queued anyway.
	RunRows int `json:"runRows"`
	// RowsForNotAccepted is the part of RunRows whose request did not get a 202.
	RowsForNotAccepted int `json:"rowsForNotAccepted"`
	// Invariants are the correctness checks. Pass is true only if all of them hold.
	Invariants []check `json:"invariants"`
	Pass       bool    `json:"pass"`
	// Clean is true when every request was accepted, which an acceptance run needs: a throttled
	// request is a different workload from the one planned.
	Clean bool `json:"clean"`
}

// evaluate checks the run against the invariants in CLAUDE.md: stock never negative, a request ID
// never consumes inventory twice, and the outcome counts follow from the stock.
func evaluate(in evalInput) evaluation {
	ev := evaluation{NotAccepted: map[int]int{}}

	submitted := make(map[string]submit, len(in.Submits))
	for _, s := range in.Submits {
		submitted[s.RequestID] = s
		if s.accepted() {
			ev.Accepted++
		} else {
			ev.NotAccepted[s.Status]++
		}
	}
	ev.Clean = ev.Accepted == len(in.Submits)

	rowsByID := map[string][]row{}
	for _, r := range in.Rows {
		rowsByID[r.RequestID] = append(rowsByID[r.RequestID], r)
	}

	var multi, missing, wrong []string
	for id, rs := range rowsByID {
		if len(rs) > 1 {
			multi = append(multi, id)
		}
		s, ours := submitted[id]
		if !ours {
			continue
		}
		ev.RunRows++
		if !s.accepted() {
			ev.RowsForNotAccepted++
		}
		for _, r := range rs {
			switch r.Status {
			case statusConfirmed:
				ev.Confirmed++
			case statusSoldOut:
				ev.SoldOut++
			default:
				wrong = append(wrong, fmt.Sprintf("%s has status %q", id, r.Status))
			}
			if r.CardID != in.CardID {
				wrong = append(wrong, fmt.Sprintf("%s is for card %q, want %q", id, r.CardID, in.CardID))
			}
		}
	}
	for _, s := range in.Submits {
		if s.accepted() && len(rowsByID[s.RequestID]) == 0 {
			missing = append(missing, s.RequestID)
		}
	}
	sort.Strings(multi)
	sort.Strings(missing)
	sort.Strings(wrong)

	add := func(name string, pass bool, detail string) {
		ev.Invariants = append(ev.Invariants, check{Name: name, Pass: pass, Detail: detail})
	}
	add("dead letter queue is empty", in.DLQDepth == 0, fmt.Sprintf("%d message(s)", in.DLQDepth))
	add("every accepted request has an outcome", len(missing) == 0, listDetail(missing, "no missing outcomes"))
	add("no request ID has two rows", len(multi) == 0, listDetail(multi, fmt.Sprintf("%d request IDs in the table, each with one row", len(rowsByID))))
	add("outcomes are valid and for the requested card", len(wrong) == 0, listDetail(wrong, "all CONFIRMED or SOLD_OUT"))
	wantConfirmed := min(in.Stock, ev.RunRows)
	add("CONFIRMED equals min(stock, outcomes)", ev.Confirmed == wantConfirmed,
		fmt.Sprintf("%d CONFIRMED, want min(%d, %d) = %d", ev.Confirmed, in.Stock, ev.RunRows, wantConfirmed))
	add("SOLD_OUT is every other outcome", ev.SoldOut == ev.RunRows-ev.Confirmed,
		fmt.Sprintf("%d SOLD_OUT, want %d - %d = %d", ev.SoldOut, ev.RunRows, ev.Confirmed, ev.RunRows-ev.Confirmed))
	add("final stock equals stock minus CONFIRMED", in.FinalStock == in.Stock-ev.Confirmed,
		fmt.Sprintf("final %d, want %d - %d = %d (a mismatch can also mean the card was not reseeded before the run)", in.FinalStock, in.Stock, ev.Confirmed, in.Stock-ev.Confirmed))
	add("stock is never negative", in.FinalStock >= 0, fmt.Sprintf("final stock %d", in.FinalStock))

	ev.Pass = true
	for _, c := range ev.Invariants {
		ev.Pass = ev.Pass && c.Pass
	}
	return ev
}

func listDetail(items []string, ok string) string {
	if len(items) == 0 {
		return ok
	}
	const show = 5
	if len(items) > show {
		return fmt.Sprintf("%d problems, first %d: %v", len(items), show, items[:show])
	}
	return fmt.Sprintf("%v", items)
}

// timing holds the numbers that come from comparing submit times with stored outcomes.
type timing struct {
	TimeToFinal stats `json:"timeToFinalOutcome"`
	Submit      stats `json:"submitLatency"`
	// SubmitPerSec is accepted requests divided by the time from the first send to the last response.
	SubmitPerSec float64 `json:"submitPerSec"`
	// EndToEndPerSec is stored outcomes divided by the time from the first send to the last outcome.
	EndToEndPerSec float64 `json:"endToEndPerSec"`
	// DrainSec is the time from the last response to the last stored outcome (0 if the worker kept up).
	DrainSec float64 `json:"drainSec"`
	// NegativeTimeToFinal counts outcomes stored "before" they were submitted, which only happens
	// through clock skew or the one-second resolution of updatedAt.
	NegativeTimeToFinal int `json:"negativeTimeToFinal"`
}

// measure computes latency, throughput and drain time. updatedAt has one-second resolution
// (the worker truncates it), so time to final outcome is only good to about a second.
func measure(submits []submit, rows []row) timing {
	byID := map[string]row{}
	for _, r := range rows {
		byID[r.RequestID] = r
	}

	var t timing
	var submitMs, finalMs []float64
	var firstSubmit, lastResponse int64
	var lastOutcome time.Time
	outcomes := 0
	for i, s := range submits {
		if i == 0 || s.SubmittedAtMs < firstSubmit {
			firstSubmit = s.SubmittedAtMs
		}
		lastResponse = max(lastResponse, s.RespondedAtMs)
		if !s.accepted() {
			continue
		}
		submitMs = append(submitMs, s.DurationMs)
		r, ok := byID[s.RequestID]
		if !ok {
			continue
		}
		outcomes++
		d := float64(r.UpdatedAt.UnixMilli() - s.SubmittedAtMs)
		if d < 0 {
			t.NegativeTimeToFinal++
		}
		finalMs = append(finalMs, d)
		if r.UpdatedAt.After(lastOutcome) {
			lastOutcome = r.UpdatedAt
		}
	}
	t.Submit = summarize(submitMs)
	t.TimeToFinal = summarize(finalMs)

	burstSec := max(float64(lastResponse-firstSubmit)/1000, 0.001)
	t.SubmitPerSec = float64(len(submitMs)) / burstSec
	if outcomes > 0 {
		// The denominator is at least one second: updatedAt cannot resolve less.
		total := max(float64(lastOutcome.UnixMilli()-firstSubmit)/1000, 1)
		t.EndToEndPerSec = float64(outcomes) / total
		t.DrainSec = max(float64(lastOutcome.UnixMilli()-lastResponse)/1000, 0)
	}
	return t
}
