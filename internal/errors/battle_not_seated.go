package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrBattleNotSeated: the player holds no seat in the battle they addressed.
var ErrBattleNotSeated = errcode.Define(100008, "battle_not_seated", "not seated in this battle")
