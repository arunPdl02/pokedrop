//go:build dynamodblocal

// Integration tests against DynamoDB Local (`make test-local`). They prove the invariants with
// the real transaction semantics: stock never goes negative and a repeated request ID never
// consumes inventory twice, under concurrency.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

// localEndpoint is where DynamoDB Local listens; `make test-local` sets it.
func localEndpoint() string {
	if e := os.Getenv("DYNAMODB_LOCAL_ENDPOINT"); e != "" {
		return e
	}
	return "http://localhost:8000"
}

func localClient(t *testing.T) *dynamodb.Client {
	t.Helper()
	cfg := aws.Config{
		Region: "us-west-2",
		// DynamoDB Local accepts any credentials; these never reach AWS.
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
		}),
	}
	client := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(localEndpoint())
	})

	// Wait for the container to accept requests.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := client.ListTables(context.Background(), &dynamodb.ListTablesInput{})
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("DynamoDB Local is not reachable at %s (run `make test-local`): %v", localEndpoint(), err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

type localEnv struct {
	db     *dynamodb.Client
	tables TableNames
	ledger *Ledger
}

// newLocalEnv creates uniquely named tables with the production key schema and deletes them
// when the test ends.
func newLocalEnv(t *testing.T) *localEnv {
	t.Helper()
	db := localClient(t)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	tables := TableNames{Inventory: "inventory-" + suffix, Reservations: "reservations-" + suffix}
	ctx := context.Background()

	create := []*dynamodb.CreateTableInput{
		{
			TableName:            aws.String(tables.Inventory),
			BillingMode:          types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String(InventoryKeyCardID), AttributeType: types.ScalarAttributeTypeS}},
			KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String(InventoryKeyCardID), KeyType: types.KeyTypeHash}},
		},
		{
			TableName:   aws.String(tables.Reservations),
			BillingMode: types.BillingModePayPerRequest,
			AttributeDefinitions: []types.AttributeDefinition{
				{AttributeName: aws.String(ReservationKeyTrainerID), AttributeType: types.ScalarAttributeTypeS},
				{AttributeName: aws.String(ReservationKeyRequestID), AttributeType: types.ScalarAttributeTypeS},
			},
			KeySchema: []types.KeySchemaElement{
				{AttributeName: aws.String(ReservationKeyTrainerID), KeyType: types.KeyTypeHash},
				{AttributeName: aws.String(ReservationKeyRequestID), KeyType: types.KeyTypeRange},
			},
		},
	}
	for _, in := range create {
		if _, err := db.CreateTable(ctx, in); err != nil {
			t.Fatalf("create %s: %v", aws.ToString(in.TableName), err)
		}
		name := aws.ToString(in.TableName)
		t.Cleanup(func() {
			_, _ = db.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
		})
	}
	return &localEnv{db: db, tables: tables, ledger: NewLedger(db, tables)}
}

