package def

//roost:dao coll=trade_wallets db=roost_generated_it_placeholder dbscope=global
type WalletDao struct {
	Payload   string `dao:"persist" store:"payload"`
	X         int64  `dao:"nopersist,sync" store:"x"`
	Y         int64  `dao:"nopersist,sync" store:"y"`
	Changed   int64  `dao:"nopersist,sync" store:"changed"`
	Coins     int64  `dao:"persist,sync" store:"coins"`
	Transfers int64  `dao:"persist,sync" store:"transfers"`
}

//roost:dao coll=trade_inventories db=roost_generated_it_placeholder dbscope=global
type InventoryDao struct {
	Payload   string `dao:"persist" store:"payload"`
	Items     int64  `dao:"persist,sync" store:"items"`
	Transfers int64  `dao:"persist,sync" store:"transfers"`
}
