package handler

import (
	player "example.com/planet/game/entities/player"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftDebitCompensation is the gift saga's refund: give the debit back (never refused).
//
//roost:nest rollback=undo durability=strict
func handlerGiftDebitCompensation(target player.IWalletEntity, amount int64, step saga.NativeStep) error {
	target.WalletComp().Refund(amount)
	return step.Complete(true, "")
}
