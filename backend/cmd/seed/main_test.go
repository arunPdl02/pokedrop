package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/arunPdl02/pokedrop/backend/internal/storage"
)

func TestEmbeddedFixtureIsValid(t *testing.T) {
	cards, err := parseCards(cardsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 3 {
		t.Fatalf("got %d cards, want 3", len(cards))
	}
}

func TestParseCardsRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"not json":        `nope`,
		"missing name":    `[{"cardId":"a","rarity":"rare","stock":1}]`,
		"duplicate id":    `[{"cardId":"a","name":"A","rarity":"r","stock":1},{"cardId":"a","name":"B","rarity":"r","stock":1}]`,
		"negative stock":  `[{"cardId":"a","name":"A","rarity":"r","stock":-1}]`,
		"absurd stock":    `[{"cardId":"a","name":"A","rarity":"r","stock":100000}]`,
		"missing card id": `[{"name":"A","rarity":"r","stock":1}]`,
	}
	for name, in := range cases {
		if _, err := parseCards([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInventoryItemUsesStorageSchema(t *testing.T) {
	item := inventoryItem(seedCard{CardID: "card-001", Name: "Ember Drake", Rarity: "rare", Stock: 10})
	for _, attr := range []string{
		storage.InventoryKeyCardID, storage.InventoryName, storage.InventoryRarity, storage.InventoryStock,
	} {
		if _, ok := item[attr]; !ok {
			t.Errorf("missing attribute %q", attr)
		}
	}
	if len(item) != 4 {
		t.Errorf("got %d attributes, want 4", len(item))
	}
	if n, ok := item[storage.InventoryStock].(*types.AttributeValueMemberN); !ok || n.Value != "10" {
		t.Errorf("stock attribute = %+v, want number 10", item[storage.InventoryStock])
	}
}
