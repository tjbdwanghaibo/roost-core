//go:build protocoldef

package protocoldef

// SendChatRequest publishes one line of text. Kind is "world" or "private";
// Target is the world id for world (the demo has world 1) and the peer's
// player id for private. The frame sequence is the idempotency key, so a
// retried frame does not store the line twice.
type SendChatRequest struct {
	Kind   string `pb:"1"`
	Target int64  `pb:"2"`
	Text   string `pb:"3"`
}

// SendChatResponse carries the per-channel sequence the chat service gave the
// message; history pages by it.
type SendChatResponse struct {
	Code   int32  `pb:"1"`
	Reason string `pb:"2"`
	Seq    int64  `pb:"3"`
}

//roost:protocol group=game handler=player
type SendChatProtocol interface {
	//roost:msg id=10006
	SendChat(SendChatRequest) SendChatResponse
}
