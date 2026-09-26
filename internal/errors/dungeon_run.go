package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrDungeonRun: the request names no run, or names one this call cannot pay for.
var ErrDungeonRun = errcode.Define(100010, "dungeon_run", "invalid dungeon run")
