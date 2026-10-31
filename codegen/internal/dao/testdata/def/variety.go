package def

// VarietyDao exercises the explicit tag intents and every map mode so the
// golden files lock those template branches too.
//
//roost:dao coll=varieties db=game
type VarietyDao struct {
	PersistOnly int64 `dao:"persist"`
	SyncOnly    int64 `dao:"nopersist,sync"`
	Neither     int64 `dao:"nopersist,nosync"`
	// Pending is transaction-only state in a map: no storage, no sync, but it
	// has mutators and rolls back with the transaction (A1).
	Pending     map[int32]int64  `dao:"nopersist,nosync,map=fast"`
	FastItems   map[int64]int32  `dao:"persist,sync,map=fast"`
	ShardedTags map[int32]string `dao:"persist,sync,map=sharded"`
	// ResultJSON is an opaque byte payload (a serialized JSON document): the
	// slice API plus the whole-value copy getter GetResultJSONAll, stored as
	// BSON binary.
	ResultJSON []byte
	// Blobs is a map whose values are byte slices (the shape of the saga
	// receipts SagaReceipts): GetBlobs / RangeBlobs hand out copies and
	// SetBlobs stores a copy, so no caller holds the DAO's own bytes.
	Blobs map[string][]byte
	// Envelope carries the same shape inside a nested struct.
	Envelope Envelope
	Enabled  bool   `dao:"nopersist,sync"`
	Counter  uint64 `dao:"nopersist,sync"`
}

// Envelope locks the nested map-of-bytes shape: GetMarks / RangeMarks hand
// out copies and SetMarks stores copies of the values.
type Envelope struct {
	Marks map[int32][]byte
}

//roost:redisdao mode=raw key=RunID prefix=roost:test:raw
type RawSession struct {
	RunID int64
	State int32
}
