// Package migrations holds this project's stored-document upgrades.
//
// A DAO's `schema=` version and this package are the two halves of one fact:
// the definition says what the code expects, and a step here says how a
// document written by an older build becomes that. The generated `Migrate`
// runs the chain on load, so a document is upgraded once, in memory, on its
// way into the Entity — nothing rewrites the collection in bulk.
//
// The rule worth stating: a step may only READ what the old version actually
// had. It is running against a document written by a build that no longer
// exists, so "I know the field is there" is only true if the old definition
// said so.
package migrations

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/migration"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Register installs every step this project owns. It is called once at
// startup, before anything can load a document.
//
// Registration is deliberately explicit rather than an init(): a migration
// that runs because a package happened to be linked in is a migration nobody
// decided to run.
func Register() error {
	return migration.RegisterDAO(migration.DAOStep{
		Name:       "player_weapon_to_equipment",
		Collection: "player",
		From:       1,
		To:         2,
		Apply:      playerWeaponToEquipment,
	})
}

// playerWeaponToEquipment moves the v1 flat weapon id into the v2 equipment
// slots.
//
// This is the kind of change a zero value cannot cover. Adding a field needs
// no step at all — BSON decode gives it the zero value and the game carries
// on. What needs a step is a change of SHAPE: v1 stored `weapon_id` as a
// number on the Player, v2 stores an `equipment` sub-document keyed by slot,
// and nothing in the new definition can find the old number.
func playerWeaponToEquipment(_ context.Context, raw []byte) ([]byte, error) {
	var document bson.M
	if err := bson.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("player 1->2: decode: %w", err)
	}
	// A document that never had a weapon is still a v1 document and still
	// has to come out as v2 — the step is about the SHAPE, not about whether
	// this particular player owned anything.
	weapon, worn := document["weapon_id"]
	delete(document, "weapon_id")
	if worn {
		if itemID := toInt64(weapon); itemID > 0 {
			document["equipment"] = bson.M{
				"slots": bson.M{
					// Slot 1 is the weapon slot; game/equipment names it.
					"1": bson.M{"item_id": itemID, "level": int32(1)},
				},
			}
		}
	}
	upgraded, err := bson.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("player 1->2: encode: %w", err)
	}
	return upgraded, nil
}

// toInt64 accepts whatever numeric type the old document happened to use.
// A stored number's Go type depends on the driver and the build that wrote
// it, so a step that type-asserts one spelling fails on documents it should
// have upgraded.
func toInt64(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int32:
		return int64(typed)
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}
	return 0
}
