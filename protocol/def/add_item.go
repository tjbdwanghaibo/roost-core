//go:build protocoldef

package protocoldef

// AddItemRequest carries the demo's one gameplay request. Field names must
// match the Nest handler's parameters (itemID, count): the endpoint generator
// refuses to wire a message whose fields do not, so protocol and handler
// cannot drift apart silently.
type AddItemRequest struct {
	ItemID int64 `pb:"1"`
	Count  int32 `pb:"2"`
}

// AddItemResponse carries either the new amount or a coded failure. Code 0 is
// success; other codes are the stable values from internal/errors, and Reason
// is their client-facing message. Nothing else about a failure crosses here.
type AddItemResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	Count  int32  `pb:"3"`
}

//roost:protocol group=game handler=player
type AddItemProtocol interface {
	//roost:msg id=10000
	AddItem(AddItemRequest) AddItemResponse
}
