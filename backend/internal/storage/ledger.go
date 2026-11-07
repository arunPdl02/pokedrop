package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

// ledgerAPI is the slice of the DynamoDB client the worker's writes use.
type ledgerAPI interface {
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// Ledger records reservation outcomes and takes inventory. It implements reservations.Ledger and
// is used only by the worker.
type Ledger struct {
	db     ledgerAPI
	tables TableNames
}

// NewLedger returns a Ledger over db using the given table names.
func NewLedger(db ledgerAPI, tables TableNames) *Ledger {
	return &Ledger{db: db, tables: tables}
}

var _ reservations.Ledger = (*Ledger)(nil)

// Positions of the items in the reserve transaction. Cancellation reasons come back in the same
// order, which is how a failed condition is attributed to inventory or to the reservation.
const (
	txInventory   = 0
	txReservation = 1
	txItems       = 2
)

// Expression placeholders shared by the conditional writes.
var (
	stockNames = map[string]string{"#stock": InventoryStock}
	one        = &types.AttributeValueMemberN{Value: "1"}
	// Any existing outcome for (trainerId, requestId) blocks the write.
	noOutcomeCondition = aws.String("attribute_not_exists(#requestId)")
	noOutcomeNames     = map[string]string{"#requestId": ReservationKeyRequestID}
)

// Reserve decrements the card's stock (only if stock >= 1) and puts r as CONFIRMED (only if no
// outcome exists for the request) in one transaction, so both happen or neither does.
func (l *Ledger) Reserve(ctx context.Context, r reservations.Reservation) (reservations.Recorded, error) {
	r.Status = reservations.StatusConfirmed
	_, err := l.db.TransactWriteItems(ctx, l.reserveInput(r))
	if err == nil {
		return reservations.Recorded{Reservation: r}, nil
	}

	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return reservations.Recorded{}, classifyError("reserve", err)
	}
	c := classifyCancellation(canceled.CancellationReasons)
	switch c.kind {
	case cancelDuplicate:
		return l.existing(ctx, r, c.existing)
	case cancelOutOfStock:
		return reservations.Recorded{}, reservations.ErrOutOfStock
	case cancelCardMissing:
		return reservations.Recorded{}, fmt.Errorf("%w: card %q is not in the inventory: %w", reservations.ErrPermanent, r.CardID, err)
	case cancelPermanent:
		return reservations.Recorded{}, fmt.Errorf("%w: reserve canceled (%s): %w", reservations.ErrPermanent, c.detail, err)
	default:
		return reservations.Recorded{}, fmt.Errorf("%w: reserve canceled (%s): %w", reservations.ErrRetryable, c.detail, err)
	}
}

// RecordSoldOut puts r as SOLD_OUT only if no outcome exists for the request. Inventory is not
// touched.
func (l *Ledger) RecordSoldOut(ctx context.Context, r reservations.Reservation) (reservations.Recorded, error) {
	r.Status = reservations.StatusSoldOut
	_, err := l.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                           aws.String(l.tables.Reservations),
		Item:                                reservationItem(r),
		ConditionExpression:                 noOutcomeCondition,
		ExpressionAttributeNames:            noOutcomeNames,
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err == nil {
		return reservations.Recorded{Reservation: r}, nil
	}
	var exists *types.ConditionalCheckFailedException
	if errors.As(err, &exists) {
		return l.existing(ctx, r, exists.Item)
	}
	return reservations.Recorded{}, classifyError("record sold out", err)
}

func (l *Ledger) reserveInput(r reservations.Reservation) *dynamodb.TransactWriteItemsInput {
	items := make([]types.TransactWriteItem, txItems)
	items[txInventory] = types.TransactWriteItem{Update: &types.Update{
		TableName:                           aws.String(l.tables.Inventory),
		Key:                                 map[string]types.AttributeValue{InventoryKeyCardID: &types.AttributeValueMemberS{Value: r.CardID}},
		UpdateExpression:                    aws.String("SET #stock = #stock - :one"),
		ConditionExpression:                 aws.String("#stock >= :one"),
		ExpressionAttributeNames:            stockNames,
		ExpressionAttributeValues:           map[string]types.AttributeValue{":one": one},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	}}
	items[txReservation] = types.TransactWriteItem{Put: &types.Put{
		TableName:                           aws.String(l.tables.Reservations),
		Item:                                reservationItem(r),
		ConditionExpression:                 noOutcomeCondition,
		ExpressionAttributeNames:            noOutcomeNames,
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	}}
	return &dynamodb.TransactWriteItemsInput{TransactItems: items}
}

