// Command seed loads the three fictional launch cards into the inventory table of a deployed
// stack. Table names come from the stack outputs, so nothing is hard-coded.
//
// Seeding is idempotent: each card is written unconditionally, so re-running resets stock to
// the fixture values. It never touches the reservations table, so reseeding after real
// reservations leaves stock inconsistent with them. It is meant for dev stacks.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

const (
	region               = "us-west-2"
	inventoryOutputKey   = "InventoryTableName"
	seedStockMax         = 1000
	expiredTokenGuidance = "AWS credentials have expired: run `aws login --profile pokedrop` and retry"
)

//go:embed cards.json
var cardsJSON []byte

type seedCard struct {
	CardID string `json:"cardId"`
	Name   string `json:"name"`
	Rarity string `json:"rarity"`
	Stock  int    `json:"stock"`
}

func parseCards(data []byte) ([]seedCard, error) {
	var cards []seedCard
	if err := json.Unmarshal(data, &cards); err != nil {
		return nil, fmt.Errorf("parse cards: %w", err)
	}
	seen := make(map[string]bool, len(cards))
	for _, c := range cards {
		switch {
		case c.CardID == "" || c.Name == "" || c.Rarity == "":
			return nil, fmt.Errorf("card %+v: cardId, name and rarity are required", c)
		case seen[c.CardID]:
			return nil, fmt.Errorf("duplicate cardId %q", c.CardID)
		case c.Stock < 0 || c.Stock > seedStockMax:
			return nil, fmt.Errorf("card %q: stock %d out of range [0, %d]", c.CardID, c.Stock, seedStockMax)
		}
		seen[c.CardID] = true
	}
	return cards, nil
}

// noStockOverride is the -stock default: keep the stock from cards.json.
const noStockOverride = -1

// withStock returns a copy of cards with every card's stock set to stock. noStockOverride returns
// the cards unchanged. Load tests use it to size a drop (for example 100 cards for 200 requests)
// without editing the fixture.
func withStock(cards []seedCard, stock int) ([]seedCard, error) {
	if stock == noStockOverride {
		return cards, nil
	}
	if stock < 0 || stock > seedStockMax {
		return nil, fmt.Errorf("stock %d out of range [0, %d]", stock, seedStockMax)
	}
	out := make([]seedCard, len(cards))
	for i, c := range cards {
		c.Stock = stock
		out[i] = c
	}
	return out, nil
}

func inventoryItem(c seedCard) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		storage.InventoryKeyCardID: &types.AttributeValueMemberS{Value: c.CardID},
		storage.InventoryName:      &types.AttributeValueMemberS{Value: c.Name},
		storage.InventoryRarity:    &types.AttributeValueMemberS{Value: c.Rarity},
		storage.InventoryStock:     &types.AttributeValueMemberN{Value: strconv.Itoa(c.Stock)},
	}
}

func main() {
	stage := flag.String("stage", "dev", "stack stage to seed (stack name: pokedrop-<stage>)")
	stock := flag.Int("stock", noStockOverride, "set every card's stock to this value instead of the cards.json values")
	flag.Parse()

	if err := run(context.Background(), *stage, *stock); err != nil {
		if strings.Contains(err.Error(), "ExpiredToken") {
			fmt.Fprintln(os.Stderr, expiredTokenGuidance)
		}
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, stage string, stock int) error {
	cards, err := parseCards(cardsJSON)
	if err != nil {
		return err
	}
	if cards, err = withStock(cards, stock); err != nil {
		return err
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	stackName := "pokedrop-" + stage
	table, err := inventoryTableName(ctx, cloudformation.NewFromConfig(cfg), stackName)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "WARNING: resetting stock in %s to seed values; reservations are not touched\n", table)
	db := dynamodb.NewFromConfig(cfg)
	for _, c := range cards {
		if _, err := db.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(table),
			Item:      inventoryItem(c),
		}); err != nil {
			return fmt.Errorf("put %s: %w", c.CardID, err)
		}
		fmt.Printf("seeded %s %q (%s) stock=%d\n", c.CardID, c.Name, c.Rarity, c.Stock)
	}
	return nil
}

func inventoryTableName(ctx context.Context, cf *cloudformation.Client, stackName string) (string, error) {
	out, err := cf.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return "", fmt.Errorf("describe stack %s (is it deployed? try `make deploy`): %w", stackName, err)
	}
	if len(out.Stacks) == 0 {
		return "", errors.New("stack " + stackName + " not found")
	}
	for _, o := range out.Stacks[0].Outputs {
		if aws.ToString(o.OutputKey) == inventoryOutputKey && aws.ToString(o.OutputValue) != "" {
			return aws.ToString(o.OutputValue), nil
		}
	}
	return "", fmt.Errorf("stack %s has no %s output", stackName, inventoryOutputKey)
}
