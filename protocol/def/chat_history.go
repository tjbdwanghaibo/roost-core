//go:build protocoldef

package protocoldef

// ChatHistoryRequest pages a channel the player may read. AfterSeq is the
// reconnect cursor (exclusive; zero means from the oldest retained line);
// Limit is required and capped by the chat service.
type ChatHistoryRequest struct {
	Kind     string `pb:"1"`
	Target   int64  `pb:"2"`
	AfterSeq int64  `pb:"3"`
	Limit    int32  `pb:"4"`
}

// ChatHistoryResponse returns the lines in sequence order plus the cursor to
// continue from.
type ChatHistoryResponse struct {
	Code       int32             `pb:"1"`
	Reason     string            `pb:"2"`
	Messages   []ChatMessagePush `pb:"3"`
	NextCursor int64             `pb:"4"`
	HasMore    bool              `pb:"5"`
}

//roost:protocol group=game handler=player
type ChatHistoryProtocol interface {
	//roost:msg id=10007
	ChatHistory(ChatHistoryRequest) ChatHistoryResponse
}
