//go:build integration

// Package integration holds the acceptance tests that run against a deployed pokedrop-test stack
// (docs/PLAN.md, Phase 6). They go through the real API, queue, worker and tables, so they need
// AWS credentials and two trainer tokens:
//
//	make test-integration STAGE=test          # rows 1, 2 and 5: failure injection off
//	make test-integration-failure STAGE=test  # row 3: failure injection on (make fail-on MODE=once)
//
// Row 4 (a permanently failing message reaches the DLQ and raises the alarms) takes more than ten
// minutes and is not repeated here: see "Drill results" in docs/runbook.md.
//
// The tests reset card stock, so they refuse to run against any stage but test.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/arunPdl02/pokedrop/backend/internal/stackinfo"
	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

const (
	// pollEvery is how often the tests poll the API, the queue and the logs.
	pollEvery = time.Second
	// idleReads is how many consecutive empty readings count as an empty queue. SQS queue counts
	// are approximate, so one empty reading is not proof.
	idleReads = 3
	idleEvery = 2 * time.Second
)

// harness is everything the tests need: the deployed stack, AWS clients and two trainers.
type harness struct {
	stage       string
	apiURL      string
	tables      storage.TableNames
	queueURL    string
	dlqURL      string
	workerFn    string
	workerLogs  string
	capacity    stackinfo.Capacity
	tokenA      string
	tokenB      string
	http        *http.Client
	db          *dynamodb.Client
	sqs         *sqs.Client
	lambda      *lambda.Client
	logs        *cloudwatchlogs.Client
	startedAt   time.Time
	concurrency int
}

var h *harness

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	stage := os.Getenv("STAGE")
	if stage == "" {
		stage = "test"
	}
	if stage != "test" {
		fmt.Fprintf(os.Stderr, "integration: refusing to run against stage %q: the tests reset card stock, so only the test stage is allowed\n", stage)
		return 2
	}
	tokenA, tokenB := os.Getenv("TOKEN_A"), os.Getenv("TOKEN_B")
	if tokenA == "" || tokenB == "" {
		fmt.Fprintln(os.Stderr, "integration: TOKEN_A and TOKEN_B are required (use make test-integration, which fetches them with make token)")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	built, err := newHarness(ctx, stage, tokenA, tokenB)
	if err != nil {
		if hint := stackinfo.ExpiredTokenHint(err); hint != "" {
			fmt.Fprintln(os.Stderr, hint)
		}
		fmt.Fprintln(os.Stderr, "integration:", err)
		return 2
	}
	h = built
	fmt.Fprintf(os.Stderr, "integration: stage=%s Lambda account limit=%d worker max concurrency=%d, so at most %d requests in flight\n",
		h.stage, h.capacity.AccountLimit, h.capacity.WorkerMaxConcurrency, h.concurrency)
	return m.Run()
}

