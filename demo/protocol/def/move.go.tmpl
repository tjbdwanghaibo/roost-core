//go:build protocoldef

package protocoldef

// MoveRequest asks to stand on a point. The demo moves a player one point at
// a time and the server decides whether that point is available; a game with
// continuous movement sends an intent (a direction, a destination) instead
// and lets the server integrate it, which is the same endpoint with a
// different payload.
type MoveRequest struct {
	X int64 `pb:"1"`
	Y int64 `pb:"2"`
}

// MoveResponse carries the position the server settled on. A client renders
// that, not the point it asked for: the two differ whenever the move was
// refused, and a client that renders its own guess is a client that will
// drift.
type MoveResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	X      int64  `pb:"3"`
	Y      int64  `pb:"4"`
}

//roost:protocol group=game handler=player
type MoveProtocol interface {
	//roost:msg id=10017
	Move(MoveRequest) MoveResponse
}
