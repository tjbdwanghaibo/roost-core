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
	// OrderKey 是 RPC 请求的顺序键（bus.WithOrderKey）：接收实例上同一个键的请求按到达顺序逐个处理，空键的请求并行、
	// 不承诺顺序。路由 RPC 填目标实体 ID，affinity 方法填 affinity 键；模块消息不用。
	OrderKey string
}

// BroadcastType determines message routing scope.
type BroadcastType int32

const (
	BroadcastNone       BroadcastType = 0 // point-to-point
	BroadcastModule     BroadcastType = 1 // all instances of a module
	BroadcastServerType BroadcastType = 2 // all servers of a type
	BroadcastAll        BroadcastType = 3 // all servers
)
