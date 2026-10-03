package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrGuildFull: the roster is at its limit. The limit exists because the
// roster is one locked document per write.
var ErrGuildFull = errcode.Define(100019, "guild_full", "guild is full")
