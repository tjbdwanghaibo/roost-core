package dbdef

// PlayerDao: the saga e2e's wallet. db=main is the logical database; the
// test maps it to a unique physical database per run (dataengine.databases).
//
//roost:dao coll=player db=main
type PlayerDao struct {
	Gold  int64 `bson:"gold" dao:"persist"`
	Level int32 `bson:"level" dao:"persist"`
}
