package def

// WraithDao is the nocoll shape: an in-memory DAO for an entity that is never
// stored (W-2026-09-18-09). Every field is nopersist; the golden locks that it
// keeps sync, accessors and rollback and has no collection or storage path.
//
//roost:dao nocoll
type WraithDao struct {
	PosX    int64           `bson:"pos_x" dao:"nopersist,sync"`
	PosY    int64           `bson:"pos_y" dao:"nopersist,sync"`
	Buffs   map[int32]int64 `bson:"buffs" dao:"nopersist,sync"`
	Trail   []int64         `bson:"trail" dao:"nopersist,sync"`
	Scratch int64           `dao:"nopersist,nosync"`
}
