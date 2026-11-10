package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/arunPdl02/pokedrop/backend/internal/stackinfo"
)

const (
	// idleReads empty readings in a row count as an empty queue: SQS counts are approximate.
	idleReads = 3
	idleEvery = 2 * time.Second

	metricsPoll = 15 * time.Second
)

// verify waits for a run to drain, checks the invariants and writes the report.
func verify(args []string) error {
	var c common
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	c.register(fs)
	stock := fs.Int("stock", -1, "stock the card was seeded with before the run (required)")
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the queue to drain and every outcome to be stored")
	metricsWait := fs.Duration("metrics-wait", 4*time.Minute, "how long to wait for CloudWatch to report every API invocation")
	repo := fs.String("repo", "..", "repository root, for the commit hash")
	_ = fs.Parse(args)
	if err := c.check(); err != nil {
		return err
	}
	if *stock < 0 {
		return errors.New("-stock is required: the stock the card was seeded with (make seed STOCK=...)")
	}

	var load loadParams
	b, err := os.ReadFile(filepath.Join(c.dir(), loadFile))
	if err != nil {
		return fmt.Errorf("read %s (run `make load` first, it writes it): %w", loadFile, err)
	}
	if err := json.Unmarshal(b, &load); err != nil {
		return fmt.Errorf("%s: %w", loadFile, err)
	}
	f, err := os.Open(filepath.Join(c.dir(), submitsFile))
	if err != nil {
		return fmt.Errorf("open the submit log (did k6 run?): %w", err)
	}
	submits, err := parseSubmits(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", submitsFile, err)
	}
	if len(submits) != load.Requests {
		return fmt.Errorf("the submit log has %d requests but the run planned %d: k6 did not finish", len(submits), load.Requests)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+*metricsWait+5*time.Minute)
	defer cancel()
	cl, err := newClients(ctx)
	if err != nil {
		return err
	}
	st, err := cl.loadStack(ctx, c.stage)
	if err != nil {
		return err
	}

	var r report
	r.Run, r.Stage, r.GeneratedAt = c.run, c.stage, time.Now().UTC()
	r.Workload.CardID, r.Workload.Stock, r.Workload.Requests, r.Workload.Concurrency = load.CardID, *stock, load.Requests, load.Concurrency
	r.Clock.PreflightSkewMs = load.ClockSkewMs

	// 1. Wait for the queue to empty and every accepted request to have an outcome.
	waited, rows, drained, err := cl.waitForDrain(ctx, st, submits, *timeout)
	if err != nil {
		return err
	}
	r.WaitedSec = waited.Seconds()
	if !drained {
		r.Notes = append(r.Notes, fmt.Sprintf("Timed out after %s: the queue was not empty or an accepted request had no outcome, so the checks below ran on partial data.", *timeout))
	}

	// 2. Read the final state.
	finalStock, err := cl.stock(ctx, st.tables.Inventory, load.CardID)
	if err != nil {
		return err
	}
	dlq, err := cl.queueTotal(ctx, st.dlqURL)
	if err != nil {
		return err
	}
	r.FinalStock = finalStock
	r.AWS.DLQDepth = dlq
	r.Evaluation = evaluate(evalInput{Stock: *stock, CardID: load.CardID, Submits: submits, Rows: rows, FinalStock: finalStock, DLQDepth: dlq})
	r.Timing = measure(submits, rows)
	if r.Timing.NegativeTimeToFinal > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d outcome(s) were stored before their request was sent: that is clock skew or the one-second resolution of updatedAt, so treat time to final outcome as approximate.", r.Timing.NegativeTimeToFinal))
	}

	// 3. Clock skew now, deployed configuration, commit.
	if skew, err := clockSkew(ctx, st.apiURL); err != nil {
		r.Notes = append(r.Notes, "Could not re-measure clock skew after the run: "+err.Error())
	} else {
		r.Clock.VerifySkewMs = skew.Milliseconds()
		if skew > maxSkew || skew < -maxSkew {
			r.Notes = append(r.Notes, fmt.Sprintf("The local clock was %d ms off the API's after the run: time to final outcome is unreliable.", skew.Milliseconds()))
		}
	}
	if r.Config, err = cl.deployedConfig(ctx, st); err != nil {
		return err
	}
	if r.Commit, r.Dirty, err = gitState(*repo); err != nil {
		return err
	}
	if r.Dirty {
		r.Notes = append(r.Notes, "The working tree had uncommitted changes in backend, infra, Makefile or tests: the commit hash does not fully describe what was measured.")
	}
	if r.Config.Worker.FailAfterCommit != "" {
		r.Notes = append(r.Notes, fmt.Sprintf("Failure injection was ON (FAIL_AFTER_COMMIT=%s): every first delivery failed and was retried 60 s later, so these timings are not a normal run.", r.Config.Worker.FailAfterCommit))
	}

	// 4. CloudWatch counters. Metrics lag a minute or two behind, so wait for the API invocations.
	first := firstSubmitted(submits)
	if err := cl.collectAWSFacts(ctx, st, &r, first, r.Evaluation.Accepted, *metricsWait); err != nil {
		return err
	}
	if !r.AWS.MetricsComplete {
		r.Notes = append(r.Notes, "CloudWatch had not reported every API invocation when the tool stopped waiting: the Lambda counts above may be low.")
	}
	r.Notes = append(r.Notes, "Lambda counts are CloudWatch minute buckets starting at the minute of the first request; worker log counts start at the first request.")

	// 5. Write and print.
	if err := writeJSON(filepath.Join(c.dir(), reportFile), r); err != nil {
		return err
	}
	md := r.markdown()
	if err := os.WriteFile(filepath.Join(c.dir(), reportMD), []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Print(md)
	fmt.Fprintf(os.Stderr, "\nreport: %s\n", filepath.Join(c.dir(), reportFile))
	fmt.Fprintf(os.Stderr, "invariants: %s, workload: %s\n", verdict(r.Evaluation.Pass), map[bool]string{true: "clean", false: "NOT clean (not every request got a 202)"}[r.Evaluation.Clean])
	if !r.Evaluation.Pass {
		return errors.New("an invariant failed: see the table above")
	}
	if !drained {
		return errors.New("the run did not drain in time")
	}
	if !r.Evaluation.Clean {
		return errors.New("not every request was accepted, so this is not a clean run")
	}
	return nil
}

