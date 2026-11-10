package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// loadParams is written by `preflight` next to the run's submit log, so the report states the
// workload that was actually requested instead of trusting flags typed again later.
type loadParams struct {
	Run                  string    `json:"run"`
	Stage                string    `json:"stage"`
	CardID               string    `json:"cardId"`
	Requests             int       `json:"requests"`
	Concurrency          int       `json:"concurrency"`
	PreflightAt          time.Time `json:"preflightAt"`
	AccountLimit         int       `json:"accountConcurrencyLimit"`
	WorkerMaxConcurrency int       `json:"workerMaxConcurrency"`
	SafeConcurrency      int       `json:"safeConcurrency"`
	ClockSkewMs          int64     `json:"clockSkewMs"`
}

type lambdaConfig struct {
	MemoryMB        int    `json:"memoryMb"`
	TimeoutSec      int    `json:"timeoutSec"`
	Architecture    string `json:"architecture"`
	Runtime         string `json:"runtime"`
	FailAfterCommit string `json:"failAfterCommit,omitempty"`
}

type deployedConfig struct {
	Region               string       `json:"region"`
	AccountLimit         int          `json:"accountConcurrencyLimit"`
	API                  lambdaConfig `json:"api"`
	Worker               lambdaConfig `json:"worker"`
	WorkerBatchSize      int          `json:"workerBatchSize"`
	WorkerMaxConcurrency int          `json:"workerMaxConcurrency"`
	QueueVisibilitySec   int          `json:"queueVisibilityTimeoutSec"`
	QueueMaxReceiveCount int          `json:"queueMaxReceiveCount"`
}

// awsFacts are counters read from CloudWatch after the run.
type awsFacts struct {
	APIInvocations    int `json:"apiInvocations"`
	APIThrottles      int `json:"apiThrottles"`
	APIErrors         int `json:"apiErrors"`
	WorkerInvocations int `json:"workerInvocations"`
	WorkerThrottles   int `json:"workerThrottles"`
	WorkerErrors      int `json:"workerErrors"`
	// Worker log lines since the run started: RETRY outcomes (transaction conflicts and other
	// retryable errors), deliveries beyond the first, and permanent failures.
	WorkerRetryLines   int `json:"workerRetryLines"`
	WorkerRedeliveries int `json:"workerRedeliveries"`
	WorkerFailedLines  int `json:"workerFailedLines"`
	DLQDepth           int `json:"dlqDepth"`
	// MetricsComplete is false if CloudWatch had not caught up when the tool gave up waiting.
	MetricsComplete bool `json:"metricsComplete"`
}

type report struct {
	Run         string    `json:"run"`
	Stage       string    `json:"stage"`
	GeneratedAt time.Time `json:"generatedAt"`
	Commit      string    `json:"commit"`
	Dirty       bool      `json:"dirty"`
	Workload    struct {
		CardID      string `json:"cardId"`
		Stock       int    `json:"stock"`
		Requests    int    `json:"requests"`
		Concurrency int    `json:"concurrency"`
	} `json:"workload"`
	Config deployedConfig `json:"config"`
	Clock  struct {
		PreflightSkewMs int64 `json:"preflightSkewMs"`
		VerifySkewMs    int64 `json:"verifySkewMs"`
	} `json:"clock"`
	Evaluation evaluation `json:"evaluation"`
	Timing     timing     `json:"timing"`
	AWS        awsFacts   `json:"aws"`
	FinalStock int        `json:"finalStock"`
	// WaitedSec is how long verify waited for the queue to be empty and every outcome stored.
	WaitedSec float64  `json:"waitedSec"`
	Notes     []string `json:"notes,omitempty"`
}

func fmtMs(v float64) string {
	if v >= 100 || v <= -100 {
		return fmt.Sprintf("%.0f ms", v)
	}
	return fmt.Sprintf("%.1f ms", v)
}

func fmtSec(ms float64) string { return fmt.Sprintf("%.1f s", ms/1000) }

func verdict(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}

