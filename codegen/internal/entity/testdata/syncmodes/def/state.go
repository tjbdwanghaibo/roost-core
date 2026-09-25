package def

//roost:dao coll=states db=game dbscope=sid
type StateDao struct {
	Value int64 `dao:"sync"`
}
