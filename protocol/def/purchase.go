//go:build protocoldef

package protocoldef

// PurchaseRequest buys one catalogue product.
//
// It carries no price and no player id. Both are the server's to decide: a
// client that names its own amount names its own discount, and a client that
// names its own player id buys for someone else. The only thing it chooses is
// which product.
type PurchaseRequest struct {
	ProductID string `pb:"1"`
}

// PurchaseResponse reports the order and what reached the bag.
//
// Delivered and Replayed come from the platform service's receipt: Replayed
// is true when this exact purchase had already been recorded, which is the
// truthful answer to a client that retried rather than a second order.
type PurchaseResponse struct {
	Code      int32  `pb:"1"`
	Reason    string `pb:"2"`
	OrderID   string `pb:"3"`
	ItemID    int64  `pb:"4"`
	Count     int32  `pb:"5"`
	BagCount  int32  `pb:"6"`
	Delivered bool   `pb:"7"`
	Replayed  bool   `pb:"8"`
}

//roost:protocol group=game handler=player
type PurchaseProtocol interface {
	//roost:msg id=10019
	Purchase(PurchaseRequest) PurchaseResponse
}
