package def

//roost:dao coll=remote_balances db=roost_remote_generated_placeholder dbscope=global
type BalanceDao struct {
	Value int64 `dao:"persist,sync"`
}

//roost:dao coll=remote_items db=roost_remote_generated_placeholder dbscope=global
type ItemsDao struct {
	Value int64 `dao:"persist,sync"`
}
