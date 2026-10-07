//go:build protocoldef

package protocoldef

// SendGiftRequest gives count of an item from the sender's Bag to another
// player's mailbox. It starts a saga (debit the Bag, then mail the item);
// the response only says the saga started — GiftStatus tells how it went.
type SendGiftRequest struct {
	ToPlayerID int64 `pb:"1"`
	ItemID     int64 `pb:"2"`
	Count      int32 `pb:"3"`
}

// SendGiftResponse carries the saga id to poll. A repeat of the same frame
// (same sequence) names the same saga.
type SendGiftResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	SagaID string `pb:"3"`
}

//roost:protocol group=game handler=player
type SendGiftProtocol interface {
	//roost:msg id=10013
	SendGift(SendGiftRequest) SendGiftResponse
}
