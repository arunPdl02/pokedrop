package main

import (
	"strings"
	"testing"
	"time"
)

func sampleReport() report {
	submits, rows := run(200, 100)
	var r report
	r.Run, r.Stage, r.Commit = "full", "test", "abc1234"
	r.Workload.CardID, r.Workload.Stock, r.Workload.Requests, r.Workload.Concurrency = "card-001", 100, 200, 4
	r.Config = deployedConfig{
		Region: "us-west-2", AccountLimit: 10, WorkerBatchSize: 10, WorkerMaxConcurrency: 5,
		API:                  lambdaConfig{MemoryMB: 256, TimeoutSec: 10, Architecture: "arm64", Runtime: "provided.al2023"},
		Worker:               lambdaConfig{MemoryMB: 256, TimeoutSec: 10, Architecture: "arm64", Runtime: "provided.al2023"},
		QueueVisibilitySec:   60,
		QueueMaxReceiveCount: 5,
	}
	r.Evaluation = evaluate(evalInput{Stock: 100, CardID: "card-001", Submits: submits, Rows: rows, FinalStock: 0})
	r.Timing = measure(submits, rows)
	return r
}

func TestMarkdownStatesEverythingTheBenchmarkNeeds(t *testing.T) {
	md := sampleReport().markdown()
	for _, want := range []string{
		"`pokedrop-test`", "`card-001`", "stock 100", "200 unique request IDs", "4 virtual users", "us-west-2", "`abc1234`",
		"100 / 100", "Submit latency", "p50", "p95", "Time to final outcome", "Submit throughput", "End-to-end throughput",
		"Queue drain time", "throttles", "Account Lambda concurrency limit | 10", "256 MB", "maxConcurrency 5", "visibility timeout 60 s",
		"dead letter queue is empty | PASS", "every request was accepted (a clean workload) | PASS",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("the report is missing %q", want)
		}
	}
	if strings.Contains(md, "| FAIL |") {
		t.Errorf("a passing run must not print FAIL:\n%s", md)
	}
}

func TestMarkdownFlagsFailuresDirtyTreesAndNotes(t *testing.T) {
	r := sampleReport()
	r.Dirty = true
	r.Evaluation.Invariants[0].Pass = false
	r.NotesAdd("a note")
	md := r.markdown()
	for _, want := range []string{"uncommitted changes", "dead letter queue is empty | FAIL", "> a note"} {
		if !strings.Contains(md, want) {
			t.Errorf("the report is missing %q", want)
		}
	}
}

func (r *report) NotesAdd(s string) { r.Notes = append(r.Notes, s) }

func TestNotAcceptedSummary(t *testing.T) {
	var r report
	r.Evaluation.NotAccepted = map[int]int{}
	if got := r.notAcceptedSummary(); got != "none" {
		t.Errorf("got %q", got)
	}
	r.Evaluation.NotAccepted = map[int]int{503: 1, 429: 2, 0: 3}
	if got, want := r.notAcceptedSummary(), "no response ×3, 429 ×2, 503 ×1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSkewFrom(t *testing.T) {
	date := time.Date(2026, 11, 9, 20, 0, 5, 0, time.UTC) // the server said :05, so its time was :05.0-:05.999
	cases := map[string]struct {
		sent, received time.Time
		want           time.Duration
	}{
		"in step: the request straddles the middle of the second": {date.Add(400 * time.Millisecond), date.Add(600 * time.Millisecond), 0},
		"local clock a second ahead":                              {date.Add(1400 * time.Millisecond), date.Add(1600 * time.Millisecond), time.Second},
		"local clock two seconds behind":                          {date.Add(-1600 * time.Millisecond), date.Add(-1400 * time.Millisecond), -2 * time.Second},
	}
	for name, tc := range cases {
		if got := skewFrom(tc.sent, tc.received, date); got != tc.want {
			t.Errorf("%s: skew = %v, want %v", name, got, tc.want)
		}
	}
}

func TestDescribeSkew(t *testing.T) {
	if got := describeSkew(-2024 * time.Millisecond); got != "2024 ms behind" {
		t.Errorf("got %q", got)
	}
	if got := describeSkew(202 * time.Millisecond); got != "202 ms ahead of" {
		t.Errorf("got %q", got)
	}
}

func TestHasAllAccepted(t *testing.T) {
	submits, rows := run(3, 3)
	if !hasAllAccepted(submits, rows) {
		t.Error("every accepted request has a row")
	}
	if hasAllAccepted(submits, rows[:2]) {
		t.Error("a missing outcome must keep the wait going")
	}
	submits[2].Status = 429
	if !hasAllAccepted(submits, rows[:2]) {
		t.Error("a request that was not accepted needs no outcome")
	}
}

func TestRunNameAndStageChecks(t *testing.T) {
	for _, bad := range []string{"", "Small", "../etc", "a b", strings.Repeat("a", 40)} {
		if err := (&common{stage: "test", run: bad}).check(); err == nil {
			t.Errorf("run %q should be rejected", bad)
		}
	}
	if err := (&common{stage: "test", run: "small-1"}).check(); err != nil {
		t.Errorf("a plain run name was rejected: %v", err)
	}
	if err := (&common{stage: "prod", run: "small"}).check(); err == nil {
		t.Error("prod must be refused")
	}
}
