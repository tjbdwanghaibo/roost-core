//go:build protocoldef

package protocoldef

// JoinQueueRequest asks to wait for a match. Mode selects the queue: "duel"
// (arrival order; also the default) or "ranked" (by level, widening window);
// anything else is a coded refusal.
type JoinQueueRequest struct {
	Mode string `pb:"1"`
}

// JoinQueueResponse carries the ticket the match service minted. A repeat of
// the same frame (same sequence) returns the same ticket: the endpoint uses
// the frame sequence as the enqueue idempotency key.
type JoinQueueResponse struct {
	Code     int32  `pb:"1"`
	Reason   string `pb:"2"`
	TicketID string `pb:"3"`
	State    string `pb:"4"`
}

//roost:protocol group=game handler=player
type JoinQueueProtocol interface {
	//roost:msg id=10003
	JoinQueue(JoinQueueRequest) JoinQueueResponse
}