func newHarness(ctx context.Context, stage, tokenA, tokenB string) (*harness, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(stackinfo.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	outputs, err := stackinfo.LoadOutputs(ctx, cloudformation.NewFromConfig(cfg), stage)
	if err != nil {
		return nil, err
	}
	need := func(key string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = outputs.Require(key)
		return v
	}
	b := &harness{
		stage:      stage,
		apiURL:     strings.TrimRight(need(stackinfo.OutputAPIURL), "/"),
		tables:     storage.TableNames{Inventory: need(stackinfo.OutputInventoryTable), Reservations: need(stackinfo.OutputReservationsTable)},
		queueURL:   need(stackinfo.OutputQueueURL),
		dlqURL:     need(stackinfo.OutputDLQURL),
		workerFn:   need(stackinfo.OutputWorkerFunction),
		workerLogs: need(stackinfo.OutputWorkerLogGroup),
		tokenA:     tokenA,
		tokenB:     tokenB,
		http:       &http.Client{Timeout: 30 * time.Second},
		db:         dynamodb.NewFromConfig(cfg),
		sqs:        sqs.NewFromConfig(cfg),
		lambda:     lambda.NewFromConfig(cfg),
		logs:       cloudwatchlogs.NewFromConfig(cfg),
		startedAt:  time.Now(),
	}
	queueARN := need(stackinfo.OutputQueueARN)
	if err != nil {
		return nil, err
	}
	b.capacity, err = stackinfo.ReadCapacity(ctx, b.lambda, b.workerFn, queueARN)
	if err != nil {
		return nil, err
	}
	b.concurrency = b.capacity.SafeConcurrency
	return b, nil
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// forEach runs fn(0..n-1) with at most h.concurrency running at once, which keeps the API Lambda
// inside the account's Lambda concurrency next to the worker.
func (h *harness) forEach(n int, fn func(i int)) {
	sem := make(chan struct{}, h.concurrency)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

// ---- API -----------------------------------------------------------------------------------

type apiResponse struct {
	Status int
	Body   []byte
}

func (h *harness) do(ctx context.Context, method, path, token string, body any) (apiResponse, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.apiURL+path, reader)
	if err != nil {
		return apiResponse{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return apiResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return apiResponse{Status: resp.StatusCode, Body: data}, err
}

// submit posts one reservation request and reports whether the API queued it (202 QUEUED).
func (h *harness) submit(ctx context.Context, token, requestID, cardID string) error {
	resp, err := h.do(ctx, http.MethodPost, "/reservations", token, map[string]string{"requestId": requestID, "cardId": cardID})
	if err != nil {
		return fmt.Errorf("POST %s: %w", requestID, err)
	}
	var out struct {
		RequestID string `json:"requestId"`
		Status    string `json:"status"`
	}
	if resp.Status != http.StatusAccepted || json.Unmarshal(resp.Body, &out) != nil || out.Status != "QUEUED" || out.RequestID != requestID {
		return fmt.Errorf("POST %s: got %d %s, want 202 QUEUED", requestID, resp.Status, resp.Body)
	}
	return nil
}

type reservation struct {
	RequestID string `json:"requestId"`
	CardID    string `json:"cardId"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
}

// getReservation reads one reservation. found is false for a 404.
func (h *harness) getReservation(ctx context.Context, token, requestID string) (r reservation, found bool, err error) {
	resp, err := h.do(ctx, http.MethodGet, "/reservations/"+requestID, token, nil)
	if err != nil {
		return reservation{}, false, err
	}
	switch resp.Status {
	case http.StatusOK:
		if err := json.Unmarshal(resp.Body, &r); err != nil {
			return reservation{}, false, fmt.Errorf("GET %s: bad body %s: %w", requestID, resp.Body, err)
		}
		return r, true, nil
	case http.StatusNotFound:
		return reservation{}, false, nil
	}
	return reservation{}, false, fmt.Errorf("GET %s: unexpected %d %s", requestID, resp.Status, resp.Body)
}

// awaitOutcome polls until the worker has recorded a terminal outcome. A 404 means "not processed
// yet" (docs/api.md), so it keeps polling until ctx ends.
func (h *harness) awaitOutcome(ctx context.Context, token, requestID string) (reservation, error) {
	for {
		r, found, err := h.getReservation(ctx, token, requestID)
		if err != nil {
			return reservation{}, err
		}
		if found {
			if r.Status != "CONFIRMED" && r.Status != "SOLD_OUT" {
				return reservation{}, fmt.Errorf("%s: unexpected status %q", requestID, r.Status)
			}
			return r, nil
		}
		select {
		case <-ctx.Done():
			return reservation{}, fmt.Errorf("%s: no outcome before the deadline: %w", requestID, ctx.Err())
		case <-time.After(pollEvery):
		}
	}
}

// ---- DynamoDB -------------------------------------------------------------------------------

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

// setStock sets a seeded card's stock. The card must already exist (make seed), so a test can never
// invent inventory in an unseeded stack.
func (h *harness) setStock(t *testing.T, ctx context.Context, cardID string, stock int) {
	t.Helper()
	_, err := h.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String(h.tables.Inventory),
		Key:                       map[string]types.AttributeValue{storage.InventoryKeyCardID: str(cardID)},
		UpdateExpression:          aws.String("SET #stock = :n"),
		ConditionExpression:       aws.String("attribute_exists(#id)"),
		ExpressionAttributeNames:  map[string]string{"#stock": storage.InventoryStock, "#id": storage.InventoryKeyCardID},
		ExpressionAttributeValues: map[string]types.AttributeValue{":n": &types.AttributeValueMemberN{Value: fmt.Sprint(stock)}},
	})
	if err != nil {
		t.Fatalf("set stock of %s to %d (is the stack seeded? run make seed STAGE=%s): %v", cardID, stock, h.stage, err)
	}
}

// stock reads a card's stock with a consistent read.
func (h *harness) stock(t *testing.T, ctx context.Context, cardID string) int {
	t.Helper()
	out, err := h.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(h.tables.Inventory),
		Key:            map[string]types.AttributeValue{storage.InventoryKeyCardID: str(cardID)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatalf("read stock of %s: %v", cardID, err)
	}
	n, ok := out.Item[storage.InventoryStock].(*types.AttributeValueMemberN)
	if !ok {
		t.Fatalf("card %s has no numeric stock: %v", cardID, out.Item)
	}
	var v int
	if _, err := fmt.Sscan(n.Value, &v); err != nil {
		t.Fatalf("card %s stock %q: %v", cardID, n.Value, err)
	}
	return v
}

type row struct {
	TrainerID, RequestID, CardID, Status, UpdatedAt string
}

// rowsByRequestID scans the whole reservations table (consistent) and groups the rows by request
// ID across every trainer. A request ID with two rows is exactly the failure the tests look for,
// and only a scan can see a second row under another trainer. The test table is small.
func (h *harness) rowsByRequestID(t *testing.T, ctx context.Context) map[string][]row {
	t.Helper()
	out := map[string][]row{}
	var start map[string]types.AttributeValue
	for {
		page, err := h.db.Scan(ctx, &dynamodb.ScanInput{
			TableName: aws.String(h.tables.Reservations), ConsistentRead: aws.Bool(true), ExclusiveStartKey: start,
		})
		if err != nil {
			t.Fatalf("scan reservations: %v", err)
		}
		for _, item := range page.Items {
			get := func(k string) string {
				if v, ok := item[k].(*types.AttributeValueMemberS); ok {
					return v.Value
				}
				return ""
			}
			r := row{get(storage.ReservationKeyTrainerID), get(storage.ReservationKeyRequestID), get(storage.ReservationCardID), get(storage.ReservationStatus), get(storage.ReservationUpdatedAt)}
			out[r.RequestID] = append(out[r.RequestID], r)
		}
		if page.LastEvaluatedKey == nil {
			return out
		}
		start = page.LastEvaluatedKey
	}
}

// ---- SQS -----------------------------------------------------------------------------------

func (h *harness) depth(ctx context.Context, url string) (int, error) {
	out, err := h.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, v := range out.Attributes {
		var n int
		if _, err := fmt.Sscan(v, &n); err != nil {
			return 0, fmt.Errorf("queue attribute %q: %w", v, err)
		}
		total += n
	}
	return total, nil
}

// awaitIdle waits until the reservation queue has no visible, in-flight or delayed messages for
// idleReads consecutive readings, and then checks that nothing was dead-lettered: a valid message
// in the DLQ would mean the worker gave up on it.
func (h *harness) awaitIdle(t *testing.T, ctx context.Context) {
	t.Helper()
	consecutive := 0
	for consecutive < idleReads {
		n, err := h.depth(ctx, h.queueURL)
		if err != nil {
			t.Fatalf("read queue depth: %v", err)
		}
		if n == 0 {
			consecutive++
		} else {
			consecutive = 0
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the queue was not empty before the deadline (last depth %d)", n)
		case <-time.After(idleEvery):
		}
	}
	if n, err := h.depth(ctx, h.dlqURL); err != nil || n != 0 {
		t.Fatalf("dead letter queue depth = %d (err %v), want 0", n, err)
	}
}

// ---- worker logs and configuration ---------------------------------------------------------

// workerLines returns the worker's JSON log lines for one request, oldest first.
func (h *harness) workerLines(ctx context.Context, requestID string) ([]map[string]any, error) {
	var lines []map[string]any
	var token *string
	for {
		out, err := h.logs.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(h.workerLogs),
			FilterPattern: aws.String(fmt.Sprintf(`{ $.requestId = %q }`, requestID)),
			StartTime:     aws.Int64(h.startedAt.Add(-time.Minute).UnixMilli()),
			NextToken:     token,
		})
		if err != nil {
			return nil, err
		}
		for _, e := range out.Events {
			var line map[string]any
			if json.Unmarshal([]byte(aws.ToString(e.Message)), &line) == nil {
				lines = append(lines, line)
			}
		}
		if out.NextToken == nil {
			return lines, nil
		}
		token = out.NextToken
	}
}

// failMode is the deployed worker's FAIL_AFTER_COMMIT setting ("" when off).
func (h *harness) failMode(ctx context.Context) (string, error) {
	out, err := h.lambda.GetFunctionConfiguration(ctx, &lambda.GetFunctionConfigurationInput{FunctionName: aws.String(h.workerFn)})
	if err != nil {
		return "", err
	}
	if out.Environment == nil {
		return "", nil
	}
	v := out.Environment.Variables["FAIL_AFTER_COMMIT"]
	if v == "off" {
		return "", nil
	}
	return v, nil
}

// requireFailMode stops a test that would prove nothing, or be slow and misleading, under the
// wrong failure-injection setting.
func (h *harness) requireFailMode(t *testing.T, want string) {
	t.Helper()
	ctx := testContext(t, time.Minute)
	got, err := h.failMode(ctx)
	if err != nil {
		t.Fatalf("read the worker's FAIL_AFTER_COMMIT: %v", err)
	}
	if got == want {
		return
	}
	fix := "make fail-off STAGE=" + h.stage
	if want != "" {
		fix = fmt.Sprintf("make fail-on STAGE=%s MODE=%s", h.stage, want)
	}
	t.Fatalf("the worker's FAIL_AFTER_COMMIT is %q but this test needs %q (after cdk diff, run: %s)", got, want, fix)
}
