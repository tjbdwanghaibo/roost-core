package handler

import (
	"time"

	player "example.com/planet/game/entities/player"
	gift "example.com/planet/saga/gift"
)

// handlerStartGift checks the sender's balance under the lock and emits the
// saga's start intent in the same WAL record.
//
//roost:nest rollback=undo durability=strict
func handlerStartGift(target player.IWalletEntity, from int64, to int64, amount int64, businessKey string) error {
	if target.WalletComp().Gold() < amount {
		return nil
	}
	state, err := gift.EncodeState(gift.State{From: from, To: to, Amount: amount})
	if err != nil {
		return err
	}
	return gift.EmitStart(businessKey, state, time.Now().Add(2*time.Minute)) //glsvet:system-clock a saga deadline is the coordinator's system time
}
