package def

//roost:dao coll=states db=roost_generated_it_placeholder dbscope=global
type StateDao struct {
	Value int64 `dao:"persist"`
}
