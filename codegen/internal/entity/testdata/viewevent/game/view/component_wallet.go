package view

import dbx "example.com/g/db"

type Wallet interface{ Gold() int64 }

type WalletHolder interface {
	Wallet() Wallet
	WalletState() *dbx.WalletState
	WalletLedger() *dbx.WalletLedger
	// WalletSpent 是业务钩子，有参数，手写。
	WalletSpent(amount, nowUnixMilli int64) error
}
