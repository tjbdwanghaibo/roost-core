package dbdef

// PlayerDao: the saga e2e's wallet. db= is replaced with a unique name per run.
//
//roost:dao coll=player db=SAGAGEN_DB
type PlayerDao struct {
	Gold  int64 `bson:"gold" dao:"persist"`
	Level int32 `bson:"level" dao:"persist"`
}
