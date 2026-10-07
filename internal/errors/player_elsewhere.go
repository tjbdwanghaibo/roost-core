package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrPlayerElsewhere: this player is bound to another game server, so this one
// may not serve them. A player is bound to one sid when the role is created
// (account Role.ServerID) and never moves, so this is not a transient state:
// do NOT retry on this server. Reconnect to the gateway of the sid named in
// the refusal (owner_sid); when owner_sid is 0 the server could not tell where
// the player belongs — select the role again and connect to the
// Session.ServerID it returns.
var ErrPlayerElsewhere = errcode.Define(100015, "player_elsewhere", "player is bound to another server; reconnect to that server")
