package equipment

// Package equipment is the game's side of what a player wears: which slots
// exist, and what an item has to be to go in one.
//
// The slot ids are here rather than in the DAO because they are a game
// decision — the DAO only knows "a map keyed by int32".

// Slot ids. A slot is a position, not an item: a game adds slots by adding
// constants here and a rule below, never by letting the client name one.
const (
	SlotWeapon int32 = 1
	SlotArmor  int32 = 2
)

// Slots is every slot a player has, in display order.
func Slots() []int32 { return []int32{SlotWeapon, SlotArmor} }

// ValidSlot reports whether a slot exists. The endpoint checks it before the
// transaction: a slot the game does not have is a client error, not a state
// the Player should be asked to hold.
func ValidSlot(slot int32) bool {
	for _, known := range Slots() {
		if known == slot {
			return true
		}
	}
	return false
}

// SlotFor says which slot an item belongs in, and whether it can be worn at
// all. The demo keys it off the item id's range because the item table has no
// "kind" column; a real game reads the column and this function disappears
// into the table.
func SlotFor(itemID int64) (int32, bool) {
	switch {
	case itemID >= 2000 && itemID < 2100:
		return SlotWeapon, true
	case itemID >= 2100 && itemID < 2200:
		return SlotArmor, true
	}
	return 0, false
}

// Result is what the equip transaction reports back.
//
// It lives here rather than next to the handler because the generated sender
// is in its own package and names the return type: a handler's result type
// has to be somewhere both halves can import, which in this demo is always
// the game package the handler is about (see dungeon.ClaimResult).
type Result struct {
	Slot int32
	// Replaced is what came off and went back in the bag, 0 when the slot
	// was empty.
	Replaced int64
}
