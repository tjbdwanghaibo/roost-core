//go:build protocoldef

package protocoldef

// ChatMessagePush is one chat line as delivered to a connected player: the
// same shape history returns. FromID is zero and FromName is the actor label
// for a system message (a login announcement, for instance).
type ChatMessagePush struct {
	Kind     string `pb:"1"`
	Target   int64  `pb:"2"`
	Seq      int64  `pb:"3"`
	FromID   int64  `pb:"4"`
	FromName string `pb:"5"`
	Text     string `pb:"6"`
	System   bool   `pb:"7"`
}

//roost:protocol group=game handler=player
type ChatMessageProtocol interface {
	//roost:msg id=10101
	ChatMessage() ChatMessagePush
}
