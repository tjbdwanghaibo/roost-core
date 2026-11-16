package handler

import (
	"example.com/planet/game/view"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftCreditCompensation is the gift saga's take the credit back.
//
//roost:nest rollback=undo durability=pipelined
func handlerGiftCreditCompensation(target view.IWalletEntity, amount int64, step saga.NativeStep) error {
	if err := target.Wallet().Spend(amount); err != nil {
		return step.Complete(false, err.Error())
	}
	return step.Complete(true, "")
}
