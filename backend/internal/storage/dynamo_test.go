package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

type fakeDynamo struct {
	getIn   []*dynamodb.GetItemInput
	getOut  *dynamodb.GetItemOutput
	getErr  error
	scanIn  []*dynamodb.ScanInput
	scanOut []*dynamodb.ScanOutput
	scanErr error
}

func (f *fakeDynamo) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.getIn = append(f.getIn, in)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getOut == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return f.getOut, nil
}

func (f *fakeDynamo) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.scanIn = append(f.scanIn, in)
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	out := f.scanOut[0]
	f.scanOut = f.scanOut[1:]
	return out, nil
}

var testTables = TableNames{Inventory: "inv", Reservations: "res"}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }

func cardItem(id, name, rarity, stock string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		InventoryKeyCardID: s(id), InventoryName: s(name), InventoryRarity: s(rarity), InventoryStock: n(stock),
	}
}

func TestCardsFollowsPaginationAndMapsFields(t *testing.T) {
	db := &fakeDynamo{scanOut: []*dynamodb.ScanOutput{
		{Items: []map[string]types.AttributeValue{cardItem("card-001", "Ember Drake", "rare", "10")},
			LastEvaluatedKey: map[string]types.AttributeValue{InventoryKeyCardID: s("card-001")}},
		{Items: []map[string]types.AttributeValue{cardItem("card-002", "Tide Wisp", "common", "0")}},
	}}
	got, err := NewStore(db, testTables).Cards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []reservations.Card{
		{CardID: "card-001", Name: "Ember Drake", Rarity: "rare", Available: 10},
		{CardID: "card-002", Name: "Tide Wisp", Rarity: "common", Available: 0},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v", got)
	}
	if len(db.scanIn) != 2 || *db.scanIn[0].TableName != "inv" || db.scanIn[1].ExclusiveStartKey == nil {
		t.Fatalf("scan inputs: %+v", db.scanIn)
	}
}

func TestCardsNeverAdvertiseNegativeStock(t *testing.T) {
	db := &fakeDynamo{scanOut: []*dynamodb.ScanOutput{{Items: []map[string]types.AttributeValue{cardItem("c", "n", "r", "-3")}}}}
	got, err := NewStore(db, testTables).Cards(context.Background())
	if err != nil || got[0].Available != 0 {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestCardsRejectMalformedItemsAndPropagateErrors(t *testing.T) {
	bad := []map[string]types.AttributeValue{
		{InventoryKeyCardID: s("c"), InventoryName: s("n"), InventoryRarity: s("r")},                         // no stock
		{InventoryKeyCardID: s("c"), InventoryName: s("n"), InventoryRarity: s("r"), InventoryStock: s("1")}, // stock is a string
		cardItem("c", "n", "r", "1.5"),
		cardItem("", "n", "r", "1"),
	}
	for i, item := range bad {
		db := &fakeDynamo{scanOut: []*dynamodb.ScanOutput{{Items: []map[string]types.AttributeValue{item}}}}
		if _, err := NewStore(db, testTables).Cards(context.Background()); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if _, err := NewStore(&fakeDynamo{scanErr: errors.New("boom")}, testTables).Cards(context.Background()); err == nil {
		t.Error("scan error must propagate")
	}
}

func TestCardExists(t *testing.T) {
	db := &fakeDynamo{getOut: &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{InventoryKeyCardID: s("card-001")}}}
	ok, err := NewStore(db, testTables).CardExists(context.Background(), "card-001")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if *db.getIn[0].TableName != "inv" {
		t.Errorf("table = %s", *db.getIn[0].TableName)
	}

	ok, err = NewStore(&fakeDynamo{}, testTables).CardExists(context.Background(), "card-999")
	if err != nil || ok {
		t.Fatalf("missing card: ok=%v err=%v", ok, err)
	}
	if _, err := NewStore(&fakeDynamo{getErr: errors.New("boom")}, testTables).CardExists(context.Background(), "x"); err == nil {
		t.Error("error must propagate, not read as 'not found'")
	}
}

func TestGetKeysByTrainerAndRequest(t *testing.T) {
	db := &fakeDynamo{getOut: &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
		ReservationKeyTrainerID: s("sub-A"), ReservationKeyRequestID: s("req-1"), ReservationCardID: s("card-001"),
		ReservationStatus: s("CONFIRMED"), ReservationUpdatedAt: s("2026-11-07T01:02:03+00:00"),
	}}}
	got, err := NewStore(db, testTables).Get(context.Background(), "sub-A", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	want := reservations.Reservation{
		RequestID: "req-1", TrainerID: "sub-A", CardID: "card-001", Status: reservations.StatusConfirmed,
		UpdatedAt: time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC),
	}
	if *got != want {
		t.Fatalf("got %+v", *got)
	}

	in := db.getIn[0]
	if *in.TableName != "res" || len(in.Key) != 2 {
		t.Fatalf("input: %+v", in)
	}
	if in.Key[ReservationKeyTrainerID].(*types.AttributeValueMemberS).Value != "sub-A" ||
		in.Key[ReservationKeyRequestID].(*types.AttributeValueMemberS).Value != "req-1" {
		t.Fatalf("key must be (caller sub, requestId): %+v", in.Key)
	}
	if in.ConsistentRead == nil || !*in.ConsistentRead {
		t.Error("status reads must be consistent so a just-written outcome is not missed")
	}
}

func TestGetMissingAndInvalid(t *testing.T) {
	got, err := NewStore(&fakeDynamo{}, testTables).Get(context.Background(), "sub-A", "req-1")
	if got != nil || err != nil {
		t.Fatalf("missing: got %v %v", got, err)
	}
	db := &fakeDynamo{}
	for _, c := range [][2]string{{"", "r"}, {"t", ""}} {
		if _, err := NewStore(db, testTables).Get(context.Background(), c[0], c[1]); err == nil {
			t.Errorf("%v: expected error", c)
		}
	}
	if len(db.getIn) != 0 {
		t.Error("an unkeyed read must never reach DynamoDB")
	}
	if _, err := NewStore(&fakeDynamo{getErr: errors.New("boom")}, testTables).Get(context.Background(), "t", "r"); err == nil {
		t.Error("error must propagate")
	}
}

func TestGetRejectsMalformedRecord(t *testing.T) {
	db := &fakeDynamo{getOut: &dynamodb.GetItemOutput{Item: map[string]types.AttributeValue{
		ReservationKeyTrainerID: s("t"), ReservationKeyRequestID: s("r"), ReservationCardID: s("c"),
		ReservationStatus: s("CONFIRMED"), ReservationUpdatedAt: s("yesterday"),
	}}}
	if _, err := NewStore(db, testTables).Get(context.Background(), "t", "r"); err == nil {
		t.Fatal("expected error for a bad timestamp")
	}
}
