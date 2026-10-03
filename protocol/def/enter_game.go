//go:build protocoldef

package protocoldef

// EnterGameRequest is the first message after the handshake. It is not a Nest
// handler's request — creating a Player is a lifecycle operation, done by the
// endpoint through PlayerLifecycle, not inside an entity lock — so this
// message has no `roost add endpoint` behind it; the controller method is
// hand-written.
type EnterGameRequest struct {
	ClientVersion string `pb:"1"`
}

// EnterGameResponse: Created says whether this login made the Player.
type EnterGameResponse struct {
	Code     int32  `pb:"1"`
	Reason   string `pb:"2"`
	PlayerID int64  `pb:"3"`
	Created  bool   `pb:"4"`
}

//roost:protocol group=game handler=player
type EnterGameProtocol interface {
	//roost:msg id=10002
	EnterGame(EnterGameRequest) EnterGameResponse
}
