package view

// Wallet is what the gift saga's handlers call on the wallet component
// (game/components/wallet/wallet_logic.go): the scaffold from roost add component
// with the business methods added.
type Wallet interface {
	Gold() int64
	Spend(amount int64) error
	Receive(amount int64) error
	Refund(amount int64)
	LevelUp() int32
}

// WalletHolder is "has a Wallet component"; roost generate implements Wallet()
// on the Player and writes view.IWalletEntity.
type WalletHolder interface{ Wallet() Wallet }
