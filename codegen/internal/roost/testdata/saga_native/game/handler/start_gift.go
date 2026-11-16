package handler

import (
	"time"

	"example.com/planet/game/view"
	gift "example.com/planet/saga/gift"
)

// handlerStartGift checks the sender's balance under the lock and emits the
// saga's start intent in the same WAL record.
//
//roost:nest rollback=undo durability=strict
func handlerStartGift(target view.IWalletEntity, from int64, to int64, amount int64, businessKey string) error {
	if target.Wallet().Gold() < amount {
		return nil
	}
	state, err := gift.EncodeState(gift.State{From: from, To: to, Amount: amount})
	if err != nil {
		return err
	}
	return gift.EmitStart(businessKey, state, time.Now().Add(2*time.Minute)) //glsvet:system-clock a saga deadline is the coordinator's system time
}
