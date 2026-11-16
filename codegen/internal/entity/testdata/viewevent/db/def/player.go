package dbdef

type BagState struct {
	Items map[int64]int64 `store:"1"`
}
type WalletState struct {
	Gold int64 `store:"1"`
}
type WalletLedger struct {
	Keys map[string]int64 `store:"1"`
}
type AttributeValues struct {
	HP int64 `store:"1"`
}
type ActivityBook struct {
	Open map[int64]bool `store:"1"`
}
type ProfileState struct {
	Name string `store:"1"`
}

//roost:dao coll=player db=main dbscope=sid
type PlayerDao struct {
	Bag          BagState     `store:"bag" dao:"persist,sync"`
	Wallet       WalletState  `store:"wallet" dao:"persist,sync"`
	WalletLedger WalletLedger `store:"wallet_ledger" dao:"nosync"`
	// AttributeBase / AttributeFinal 按方法名对上，Gear 按“组件名 + 字段名”对上：三个字段同类型，不能按类型找。
	AttributeBase  AttributeValues `store:"attribute_base" dao:"persist,sync"`
	Gear           AttributeValues `store:"attribute_gear" dao:"nopersist,nosync"`
	AttributeFinal AttributeValues `store:"attribute_final" dao:"nopersist,sync"`
	Activity       ActivityBook    `store:"activity" dao:"persist,nosync"`
	Profile        *ProfileState   `store:"profile" dao:"persist,sync"`
	Level          int32           `store:"level" dao:"persist,sync"`
	Hidden         int64           `store:"hidden" dao:"-"`
}
