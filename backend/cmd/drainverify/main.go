// Command drainverify checks a load test run against the deployed stack.
//
//	drainverify preflight -run small -requests 20 -concurrency 2   before the run
//	drainverify verify    -run small -stock 10                     after the run
//
// preflight reads the account's Lambda concurrency limit and the worker's cap, refuses a
// concurrency that could throttle the API or the worker, checks the local clock against the API's,
// and records the planned workload. verify waits for the queue to drain, checks the invariants
// (stock never negative, no request ID with two rows, CONFIRMED = min(stock, requests), final stock),
// and computes the benchmark numbers from the submit log that tests/load/burst.js wrote.
//
// It only reads AWS. It refuses the prod stage. Run it through `make load` and `make drain-verify`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/arunPdl02/pokedrop/backend/internal/stackinfo"
)

const (
	submitsFile = "submits.jsonl"
	loadFile    = "load.json"
	reportFile  = "report.json"
	reportMD    = "report.md"

	// maxSkew is how far the local clock may differ from the API's before time to final outcome
	// stops being meaningful. updatedAt only resolves a second, so a larger skew is the bigger error.
	maxSkew = time.Second
)

var runName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "preflight":
		err = preflight(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		if hint := stackinfo.ExpiredTokenHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		fmt.Fprintln(os.Stderr, "drainverify:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drainverify preflight|verify -run NAME [flags]   (see make load and make drain-verify)")
	os.Exit(2)
}

// common holds the flags both commands share.
type common struct {
	stage   string
	run     string
	results string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.stage, "stage", "test", "stack stage (stack name: pokedrop-<stage>); prod is refused")
	fs.StringVar(&c.run, "run", "", "run name; files go in <results>/<run>/")
	fs.StringVar(&c.results, "results", "../tests/load/results", "directory holding one folder per run")
}

func (c *common) check() error {
	if c.stage == "prod" {
		return errors.New("refusing to load test prod")
	}
	if !runName.MatchString(c.run) {
		return fmt.Errorf("-run must be lowercase letters, digits and dashes (got %q)", c.run)
	}
	return nil
}

func (c *common) dir() string { return filepath.Join(c.results, c.run) }

func newClients(ctx context.Context) (*clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(stackinfo.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &clients{
		cf:     cloudformation.NewFromConfig(cfg),
		lambda: lambda.NewFromConfig(cfg),
		sqs:    sqs.NewFromConfig(cfg),
		db:     dynamodb.NewFromConfig(cfg),
		cw:     cloudwatch.NewFromConfig(cfg),
		logs:   cloudwatchlogs.NewFromConfig(cfg),
	}, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// preflight gates a run: it refuses a concurrency the account cannot take and a skewed clock, and
// records the workload and limits in <run>/load.json.
func preflight(args []string) error {
	var c common
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	c.register(fs)
	requests := fs.Int("requests", 0, "number of requests the run will send")
	concurrency := fs.Int("concurrency", 0, "virtual users the run will use")
	card := fs.String("card", "card-001", "card every request asks for")
	_ = fs.Parse(args)
	if err := c.check(); err != nil {
		return err
	}
	if *requests < 1 || *concurrency < 1 {
		return errors.New("-requests and -concurrency must be at least 1")
	}
	if _, err := os.Stat(filepath.Join(c.dir(), submitsFile)); err == nil {
		return fmt.Errorf("run %q already has a %s: pick a new RUN name, or delete %s on purpose", c.run, submitsFile, c.dir())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cl, err := newClients(ctx)
	if err != nil {
		return err
	}
	st, err := cl.loadStack(ctx, c.stage)
	if err != nil {
		return err
	}
	capacity, capErr := stackinfo.ReadCapacity(ctx, cl.lambda, st.workerFn, st.queueARN)
	fmt.Fprintf(os.Stderr, "preflight: account Lambda concurrency limit %d, worker maxConcurrency %d, so at most %d requests in flight\n",
		capacity.AccountLimit, capacity.WorkerMaxConcurrency, capacity.SafeConcurrency)
	if capErr != nil {
		return capErr
	}
	if *concurrency > capacity.SafeConcurrency {
		return fmt.Errorf("concurrency %d is too high: with an account limit of %d and a worker that can use %d, more than %d in flight could throttle the worker and push valid messages to the dead letter queue",
			*concurrency, capacity.AccountLimit, capacity.WorkerMaxConcurrency, capacity.SafeConcurrency)
	}

	skew, err := clockSkew(ctx, st.apiURL)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "preflight: local clock is %d ms ahead of the API's (limit %d ms)\n", skew.Milliseconds(), maxSkew.Milliseconds())
	if skew > maxSkew || skew < -maxSkew {
		return fmt.Errorf("the local clock is %d ms off the API's, which would skew time to final outcome: sync the clock first (on WSL, `sudo hwclock -s` or restarting WSL usually does it)", skew.Milliseconds())
	}

	if err := os.MkdirAll(c.dir(), 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(c.dir(), loadFile), loadParams{
		Run: c.run, Stage: c.stage, CardID: *card, Requests: *requests, Concurrency: *concurrency,
		PreflightAt: time.Now().UTC(), AccountLimit: capacity.AccountLimit,
		WorkerMaxConcurrency: capacity.WorkerMaxConcurrency, SafeConcurrency: capacity.SafeConcurrency,
		ClockSkewMs: skew.Milliseconds(),
	})
}
