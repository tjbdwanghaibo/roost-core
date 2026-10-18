package handler

import (
	player "example.com/planet/game/entities/player"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftCreditCompensation is the gift saga's take the credit back.
//
//roost:nest rollback=undo durability=strict
func handlerGiftCreditCompensation(target player.IWalletEntity, amount int64, step saga.NativeStep) error {
	if err := target.WalletComp().Spend(amount); err != nil {
		return step.Complete(false, err.Error())
	}
	return step.Complete(true, "")
}
