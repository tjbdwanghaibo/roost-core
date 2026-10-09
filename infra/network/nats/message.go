package nats

// NatsMsg is the wire format for inter-service communication.
type NatsMsg struct {
	FromSid   int32
	ToSid     int32 // 0 = broadcast
	ToModule  string
	MsgName   string
	Payload   []byte // encoded by the shared Bus MessagePack codec
	Broadcast BroadcastType
	SessionId string // for RPC correlation
	MsgID     string
	Attempt   int32
	CreatedAt int64
	// ReplySubject 和 DeadlineAt 用于 JetStream RPC；普通消息保持零值。
	ReplySubject string
	DeadlineAt   int64
}

// BroadcastType determines message routing scope.
type BroadcastType int32

const (
	BroadcastNone       BroadcastType = 0 // point-to-point
	BroadcastModule     BroadcastType = 1 // all instances of a module
	BroadcastServerType BroadcastType = 2 // all servers of a type
	BroadcastAll        BroadcastType = 3 // all servers
)
