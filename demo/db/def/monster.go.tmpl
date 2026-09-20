package dbdef

// MonsterDao is a monster's state, and every field is `nopersist,sync`.
//
// A monster is rebuilt by the spawner on every start, so nothing here is
// worth storing — but it IS worth replicating, and giving it a DAO keeps one
// rule true everywhere: **a position lives in a DAO and is read and written
// through a component**. A monster that kept its position in a plain struct
// field would be the one object in the demo whose position has a different
// authority from everyone else's, and the first piece of code that had to
// handle both would get it wrong.
//
// The generated marshaller is the same one the Player uses, so the packer is
// the same shape too: `MarshalSync(mask)` in, bytes out.
//
//roost:dao coll=monsters db=game
type MonsterDao struct {
	// Template is which row of the spawn table this monster came from; a
	// client needs it to know what to draw.
	Template int32 `bson:"template" dao:"nopersist,sync"`
	PosX     int64 `bson:"pos_x" dao:"nopersist,sync"`
	PosY     int64 `bson:"pos_y" dao:"nopersist,sync"`
	HP       int64 `bson:"hp" dao:"nopersist,sync"`
}
