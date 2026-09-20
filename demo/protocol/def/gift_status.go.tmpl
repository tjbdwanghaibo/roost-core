//go:build protocoldef

package protocoldef

// GiftStatusRequest asks how a gift saga is doing.
type GiftStatusRequest struct {
	SagaID string `pb:"1"`
}

// GiftStatusResponse is the saga record as the sender may see it. Status is
// the coordinator's word: pending, waiting, compensating, completed,
// compensated, failed, manual_required — or "unknown" while the start
// intent is still on its way from the outbox to the coordinator (the saga
// starts asynchronously, so a poll right after SendGift can land first) and
// for a saga that is not this player's. Step is the index of the step the
// coordinator is on; LastError is why a step refused, when one did.
type GiftStatusResponse struct {
	Code      int32  `pb:"1"`
	Reason    string `pb:"2"`
	Status    string `pb:"3"`
	Step      int32  `pb:"4"`
	LastError string `pb:"5"`
}

//roost:protocol group=game handler=player
type GiftStatusProtocol interface {
	//roost:msg id=10014
	GiftStatus(GiftStatusRequest) GiftStatusResponse
}
