//go:build protocoldef

package protocoldef

// EquipRequest asks to wear an item the player already holds. The slot is not
// in the request: which slot an item goes in is a property of the item, and
// letting a client choose would let it wear a sword as armour.
type EquipRequest struct {
	ItemID int64 `pb:"1"`
}

type EquipResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	Slot   int32  `pb:"3"`
	// Replaced is what came off and went back in the bag, 0 when the slot was
	// empty.
	Replaced int64 `pb:"4"`
}

//roost:protocol group=game handler=player
type EquipProtocol interface {
	//roost:msg id=10018
	Equip(EquipRequest) EquipResponse
}
