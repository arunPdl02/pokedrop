package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/arunPdl02/pokedrop/backend/internal/reservations"
)

// dynamoAPI is the slice of the DynamoDB client the readers use.
type dynamoAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Scan(context.Context, *dynamodb.ScanInput, ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// Store reads the catalog and reservation outcomes. It implements reservations.Catalog and
// reservations.Outcomes. It only reads: the worker (Phase 3) is the sole writer of outcomes.
type Store struct {
	db     dynamoAPI
	tables TableNames
}

// NewStore returns a Store over db using the given table names.
func NewStore(db dynamoAPI, tables TableNames) *Store {
	return &Store{db: db, tables: tables}
}

var (
	_ reservations.Catalog  = (*Store)(nil)
	_ reservations.Outcomes = (*Store)(nil)
)

// Cards scans the inventory table. The catalog is tiny (a handful of cards), so a scan is fine.
func (s *Store) Cards(ctx context.Context) ([]reservations.Card, error) {
	var cards []reservations.Card
	var startKey map[string]types.AttributeValue
	for {
		out, err := s.db.Scan(ctx, &dynamodb.ScanInput{
			TableName:         aws.String(s.tables.Inventory),
			ExclusiveStartKey: startKey,
		})
		if err != nil {
			return nil, fmt.Errorf("scan inventory: %w", err)
		}
		for _, item := range out.Items {
			card, err := cardFromItem(item)
			if err != nil {
				return nil, err
			}
			cards = append(cards, card)
		}
		if len(out.LastEvaluatedKey) == 0 {
			return cards, nil
		}
		startKey = out.LastEvaluatedKey
	}
}

// CardExists reports whether the card is in the inventory table, whatever its stock.
func (s *Store) CardExists(ctx context.Context, cardID string) (bool, error) {
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:            aws.String(s.tables.Inventory),
		Key:                  map[string]types.AttributeValue{InventoryKeyCardID: &types.AttributeValueMemberS{Value: cardID}},
		ProjectionExpression: aws.String(InventoryKeyCardID),
	})
	if err != nil {
		return false, fmt.Errorf("get card %q: %w", cardID, err)
	}
	return len(out.Item) > 0, nil
}

// Get returns the outcome recorded for (trainerID, requestID), or (nil, nil). Both parts of the
// key are required, which is what makes cross-trainer reads impossible.
func (s *Store) Get(ctx context.Context, trainerID, requestID string) (*reservations.Reservation, error) {
	if trainerID == "" || requestID == "" {
		return nil, errors.New("trainerID and requestID are required")
	}
	out, err := s.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tables.Reservations),
		Key: map[string]types.AttributeValue{
			ReservationKeyTrainerID: &types.AttributeValueMemberS{Value: trainerID},
			ReservationKeyRequestID: &types.AttributeValueMemberS{Value: requestID},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("get reservation: %w", err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	return reservationFromItem(out.Item)
}

func cardFromItem(item map[string]types.AttributeValue) (reservations.Card, error) {
	id, err := stringAttr(item, InventoryKeyCardID)
	if err != nil {
		return reservations.Card{}, err
	}
	name, err := stringAttr(item, InventoryName)
	if err != nil {
		return reservations.Card{}, err
	}
	rarity, err := stringAttr(item, InventoryRarity)
	if err != nil {
		return reservations.Card{}, err
	}
	stockStr, err := numberAttr(item, InventoryStock)
	if err != nil {
		return reservations.Card{}, err
	}
	stock, err := strconv.Atoi(stockStr)
	if err != nil {
		return reservations.Card{}, fmt.Errorf("card %q: stock %q is not an integer", id, stockStr)
	}
	// Stock is never negative by invariant; never advertise a negative number even if it were.
	if stock < 0 {
		stock = 0
	}
	return reservations.Card{CardID: id, Name: name, Rarity: rarity, Available: stock}, nil
}

func reservationFromItem(item map[string]types.AttributeValue) (*reservations.Reservation, error) {
	trainer, err := stringAttr(item, ReservationKeyTrainerID)
	if err != nil {
		return nil, err
	}
	request, err := stringAttr(item, ReservationKeyRequestID)
	if err != nil {
		return nil, err
	}
	card, err := stringAttr(item, ReservationCardID)
	if err != nil {
		return nil, err
	}
	status, err := stringAttr(item, ReservationStatus)
	if err != nil {
		return nil, err
	}
	updated, err := stringAttr(item, ReservationUpdatedAt)
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		return nil, fmt.Errorf("reservation %q: updatedAt %q is not RFC 3339", request, updated)
	}
	return &reservations.Reservation{
		RequestID: request,
		TrainerID: trainer,
		CardID:    card,
		Status:    reservations.Status(status),
		UpdatedAt: at.UTC(),
	}, nil
}

func stringAttr(item map[string]types.AttributeValue, name string) (string, error) {
	v, ok := item[name].(*types.AttributeValueMemberS)
	if !ok || v.Value == "" {
		return "", fmt.Errorf("attribute %q is missing or not a non-empty string", name)
	}
	return v.Value, nil
}

func numberAttr(item map[string]types.AttributeValue, name string) (string, error) {
	v, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return "", fmt.Errorf("attribute %q is missing or not a number", name)
	}
	return v.Value, nil
}
