package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrGuildMissing: there is no guild with that id. It is a coded refusal
// rather than an empty answer because "you joined nothing" and "the guild is
// gone" are different things to a player.
var ErrGuildMissing = errcode.Define(100018, "guild_missing", "guild not found")
