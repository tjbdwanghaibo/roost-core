//go:build protocoldef

package protocoldef

// ActivityStandingRequest asks where this player stands in the current
// server-wide event. It has no fields: which window is current is the
// server's to decide, and a client that named one could ask about a window
// the server is not running.
type ActivityStandingRequest struct{}

// ActivityStandingResponse is the window and this player's place in it.
type ActivityStandingResponse struct {
	Code       int32  `pb:"1"`
	Reason     string `pb:"2"`
	ActivityID string `pb:"3"`
	// Status is the coordinator's: pending until a server reports the phase,
	// collecting while the rest do, complete once the set is in or the grace
	// window closed.
	Status string `pb:"4"`
	// ClosesAtUnix is when this window's close phase falls due.
	ClosesAtUnix int64 `pb:"5"`
	Score        int64 `pb:"6"`
	Progress     int64 `pb:"7"`
	// Settled is this server's own record: the result arrived and the
	// rewards were paid.
	Settled bool `pb:"8"`
}

//roost:protocol group=game handler=player
type ActivityStandingProtocol interface {
	//roost:msg id=10020
	ActivityStanding(ActivityStandingRequest) ActivityStandingResponse
}
