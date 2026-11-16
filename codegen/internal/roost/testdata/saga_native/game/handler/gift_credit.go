package handler

import (
	"example.com/planet/game/view"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// handlerGiftCredit is the gift saga's credit: put the gift in the recipient's wallet; a full wallet refuses.
//
//roost:nest rollback=undo durability=pipelined
func handlerGiftCredit(target view.IWalletEntity, amount int64, step saga.NativeStep) error {
	if err := target.Wallet().Receive(amount); err != nil {
		return step.Complete(false, err.Error())
	}
	return step.Complete(true, "")
}
