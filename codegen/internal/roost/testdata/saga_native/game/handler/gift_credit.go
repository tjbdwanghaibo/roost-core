package handler

import (
	player "example.com/planet/game/entities/player"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftCredit is the gift saga's credit: put the gift in the recipient's wallet; a full wallet refuses.
//
//roost:nest rollback=undo durability=strict
func handlerGiftCredit(target player.IWalletEntity, amount int64, step saga.NativeStep) error {
	if err := target.WalletComp().Receive(amount); err != nil {
		return step.Complete(false, err.Error())
	}
	return step.Complete(true, "")
}