// existing returns the outcome already stored for r's request. It prefers the item returned by
// the failed condition and falls back to a consistent read.
func (l *Ledger) existing(ctx context.Context, r reservations.Reservation, item map[string]types.AttributeValue) (reservations.Recorded, error) {
	if len(item) == 0 {
		out, err := l.db.GetItem(ctx, &dynamodb.GetItemInput{
			TableName: aws.String(l.tables.Reservations),
			Key: map[string]types.AttributeValue{
				ReservationKeyTrainerID: &types.AttributeValueMemberS{Value: r.TrainerID},
				ReservationKeyRequestID: &types.AttributeValueMemberS{Value: r.RequestID},
			},
			ConsistentRead: aws.Bool(true),
		})
		if err != nil {
			return reservations.Recorded{}, classifyError("read existing outcome", err)
		}
		if len(out.Item) == 0 {
			// The condition said an outcome exists; a consistent read should see it.
			return reservations.Recorded{}, fmt.Errorf("%w: existing outcome for %q not found", reservations.ErrRetryable, r.RequestID)
		}
		item = out.Item
	}
	existing, err := reservationFromItem(item)
	if err != nil {
		return reservations.Recorded{}, fmt.Errorf("%w: stored outcome is unreadable: %w", reservations.ErrPermanent, err)
	}
	return reservations.Recorded{Reservation: *existing, Duplicate: true}, nil
}

func reservationItem(r reservations.Reservation) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		ReservationKeyTrainerID: &types.AttributeValueMemberS{Value: r.TrainerID},
		ReservationKeyRequestID: &types.AttributeValueMemberS{Value: r.RequestID},
		ReservationCardID:       &types.AttributeValueMemberS{Value: r.CardID},
		ReservationStatus:       &types.AttributeValueMemberS{Value: string(r.Status)},
		ReservationUpdatedAt:    &types.AttributeValueMemberS{Value: r.UpdatedAt.UTC().Format(time.RFC3339)},
	}
}

type cancelKind int

const (
	cancelRetryable cancelKind = iota
	cancelDuplicate
	cancelOutOfStock
	cancelCardMissing
	cancelPermanent
)

type cancellation struct {
	kind     cancelKind
	existing map[string]types.AttributeValue // the stored outcome, for cancelDuplicate
	detail   string                          // the reason codes, for logs
}

// Cancellation reason codes (DynamoDB TransactWriteItems API reference).
const (
	reasonNone                   = "None"
	reasonConditionalCheckFailed = "ConditionalCheckFailed"
)

// Reason codes that retry. Unknown codes retry too; this list documents the expected ones.
var retryableReasons = map[string]bool{
	"TransactionConflict":           true,
	"ThrottlingError":               true,
	"ProvisionedThroughputExceeded": true,
	"RequestLimitExceeded":          true,
}

// Reason codes that will fail the same way every time.
var permanentReasons = map[string]bool{
	"ValidationError":                 true,
	"ItemCollectionSizeLimitExceeded": true,
}

// classifyCancellation decides what a canceled reserve transaction means from which condition
// failed. Order matters:
//  1. The reservation condition failed: an outcome already exists, so this is a duplicate,
//     whatever the stock is. A duplicate never becomes SOLD_OUT and never takes stock.
//  2. Only the stock condition failed and the reservation item had no error: out of stock (or
//     the card does not exist, if the condition saw no item).
//  3. Anything else retries, unless a reason is known to be permanent. Retrying is always safe.
func classifyCancellation(reasons []types.CancellationReason) cancellation {
	if len(reasons) != txItems {
		return cancellation{kind: cancelRetryable, detail: fmt.Sprintf("unexpected %d cancellation reasons", len(reasons))}
	}
	inv, res := reasonCode(reasons[txInventory]), reasonCode(reasons[txReservation])
	detail := "inventory=" + inv + " reservation=" + res

	switch {
	case res == reasonConditionalCheckFailed:
		return cancellation{kind: cancelDuplicate, existing: reasons[txReservation].Item, detail: detail}
	case inv == reasonConditionalCheckFailed && res == reasonNone:
		if len(reasons[txInventory].Item) == 0 {
			return cancellation{kind: cancelCardMissing, detail: detail}
		}
		return cancellation{kind: cancelOutOfStock, detail: detail}
	case retryableReasons[inv] || retryableReasons[res]:
		return cancellation{kind: cancelRetryable, detail: detail}
	case permanentReasons[inv] || permanentReasons[res]:
		return cancellation{kind: cancelPermanent, detail: detail}
	default:
		return cancellation{kind: cancelRetryable, detail: detail}
	}
}

// reasonCode returns the reason's code, treating an absent code as None (no error on that item).
func reasonCode(r types.CancellationReason) string {
	if c := aws.ToString(r.Code); c != "" {
		return c
	}
	return reasonNone
}

// Errors that mean the request itself or the deployment is wrong; retrying the message cannot help.
var permanentErrorCodes = map[string]bool{
	"ValidationException":       true,
	"ResourceNotFoundException": true,
	"AccessDeniedException":     true,
}

// classifyError wraps an error that is not a condition failure as retryable or permanent.
// Throttling, server errors, timeouts and network errors are retryable.
func classifyError(op string, err error) error {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && permanentErrorCodes[apiErr.ErrorCode()] {
		return fmt.Errorf("%w: %s: %w", reservations.ErrPermanent, op, err)
	}
	return fmt.Errorf("%w: %s: %w", reservations.ErrRetryable, op, err)
}
