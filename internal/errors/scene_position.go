package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrScenePosition: the position is outside the map, already taken, or the
// player is not on a map at all. One code for all three on purpose — a client
// that learns exactly which points are occupied has been handed a map of
// everyone's positions.
var ErrScenePosition = errcode.Define(100013, "scene_position", "invalid scene position")
