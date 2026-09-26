package migrations

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// v2Player is the shape the new definition reads. Decoding into it is the
// question the step has to answer: not "what keys are in the document" but
// "can the next build read it".
type v2Player struct {
	Name      string `bson:"name"`
	Level     int32  `bson:"level"`
	Equipment struct {
		Slots map[string]struct {
			ItemID int64 `bson:"item_id"`
			Level  int32 `bson:"level"`
		} `bson:"slots"`
	} `bson:"equipment"`
}

// A v1 document becomes a v2 document, and the weapon it was wearing ends up
// in the slot the new definition reads.
func TestAV1PlayerKeepsItsWeapon(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"_id": int64(42), "name": "old", "level": int32(7), "weapon_id": int64(2001)})
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := playerWeaponToEquipment(context.Background(), raw)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Decoded into the shape the new definition expects, which is the only
	// question that matters: the next build reads it with this layout.
	var upgradedPlayer v2Player
	if err := bson.Unmarshal(upgraded, &upgradedPlayer); err != nil {
		t.Fatal(err)
	}
	var loose bson.M
	_ = bson.Unmarshal(upgraded, &loose)
	if _, stillThere := loose["weapon_id"]; stillThere {
		t.Error("the old field survived the upgrade; the next build would carry a field nothing reads")
	}
	piece, worn := upgradedPlayer.Equipment.Slots["1"]
	if !worn {
		t.Fatalf("the weapon did not land in a slot: %+v", upgradedPlayer)
	}
	if piece.ItemID != 2001 {
		t.Errorf("the weapon became item %d, want 2001", piece.ItemID)
	}
	// Everything else is untouched: a step that rewrites what it did not
	// come for is a step that loses data nobody expected it to.
	if upgradedPlayer.Name != "old" || upgradedPlayer.Level != 7 {
		t.Errorf("the step disturbed unrelated fields: %+v", upgradedPlayer)
	}
}

// A v1 player who wore nothing is still a v1 document and still has to come
// out as v2.
func TestAV1PlayerWithNoWeaponStillUpgrades(t *testing.T) {
	raw, _ := bson.Marshal(bson.M{"_id": int64(43), "name": "bare"})
	upgraded, err := playerWeaponToEquipment(context.Background(), raw)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var document bson.M
	if err := bson.Unmarshal(upgraded, &document); err != nil {
		t.Fatal(err)
	}
	if _, present := document["equipment"]; present {
		t.Error("a player who wore nothing was given an equipment sub-document; the zero value covers that")
	}
	if document["name"] != "bare" {
		t.Error("the step disturbed a document it had nothing to do")
	}
}

// The stored number's Go type depends on the driver and the build that wrote
// it. A step that understands one spelling silently skips the documents it
// exists for.
func TestTheStepAcceptsEveryNumericSpellingOfTheOldField(t *testing.T) {
	for name, weapon := range map[string]any{
		"int64":   int64(2001),
		"int32":   int32(2001),
		"float64": float64(2001),
	} {
		raw, _ := bson.Marshal(bson.M{"_id": int64(44), "weapon_id": weapon})
		upgraded, err := playerWeaponToEquipment(context.Background(), raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var upgradedPlayer v2Player
		if err := bson.Unmarshal(upgraded, &upgradedPlayer); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		piece, worn := upgradedPlayer.Equipment.Slots["1"]
		if !worn {
			t.Errorf("%s: the weapon was dropped", name)
			continue
		}
		if piece.ItemID != 2001 {
			t.Errorf("%s: item id came out as %d", name, piece.ItemID)
		}
	}
}
