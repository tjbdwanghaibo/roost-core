package mongo

type IndexConflictPolicy string

const (
	IndexConflictFail     IndexConflictPolicy = "fail"
	IndexConflictRecreate IndexConflictPolicy = "recreate"
)

// IndexModel defines a collection index.
type IndexModel struct {
	Keys               any    // index key document (e.g. bson.D{{"player_id", 1}})
	Name               string // optional index name (auto-generated if empty)
	Unique             bool
	Sparse             bool
	TTL                int32 // relative TTL in seconds (0 = no relative expiry)
	ExpireAt           bool  // expire at the indexed date itself (Mongo expireAfterSeconds=0)
	ConflictPolicy     IndexConflictPolicy
	RecreateOnConflict bool // drop and recreate this named index when its stored definition differs
}
