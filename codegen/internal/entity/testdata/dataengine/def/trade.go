package def

//roost:dao coll=trade_wallets db=roost_generated_it_placeholder dbscope=global
type WalletDao struct {
	Coins     int64 `dao:"persist"`
	Transfers int64 `dao:"persist"`
}

//roost:dao coll=trade_inventories db=roost_generated_it_placeholder dbscope=global
type InventoryDao struct {
	Items     int64 `dao:"persist"`
	Transfers int64 `dao:"persist"`
}
