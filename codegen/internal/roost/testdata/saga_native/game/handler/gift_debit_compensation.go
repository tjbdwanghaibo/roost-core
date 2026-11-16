package handler

import (
	"example.com/planet/game/view"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftDebitCompensation is the gift saga's refund: give the debit back (never refused).
//
//roost:nest rollback=undo durability=pipelined
func handlerGiftDebitCompensation(target view.IWalletEntity, amount int64, step saga.NativeStep) error {
	target.Wallet().Refund(amount)
	return step.Complete(true, "")
}
