package def

//roost:dao coll=heroes db=game dbscope=sid
type HeroDao struct {
	Name    string               `store:"name"`
	Level   int32                `store:"level"`
	Exp     int64                `store:"exp"`
	LoginAt int64                `dao:"nosync" store:"login_at"`
	Tmp     int                  `dao:"-"`
	Items   map[int64]int32      `store:"items"`
	Friends []int64              `store:"friends"`
	Pos     Position             `store:"pos"`
	Equips  map[int64]*EquipInfo `store:"equips"`
	// Squad is the slice shape of the same ownership question: a top-level
	// container of nested values (RR-20260918-10).
	Squad []*EquipInfo `store:"squad"`
	// Mount is the third shape at the TOP level: one nested pointer in a
	// field of its own. U-0238 fixed the map and the slice and left this one,
	// and it has the same defect (RR-20260919-01) — replacing it never
	// released the old value's notification.
	Mount *EquipInfo `store:"mount"`
}

type Position struct {
	X int32 `store:"x"`
	Y int32 `store:"y"`
}

type EquipInfo struct {
	Level int32              `store:"level"`
	Star  int32              `store:"star"`
	Gems  map[int32]*GemInfo `store:"gems"`
	// Runes and Core lock the other two shapes a nested child can take
	// (slice element, single pointer): notification ownership has to be
	// established and released the same way in all three (RR-20260918-03).
	Runes []*GemInfo `store:"runes"`
	Core  *GemInfo   `store:"core"`
	// Shape is a nested value: it is overwritten in place, so it is bound
	// but never unbound.
	Shape Position `store:"shape"`
}

type GemInfo struct {
	ID    int32 `store:"id"`
	Level int32 `store:"level"`
	// Sig locks the nested []byte shape (GetSigAll returns a copy).
	Sig []byte `store:"sig"`
}
