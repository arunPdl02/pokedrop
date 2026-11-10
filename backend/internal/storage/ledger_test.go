package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

type fakeLedgerDB struct {
	txIn   []*dynamodb.TransactWriteItemsInput
	txErr  error
	putIn  []*dynamodb.PutItemInput
	putErr error
	getIn  []*dynamodb.GetItemInput
	getOut *dynamodb.GetItemOutput
	getErr error
}

func (f *fakeLedgerDB) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	f.txIn = append(f.txIn, in)
	if f.txErr != nil {
		return nil, f.txErr
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (f *fakeLedgerDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putIn = append(f.putIn, in)
	if f.putErr != nil {
		return nil, f.putErr
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeLedgerDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getIn = append(f.getIn, in)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getOut == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return f.getOut, nil
}

const ledgerReqID = "3f2b8c1e-6d4a-4a57-9b0e-1c2d3e4f5a6b"

var ledgerTime = time.Date(2026, 11, 7, 20, 15, 30, 0, time.UTC)

func attempt() reservations.Reservation {
	return reservations.Reservation{
		RequestID: ledgerReqID, TrainerID: "trainer-a", CardID: "card-001",
		Status: reservations.StatusConfirmed, UpdatedAt: ledgerTime,
	}
}

func storedOutcome(status reservations.Status) map[string]types.AttributeValue {
	r := attempt()
	r.Status = status
	return reservationItem(r)
}

func reason(code string, item map[string]types.AttributeValue) types.CancellationReason {
	return types.CancellationReason{Code: aws.String(code), Item: item}
}

func canceled(reasons ...types.CancellationReason) error {
	return &types.TransactionCanceledException{Message: aws.String("canceled"), CancellationReasons: reasons}
}

func TestReserveWritesStockAndOutcomeInOneTransaction(t *testing.T) {
	db := &fakeLedgerDB{}
	rec, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicate || rec.Reservation != attempt() {
		t.Fatalf("got %+v", rec)
	}
	if len(db.txIn) != 1 || len(db.txIn[0].TransactItems) != 2 {
		t.Fatalf("want one transaction of two items, got %+v", db.txIn)
	}

	upd := db.txIn[0].TransactItems[txInventory].Update
	if upd == nil || aws.ToString(upd.TableName) != "inv" {
		t.Fatalf("item 0 must update the inventory table: %+v", db.txIn[0].TransactItems[0])
	}
	if got := upd.Key[InventoryKeyCardID].(*types.AttributeValueMemberS).Value; got != "card-001" {
		t.Fatalf("inventory key = %q", got)
	}
	if aws.ToString(upd.UpdateExpression) != "SET #stock = #stock - :one" ||
		aws.ToString(upd.ConditionExpression) != "#stock >= :one" ||
		upd.ExpressionAttributeNames["#stock"] != InventoryStock ||
		upd.ExpressionAttributeValues[":one"].(*types.AttributeValueMemberN).Value != "1" {
		t.Fatalf("stock must be decremented by one only if stock >= 1: %+v", upd)
	}
	if upd.ReturnValuesOnConditionCheckFailure != types.ReturnValuesOnConditionCheckFailureAllOld {
		t.Fatal("inventory update must return the old item on condition failure")
	}

	put := db.txIn[0].TransactItems[txReservation].Put
	if put == nil || aws.ToString(put.TableName) != "res" {
		t.Fatalf("item 1 must put into the reservations table: %+v", db.txIn[0].TransactItems[1])
	}
	if aws.ToString(put.ConditionExpression) != "attribute_not_exists(#requestId)" ||
		put.ExpressionAttributeNames["#requestId"] != ReservationKeyRequestID {
		t.Fatalf("outcome must be put only if none exists: %+v", put)
	}
	if put.ReturnValuesOnConditionCheckFailure != types.ReturnValuesOnConditionCheckFailureAllOld {
		t.Fatal("reservation put must return the existing outcome on condition failure")
	}
	got, err := reservationFromItem(put.Item)
	if err != nil {
		t.Fatal(err)
	}
	if *got != attempt() {
		t.Fatalf("stored %+v, want %+v", got, attempt())
	}
}

func TestReserveForcesConfirmedStatus(t *testing.T) {
	db := &fakeLedgerDB{}
	r := attempt()
	r.Status = reservations.StatusSoldOut
	if _, err := NewLedger(db, testTables).Reserve(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	put := db.txIn[0].TransactItems[txReservation].Put
	if put.Item[ReservationStatus].(*types.AttributeValueMemberS).Value != string(reservations.StatusConfirmed) {
		t.Fatal("a reserve transaction must only ever record CONFIRMED")
	}
}

func TestClassifyCancellation(t *testing.T) {
	stock := map[string]types.AttributeValue{InventoryKeyCardID: s("card-001"), InventoryStock: n("0")}
	outcome := storedOutcome(reservations.StatusConfirmed)

	cases := []struct {
		name    string
		reasons []types.CancellationReason
		want    cancelKind
	}{
		{"duplicate", []types.CancellationReason{reason("None", nil), reason("ConditionalCheckFailed", outcome)}, cancelDuplicate},
		{"duplicate and out of stock is a duplicate", []types.CancellationReason{reason("ConditionalCheckFailed", stock), reason("ConditionalCheckFailed", outcome)}, cancelDuplicate},
		{"duplicate while inventory conflicts", []types.CancellationReason{reason("TransactionConflict", nil), reason("ConditionalCheckFailed", outcome)}, cancelDuplicate},
		{"out of stock", []types.CancellationReason{reason("ConditionalCheckFailed", stock), reason("None", nil)}, cancelOutOfStock},
		{"out of stock with absent reservation code", []types.CancellationReason{reason("ConditionalCheckFailed", stock), {}}, cancelOutOfStock},
		{"card missing", []types.CancellationReason{reason("ConditionalCheckFailed", nil), reason("None", nil)}, cancelCardMissing},
		{"out of stock while reservation conflicts retries", []types.CancellationReason{reason("ConditionalCheckFailed", stock), reason("TransactionConflict", nil)}, cancelRetryable},
		{"inventory conflict", []types.CancellationReason{reason("TransactionConflict", nil), reason("None", nil)}, cancelRetryable},
		{"reservation conflict", []types.CancellationReason{reason("None", nil), reason("TransactionConflict", nil)}, cancelRetryable},
		{"throttled", []types.CancellationReason{reason("ThrottlingError", nil), reason("None", nil)}, cancelRetryable},
		{"throughput exceeded", []types.CancellationReason{reason("None", nil), reason("ProvisionedThroughputExceeded", nil)}, cancelRetryable},
		{"request limit", []types.CancellationReason{reason("RequestLimitExceeded", nil), reason("None", nil)}, cancelRetryable},
		{"validation error", []types.CancellationReason{reason("ValidationError", nil), reason("None", nil)}, cancelPermanent},
		{"item collection too large", []types.CancellationReason{reason("None", nil), reason("ItemCollectionSizeLimitExceeded", nil)}, cancelPermanent},
		{"conflict beats permanent", []types.CancellationReason{reason("ValidationError", nil), reason("TransactionConflict", nil)}, cancelRetryable},
		{"unknown code retries", []types.CancellationReason{reason("SomethingNew", nil), reason("None", nil)}, cancelRetryable},
		{"wrong number of reasons retries", []types.CancellationReason{reason("ConditionalCheckFailed", stock)}, cancelRetryable},
		{"no reasons retries", nil, cancelRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCancellation(tc.reasons).kind; got != tc.want {
				t.Fatalf("kind = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReserveDuplicateReturnsStoredOutcomeFromCondition(t *testing.T) {
	db := &fakeLedgerDB{txErr: canceled(reason("None", nil), reason("ConditionalCheckFailed", storedOutcome(reservations.StatusSoldOut)))}
	rec, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation.Status != reservations.StatusSoldOut {
		t.Fatalf("got %+v, want the stored SOLD_OUT", rec)
	}
	if len(db.getIn) != 0 {
		t.Fatal("no read is needed when the condition returned the item")
	}
}

func TestReserveDuplicateFallsBackToConsistentRead(t *testing.T) {
	db := &fakeLedgerDB{
		txErr:  canceled(reason("None", nil), reason("ConditionalCheckFailed", nil)),
		getOut: &dynamodb.GetItemOutput{Item: storedOutcome(reservations.StatusConfirmed)},
	}
	rec, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation.Status != reservations.StatusConfirmed {
		t.Fatalf("got %+v", rec)
	}
	if len(db.getIn) != 1 || !aws.ToBool(db.getIn[0].ConsistentRead) || aws.ToString(db.getIn[0].TableName) != "res" {
		t.Fatalf("want one consistent read of the reservations table, got %+v", db.getIn)
	}
	key := db.getIn[0].Key
	if key[ReservationKeyTrainerID].(*types.AttributeValueMemberS).Value != "trainer-a" ||
		key[ReservationKeyRequestID].(*types.AttributeValueMemberS).Value != ledgerReqID {
		t.Fatalf("read key = %+v", key)
	}
}

func TestReserveDuplicateWithNothingToReadIsRetryable(t *testing.T) {
	db := &fakeLedgerDB{txErr: canceled(reason("None", nil), reason("ConditionalCheckFailed", nil))}
	_, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
	if !errors.Is(err, reservations.ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}
}

func TestReserveDuplicateWithUnreadableOutcomeIsPermanent(t *testing.T) {
	bad := storedOutcome(reservations.StatusConfirmed)
	bad[ReservationUpdatedAt] = s("yesterday")
	db := &fakeLedgerDB{txErr: canceled(reason("None", nil), reason("ConditionalCheckFailed", bad))}
	_, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
	if !errors.Is(err, reservations.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

// When the failed condition returned no item, the fallback consistent read can itself fail. A
// transient failure must retry (the outcome exists, we just could not read it), a permanent one
// must not.
func TestReserveDuplicateFallbackReadErrorsAreClassified(t *testing.T) {
	cases := map[string]struct {
		err  error
		want error
	}{
		"throttled":     {&smithy.GenericAPIError{Code: "ThrottlingException"}, reservations.ErrRetryable},
		"timeout":       {context.DeadlineExceeded, reservations.ErrRetryable},
		"access denied": {&smithy.GenericAPIError{Code: "AccessDeniedException"}, reservations.ErrPermanent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := &fakeLedgerDB{
				txErr:  canceled(reason("None", nil), reason("ConditionalCheckFailed", nil)),
				getErr: tc.err,
			}
			_, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
			if !errors.Is(err, tc.want) || !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v wrapping its cause", err, tc.want)
			}
			if len(db.getIn) != 1 {
				t.Fatalf("made %d reads, want 1", len(db.getIn))
			}
		})
	}
}

func TestRecordSoldOutDuplicateFallsBackToConsistentRead(t *testing.T) {
	db := &fakeLedgerDB{
		putErr: &types.ConditionalCheckFailedException{},
		getOut: &dynamodb.GetItemOutput{Item: storedOutcome(reservations.StatusConfirmed)},
	}
	rec, err := NewLedger(db, testTables).RecordSoldOut(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation.Status != reservations.StatusConfirmed {
		t.Fatalf("got %+v, want the stored CONFIRMED as a duplicate", rec)
	}
	if len(db.getIn) != 1 || !aws.ToBool(db.getIn[0].ConsistentRead) {
		t.Fatalf("want one consistent read, got %+v", db.getIn)
	}
}

func TestReserveErrors(t *testing.T) {
	stock := map[string]types.AttributeValue{InventoryKeyCardID: s("card-001"), InventoryStock: n("0")}
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"out of stock", canceled(reason("ConditionalCheckFailed", stock), reason("None", nil)), reservations.ErrOutOfStock},
		{"card missing", canceled(reason("ConditionalCheckFailed", nil), reason("None", nil)), reservations.ErrPermanent},
		{"conflict", canceled(reason("TransactionConflict", nil), reason("None", nil)), reservations.ErrRetryable},
		{"canceled with validation error", canceled(reason("ValidationError", nil), reason("None", nil)), reservations.ErrPermanent},
		{"throttled request", &smithy.GenericAPIError{Code: "ThrottlingException"}, reservations.ErrRetryable},
		{"server error", &types.InternalServerError{}, reservations.ErrRetryable},
		{"validation exception", &smithy.GenericAPIError{Code: "ValidationException"}, reservations.ErrPermanent},
		{"missing table", &types.ResourceNotFoundException{}, reservations.ErrPermanent},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException"}, reservations.ErrPermanent},
		{"timeout", context.DeadlineExceeded, reservations.ErrRetryable},
		{"network", errors.New("connection reset"), reservations.ErrRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeLedgerDB{txErr: tc.err}
			_, err := NewLedger(db, testTables).Reserve(context.Background(), attempt())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want != reservations.ErrOutOfStock && !errors.Is(err, tc.err) {
				t.Fatalf("err = %v does not keep its cause", err)
			}
		})
	}
}

func TestRecordSoldOutPutsOnlyIfNoOutcomeExists(t *testing.T) {
	db := &fakeLedgerDB{}
	rec, err := NewLedger(db, testTables).RecordSoldOut(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Duplicate || rec.Reservation.Status != reservations.StatusSoldOut {
		t.Fatalf("got %+v", rec)
	}
	if len(db.txIn) != 0 {
		t.Fatal("sold out must not touch inventory")
	}
	put := db.putIn[0]
	if aws.ToString(put.TableName) != "res" || aws.ToString(put.ConditionExpression) != "attribute_not_exists(#requestId)" ||
		put.ExpressionAttributeNames["#requestId"] != ReservationKeyRequestID ||
		put.ReturnValuesOnConditionCheckFailure != types.ReturnValuesOnConditionCheckFailureAllOld {
		t.Fatalf("put = %+v", put)
	}
	if put.Item[ReservationStatus].(*types.AttributeValueMemberS).Value != string(reservations.StatusSoldOut) {
		t.Fatal("status must be SOLD_OUT")
	}
}

func TestRecordSoldOutNeverOverwritesAnOutcome(t *testing.T) {
	db := &fakeLedgerDB{putErr: &types.ConditionalCheckFailedException{Item: storedOutcome(reservations.StatusConfirmed)}}
	rec, err := NewLedger(db, testTables).RecordSoldOut(context.Background(), attempt())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Reservation.Status != reservations.StatusConfirmed {
		t.Fatalf("got %+v, want the existing CONFIRMED", rec)
	}
}

func TestRecordSoldOutErrors(t *testing.T) {
	cases := map[string]struct {
		err  error
		want error
	}{
		"throttled":  {&smithy.GenericAPIError{Code: "ThrottlingException"}, reservations.ErrRetryable},
		"validation": {&smithy.GenericAPIError{Code: "ValidationException"}, reservations.ErrPermanent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := &fakeLedgerDB{putErr: tc.err}
			_, err := NewLedger(db, testTables).RecordSoldOut(context.Background(), attempt())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
