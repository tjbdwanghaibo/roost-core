package def

// WraithDao is the nocoll shape: an in-memory DAO for an entity that is never
// stored (W-2026-09-18-09). Every field is nopersist; the golden locks that it
// keeps sync, accessors and rollback and has no collection or storage path.
//
//roost:dao nocoll
type WraithDao struct {
	PosX    int64           `dao:"nopersist,sync" store:"pos_x"`
	PosY    int64           `dao:"nopersist,sync" store:"pos_y"`
	Buffs   map[int32]int64 `dao:"nopersist,sync" store:"buffs"`
	Trail   []int64         `dao:"nopersist,sync" store:"trail"`
	Scratch int64           `dao:"nopersist,nosync" store:"scratch"`
}
