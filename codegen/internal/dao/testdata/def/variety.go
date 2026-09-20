package def

// VarietyDao exercises the explicit tag intents and every map mode so the
// golden files lock those template branches too.
//
//roost:dao coll=varieties db=game
type VarietyDao struct {
	PersistOnly int64            `dao:"persist"`
	SyncOnly    int64            `dao:"nopersist,sync"`
	Neither     int64            `dao:"nopersist,nosync"`
	FastItems   map[int64]int32  `dao:"persist,sync,map=fast"`
	ShardedTags map[int32]string `dao:"persist,sync,map=sharded"`
}

//roost:redisdao mode=raw key=RunID prefix=roost:test:raw
type RawSession struct {
	RunID int64
	State int32
}