func firstSubmitted(submits []submit) time.Time {
	first := submits[0].SubmittedAtMs
	for _, s := range submits {
		first = min(first, s.SubmittedAtMs)
	}
	return time.UnixMilli(first)
}

// waitForDrain polls until the reservation queue has been empty for idleReads readings in a row and
// every accepted request has a stored outcome, or the timeout passes. It returns the last scan.
func (c *clients) waitForDrain(ctx context.Context, st stack, submits []submit, timeout time.Duration) (time.Duration, []row, bool, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	empty := 0
	for {
		total, err := c.queueTotal(ctx, st.queueURL)
		if err != nil {
			return 0, nil, false, err
		}
		if total == 0 {
			empty++
		} else {
			empty = 0
		}
		timedOut := time.Now().After(deadline)
		if empty >= idleReads || timedOut {
			rows, err := c.scanRows(ctx, st.tables.Reservations)
			if err != nil {
				return 0, nil, false, err
			}
			if (empty >= idleReads && hasAllAccepted(submits, rows)) || timedOut {
				return time.Since(start), rows, empty >= idleReads && hasAllAccepted(submits, rows), nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, nil, false, ctx.Err()
		case <-time.After(idleEvery):
		}
	}
}

func hasAllAccepted(submits []submit, rows []row) bool {
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.RequestID] = true
	}
	for _, s := range submits {
		if s.accepted() && !have[s.RequestID] {
			return false
		}
	}
	return true
}

func (c *clients) deployedConfig(ctx context.Context, st stack) (deployedConfig, error) {
	var d deployedConfig
	d.Region = stackinfo.Region
	capacity, err := stackinfo.ReadCapacity(ctx, c.lambda, st.workerFn, st.queueARN)
	if err != nil && capacity.AccountLimit == 0 {
		return d, err
	}
	// An uncapped worker is reported as 0 rather than failing the report: preflight already refused it.
	d.AccountLimit, d.WorkerBatchSize, d.WorkerMaxConcurrency = capacity.AccountLimit, capacity.WorkerBatchSize, capacity.WorkerMaxConcurrency
	if d.API, err = c.lambdaConfig(ctx, st.apiFunction); err != nil {
		return d, err
	}
	if d.Worker, err = c.lambdaConfig(ctx, st.workerFn); err != nil {
		return d, err
	}
	d.QueueVisibilitySec, d.QueueMaxReceiveCount, err = c.queueConfig(ctx, st.queueURL)
	return d, err
}

// collectAWSFacts fills r.AWS from CloudWatch metrics and the worker's logs.
func (c *clients) collectAWSFacts(ctx context.Context, st stack, r *report, first time.Time, accepted int, wait time.Duration) error {
	now := time.Now
	read := func(fn, metric string) (int, error) {
		return c.metricSum(ctx, fn, metric, first, now().Add(time.Minute))
	}

	deadline := now().Add(wait)
	for {
		inv, err := read(st.apiFunction, "Invocations")
		if err != nil {
			return err
		}
		r.AWS.APIInvocations = inv
		if inv >= accepted {
			r.AWS.MetricsComplete = true
			break
		}
		if now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(metricsPoll):
		}
	}

	var err error
	for _, m := range []struct {
		dst        *int
		fn, metric string
	}{
		{&r.AWS.APIThrottles, st.apiFunction, "Throttles"},
		{&r.AWS.APIErrors, st.apiFunction, "Errors"},
		{&r.AWS.WorkerInvocations, st.workerFn, "Invocations"},
		{&r.AWS.WorkerThrottles, st.workerFn, "Throttles"},
		{&r.AWS.WorkerErrors, st.workerFn, "Errors"},
	} {
		if *m.dst, err = read(m.fn, m.metric); err != nil {
			return err
		}
	}
	for _, l := range []struct {
		dst     *int
		pattern string
	}{
		{&r.AWS.WorkerRetryLines, `{ $.outcome = "RETRY" }`},
		{&r.AWS.WorkerRedeliveries, `{ $.attempt > 1 }`},
		{&r.AWS.WorkerFailedLines, `{ $.outcome = "FAILED" }`},
	} {
		if *l.dst, err = c.countLogLines(ctx, st.workerLogs, l.pattern, first); err != nil {
			return err
		}
	}
	return nil
}
