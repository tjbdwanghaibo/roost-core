package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrPlayerElsewhere: this player's Entity is resident in another game
// process, so this one may not load it. The client reconnects to the gateway
// of the sid named in the refusal, or waits for that process's ownership
// lease to lapse if it is gone.
var ErrPlayerElsewhere = errcode.Define(100015, "player_elsewhere", "player is served by another server")
