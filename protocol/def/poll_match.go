//go:build protocoldef

package protocoldef

// PollMatchRequest reads the caller's own ticket; the match service checks
// ownership, so a client cannot read another player's ticket.
type PollMatchRequest struct {
	TicketID string `pb:"1"`
	// Mode names the queue the ticket is in ("duel" or "ranked"; empty means
	// duel) — a ticket is scoped to its queue in the match service.
	Mode string `pb:"2"`
}

// PollMatchResponse: State is waiting / matched / cancelled / expired; when
// matched, MatchID and the other members are set.
type PollMatchResponse struct {
	Code    int32   `pb:"1"`
	Reason  string  `pb:"2"`
	State   string  `pb:"3"`
	MatchID string  `pb:"4"`
	Members []int64 `pb:"5"`
}

//roost:protocol group=game handler=player
type PollMatchProtocol interface {
	//roost:msg id=10004
	PollMatch(PollMatchRequest) PollMatchResponse
}
