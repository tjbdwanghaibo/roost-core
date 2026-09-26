package def

//roost:dao coll=trade_wallets db=roost_generated_it_placeholder dbscope=global
type WalletDao struct {
	Payload   string `dao:"persist"`
	X         int64  `dao:"nopersist,sync"`
	Y         int64  `dao:"nopersist,sync"`
	Changed   int64  `dao:"nopersist,sync"`
	Coins     int64  `dao:"persist,sync"`
	Transfers int64  `dao:"persist,sync"`
}

//roost:dao coll=trade_inventories db=roost_generated_it_placeholder dbscope=global
type InventoryDao struct {
	Payload   string `dao:"persist"`
	Items     int64  `dao:"persist,sync"`
	Transfers int64  `dao:"persist,sync"`
}
