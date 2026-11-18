package handler

import (
	"strconv"

	"example.com/planet/game/modules/effects"
	"example.com/planet/game/view"
)

// handlerLevelUp raises the level and emits player.level_up in the same
// transaction: the notice exists exactly when the level change does.
//
//roost:nest rollback=undo
func handlerLevelUp(target view.IWalletEntity, playerID int64) (int32, error) {
	level := target.Wallet().LevelUp()
	if err := effects.EmitPlayerLevelUp(strconv.FormatInt(playerID, 10), effects.PlayerLevelUp{PlayerID: playerID, Level: level}); err != nil {
		return 0, err
	}
	return level, nil
}