func (e *localEnv) seed(t *testing.T, cardID string, stock int) {
	t.Helper()
	_, err := e.db.PutItem(context.Background(), &dynamodb.PutItemInput{
		TableName: aws.String(e.tables.Inventory),
		Item:      cardItem(cardID, "Test Card", "rare", strconv.Itoa(stock)),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *localEnv) stock(t *testing.T, cardID string) int {
	t.Helper()
	out, err := e.db.GetItem(context.Background(), &dynamodb.GetItemInput{
		TableName:      aws.String(e.tables.Inventory),
		Key:            map[string]types.AttributeValue{InventoryKeyCardID: s(cardID)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := strconv.Atoi(out.Item[InventoryStock].(*types.AttributeValueMemberN).Value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// outcomes returns every stored reservation, keyed by trainer + request ID.
func (e *localEnv) outcomes(t *testing.T) map[[2]string]reservations.Reservation {
	t.Helper()
	all := map[[2]string]reservations.Reservation{}
	var start map[string]types.AttributeValue
	for {
		out, err := e.db.Scan(context.Background(), &dynamodb.ScanInput{
			TableName: aws.String(e.tables.Reservations), ConsistentRead: aws.Bool(true), ExclusiveStartKey: start,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range out.Items {
			r, err := reservationFromItem(item)
			if err != nil {
				t.Fatal(err)
			}
			all[[2]string{r.TrainerID, r.RequestID}] = *r
		}
		if len(out.LastEvaluatedKey) == 0 {
			return all
		}
		start = out.LastEvaluatedKey
	}
}

// deliver processes msg the way SQS + the worker would: a retryable failure is redelivered until
// the message reaches a terminal outcome. Any other failure fails the test.
func deliver(p *reservations.Processor, msg reservations.QueueMessage) (reservations.Recorded, int, error) {
	for delivery := 1; delivery <= 50; delivery++ {
		rec, err := p.Process(context.Background(), msg)
		if err == nil {
			return rec, delivery, nil
		}
		if !errors.Is(err, reservations.ErrRetryable) {
			return reservations.Recorded{}, delivery, err
		}
	}
	return reservations.Recorded{}, 50, fmt.Errorf("request %s never reached an outcome", msg.RequestID)
}

func requestID(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

type delivered struct {
	msg        reservations.QueueMessage
	rec        reservations.Recorded
	deliveries int
	err        error
}

// deliverConcurrently starts every message at once and waits for all of them to finish.
func deliverConcurrently(p *reservations.Processor, msgs []reservations.QueueMessage) []delivered {
	results := make([]delivered, len(msgs))
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i, m := range msgs {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			rec, n, err := deliver(p, m)
			results[i] = delivered{msg: m, rec: rec, deliveries: n, err: err}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results
}

func TestLocalConcurrentRequestsNeverOversell(t *testing.T) {
	env := newLocalEnv(t)
	const stock, requests = 5, 50
	env.seed(t, "card-001", stock)
	p := reservations.NewProcessor(env.ledger, nil)

	var msgs []reservations.QueueMessage
	for i := 0; i < requests; i++ {
		msgs = append(msgs, reservations.QueueMessage{RequestID: requestID(i), TrainerID: fmt.Sprintf("trainer-%d", i%7), CardID: "card-001"})
	}
	results := deliverConcurrently(p, msgs)

	counts := map[reservations.Status]int{}
	redelivered := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("%s: %v", r.msg.RequestID, r.err)
		}
		if r.rec.Duplicate {
			t.Fatalf("%s: unique request reported as a duplicate", r.msg.RequestID)
		}
		counts[r.rec.Reservation.Status]++
		if r.deliveries > 1 {
			redelivered++
		}
	}
	t.Logf("%d requests for %d units: %v; %d needed redelivery after in-process retries", requests, stock, counts, redelivered)

	if counts[reservations.StatusConfirmed] != stock || counts[reservations.StatusSoldOut] != requests-stock {
		t.Fatalf("outcomes = %v, want %d CONFIRMED and %d SOLD_OUT", counts, stock, requests-stock)
	}
	if got := env.stock(t, "card-001"); got != 0 {
		t.Fatalf("stock = %d, want 0", got)
	}
	stored := env.outcomes(t)
	if len(stored) != requests {
		t.Fatalf("stored %d outcomes, want %d", len(stored), requests)
	}
	confirmed := 0
	for _, r := range stored {
		if r.Status == reservations.StatusConfirmed {
			confirmed++
		}
	}
	if confirmed != stock {
		t.Fatalf("stored %d CONFIRMED, want %d", confirmed, stock)
	}
}

func TestLocalConcurrentCopiesOfOneRequestTakeOneUnit(t *testing.T) {
	env := newLocalEnv(t)
	env.seed(t, "card-001", 3)
	p := reservations.NewProcessor(env.ledger, nil)

	msg := reservations.QueueMessage{RequestID: requestID(1), TrainerID: "trainer-a", CardID: "card-001"}
	var msgs []reservations.QueueMessage
	for i := 0; i < 20; i++ {
		msgs = append(msgs, msg)
	}
	results := deliverConcurrently(p, msgs)

	stored := env.outcomes(t)
	if len(stored) != 1 {
		t.Fatalf("stored %d outcomes, want 1", len(stored))
	}
	want := stored[[2]string{"trainer-a", requestID(1)}]
	if want.Status != reservations.StatusConfirmed {
		t.Fatalf("stored %+v, want CONFIRMED", want)
	}
	firsts := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.rec.Reservation != want {
			t.Fatalf("copy returned %+v, want the stored %+v", r.rec.Reservation, want)
		}
		if !r.rec.Duplicate {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("%d copies claimed to write the outcome, want exactly 1", firsts)
	}
	if got := env.stock(t, "card-001"); got != 2 {
		t.Fatalf("stock = %d, want 2 (exactly one decrement)", got)
	}
}

// 30 distinct requests, each delivered 3 times concurrently, compete for 10 units.
func TestLocalConcurrentDuplicatesAndCompetitionTogether(t *testing.T) {
	env := newLocalEnv(t)
	const stock, requests, copies = 10, 30, 3
	env.seed(t, "card-001", stock)
	p := reservations.NewProcessor(env.ledger, nil)

	var msgs []reservations.QueueMessage
	for i := 0; i < requests; i++ {
		for c := 0; c < copies; c++ {
			msgs = append(msgs, reservations.QueueMessage{RequestID: requestID(i), TrainerID: "trainer-a", CardID: "card-001"})
		}
	}
	results := deliverConcurrently(p, msgs)

	stored := env.outcomes(t)
	if len(stored) != requests {
		t.Fatalf("stored %d outcomes, want %d", len(stored), requests)
	}
	confirmed := 0
	for _, r := range stored {
		if r.Status == reservations.StatusConfirmed {
			confirmed++
		}
	}
	if confirmed != stock {
		t.Fatalf("stored %d CONFIRMED, want %d", confirmed, stock)
	}
	for _, r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if want := stored[[2]string{r.msg.TrainerID, r.msg.RequestID}]; r.rec.Reservation != want {
			t.Fatalf("a copy of %s returned %+v, stored %+v", r.msg.RequestID, r.rec.Reservation, want)
		}
	}
	if got := env.stock(t, "card-001"); got != 0 {
		t.Fatalf("stock = %d, want 0", got)
	}
}

func TestLocalRepeatsAfterStockRunsOutKeepTheirOutcome(t *testing.T) {
	env := newLocalEnv(t)
	env.seed(t, "card-003", 1)
	p := reservations.NewProcessor(env.ledger, nil)
	a := reservations.QueueMessage{RequestID: requestID(1), TrainerID: "trainer-a", CardID: "card-003"}
	b := reservations.QueueMessage{RequestID: requestID(2), TrainerID: "trainer-b", CardID: "card-003"}

	steps := []struct {
		msg       reservations.QueueMessage
		status    reservations.Status
		duplicate bool
	}{
		{a, reservations.StatusConfirmed, false},
		{a, reservations.StatusConfirmed, true}, // stock is 0 now: still the original CONFIRMED
		{b, reservations.StatusSoldOut, false},
		{b, reservations.StatusSoldOut, true},
		{a, reservations.StatusConfirmed, true},
	}
	for i, step := range steps {
		rec, _, err := deliver(p, step.msg)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Reservation.Status != step.status || rec.Duplicate != step.duplicate {
			t.Fatalf("step %d: got %s duplicate=%v, want %s duplicate=%v", i, rec.Reservation.Status, rec.Duplicate, step.status, step.duplicate)
		}
		if got := env.stock(t, "card-003"); got != 0 {
			t.Fatalf("step %d: stock = %d, want 0", i, got)
		}
	}
}

// Records what DynamoDB Local reports when both conditions fail, and checks the classifier
// treats it as a duplicate (never SOLD_OUT, never a second decrement).
func TestLocalBothConditionsFailingIsADuplicate(t *testing.T) {
	env := newLocalEnv(t)
	env.seed(t, "card-001", 1)
	r := attempt()
	if _, err := env.ledger.Reserve(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	_, err := env.db.TransactWriteItems(context.Background(), env.ledger.reserveInput(r))
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		t.Fatalf("err = %v, want TransactionCanceledException", err)
	}
	for i, reason := range canceled.CancellationReasons {
		t.Logf("reason[%d]: code=%s returnedItem=%v", i, aws.ToString(reason.Code), len(reason.Item) > 0)
	}
	if got := classifyCancellation(canceled.CancellationReasons).kind; got != cancelDuplicate {
		t.Fatalf("kind = %d, want duplicate", got)
	}

	rec, err := env.ledger.Reserve(context.Background(), r)
	if err != nil || !rec.Duplicate || rec.Reservation.Status != reservations.StatusConfirmed {
		t.Fatalf("got %+v, %v; want the original CONFIRMED as a duplicate", rec, err)
	}
	if got := env.stock(t, "card-001"); got != 0 {
		t.Fatalf("stock = %d, want 0", got)
	}
}

func TestLocalOutOfStockIsReportedWithTheStockItem(t *testing.T) {
	env := newLocalEnv(t)
	env.seed(t, "card-001", 0)

	_, err := env.db.TransactWriteItems(context.Background(), env.ledger.reserveInput(attempt()))
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		t.Fatalf("err = %v, want TransactionCanceledException", err)
	}
	if got := classifyCancellation(canceled.CancellationReasons).kind; got != cancelOutOfStock {
		t.Fatalf("kind = %d, want out of stock; reasons = %+v", got, canceled.CancellationReasons)
	}
}

func TestLocalUnknownCardIsPermanentAndWritesNothing(t *testing.T) {
	env := newLocalEnv(t)
	p := reservations.NewProcessor(env.ledger, nil)

	_, err := p.Process(context.Background(), reservations.QueueMessage{RequestID: requestID(1), TrainerID: "trainer-a", CardID: "card-404"})
	if !errors.Is(err, reservations.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
	if n := len(env.outcomes(t)); n != 0 {
		t.Fatalf("stored %d outcomes for an unknown card", n)
	}
}