func (r report) notAcceptedSummary() string {
	if len(r.Evaluation.NotAccepted) == 0 {
		return "none"
	}
	var codes []int
	for c := range r.Evaluation.NotAccepted {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	var parts []string
	for _, c := range codes {
		label := fmt.Sprint(c)
		if c == 0 {
			label = "no response"
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", label, r.Evaluation.NotAccepted[c]))
	}
	return strings.Join(parts, ", ")
}

// markdown renders the report as the tables that go into docs/benchmark.md.
func (r report) markdown() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	dirty := ""
	if r.Dirty {
		dirty = " (uncommitted changes in backend, infra, Makefile or tests)"
	}

	w("## Run `%s` on `pokedrop-%s`", r.Run, r.Stage)
	w("")
	w("| Workload | |")
	w("|---|---|")
	w("| Card | `%s`, seeded with stock %d |", r.Workload.CardID, r.Workload.Stock)
	w("| Requests | %d unique request IDs, one POST each, no retries |", r.Workload.Requests)
	w("| Concurrency | %d virtual users, one request in flight each |", r.Workload.Concurrency)
	w("| Region | %s |", r.Config.Region)
	w("| Commit | `%s`%s |", r.Commit, dirty)
	w("")
	w("| Result | |")
	w("|---|---|")
	e := r.Evaluation
	w("| Accepted (202) | %d of %d |", e.Accepted, r.Workload.Requests)
	w("| Not accepted | %s |", r.notAcceptedSummary())
	w("| CONFIRMED / SOLD_OUT | %d / %d |", e.Confirmed, e.SoldOut)
	w("| Final stock | %d |", r.FinalStock)
	w("| Submit latency (n=%d) | p50 %s, p95 %s (min %s, max %s) |", r.Timing.Submit.N, fmtMs(r.Timing.Submit.P50Ms), fmtMs(r.Timing.Submit.P95Ms), fmtMs(r.Timing.Submit.MinMs), fmtMs(r.Timing.Submit.MaxMs))
	w("| Time to final outcome (n=%d) | p50 %s, p95 %s (min %s, max %s); good to about ±1 s |", r.Timing.TimeToFinal.N, fmtSec(r.Timing.TimeToFinal.P50Ms), fmtSec(r.Timing.TimeToFinal.P95Ms), fmtSec(r.Timing.TimeToFinal.MinMs), fmtSec(r.Timing.TimeToFinal.MaxMs))
	w("| Submit throughput | %.1f requests/s accepted |", r.Timing.SubmitPerSec)
	w("| End-to-end throughput | %.1f outcomes/s |", r.Timing.EndToEndPerSec)
	w("| Queue drain time | %.0f s from the last response to the last stored outcome |", r.Timing.DrainSec)
	w("| API Lambda | %d invocations, %d throttles, %d errors |", r.AWS.APIInvocations, r.AWS.APIThrottles, r.AWS.APIErrors)
	w("| Worker Lambda | %d invocations, %d throttles, %d errors |", r.AWS.WorkerInvocations, r.AWS.WorkerThrottles, r.AWS.WorkerErrors)
	w("| Worker log lines | %d RETRY, %d redeliveries (attempt > 1), %d FAILED |", r.AWS.WorkerRetryLines, r.AWS.WorkerRedeliveries, r.AWS.WorkerFailedLines)
	w("| Dead letter queue | %d |", r.AWS.DLQDepth)
	w("")
	w("| Deployed configuration | |")
	w("|---|---|")
	w("| Account Lambda concurrency limit | %d |", r.Config.AccountLimit)
	w("| API Lambda | %d MB, %d s timeout, %s, %s |", r.Config.API.MemoryMB, r.Config.API.TimeoutSec, r.Config.API.Architecture, r.Config.API.Runtime)
	w("| Worker Lambda | %d MB, %d s timeout, %s, %s |", r.Config.Worker.MemoryMB, r.Config.Worker.TimeoutSec, r.Config.Worker.Architecture, r.Config.Worker.Runtime)
	w("| Worker event source | batch size %d, maxConcurrency %d |", r.Config.WorkerBatchSize, r.Config.WorkerMaxConcurrency)
	w("| Queue | visibility timeout %d s, maxReceiveCount %d |", r.Config.QueueVisibilitySec, r.Config.QueueMaxReceiveCount)
	w("| Clock skew (local minus API) | %d ms before, %d ms after |", r.Clock.PreflightSkewMs, r.Clock.VerifySkewMs)
	w("")
	w("| Check | Result | Detail |")
	w("|---|---|---|")
	for _, c := range e.Invariants {
		w("| %s | %s | %s |", c.Name, verdict(c.Pass), c.Detail)
	}
	w("| every request was accepted (a clean workload) | %s | %d of %d |", verdict(e.Clean), e.Accepted, r.Workload.Requests)
	for _, n := range r.Notes {
		w("")
		w("> %s", n)
	}
	return b.String()
}
