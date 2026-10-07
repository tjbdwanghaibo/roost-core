package syncbus

import "context"

// SyncMsg is the wire format for a sync message.
type SyncMsg struct {
	MessageID string // delivery identity; independent from the business data version
	Topic     string // sync topic (e.g. "remote_entity", "config")
	Key       int64  // business key (entity ID, config ID, etc.)
	Version   int64  // data version, subscriber uses to discard stale messages
	Data      []byte // serialized business data (nil means delete)
	FromSid   int32  // sender server ID
	Part      uint32 // zero-based part index when Parts > 1
	Parts     uint32 // total part count; zero/one means an unfragmented message
	Encoding  string // optional payload encoding, e.g. gzip
	Checksum  string // SHA-256 of the decoded business payload
}

// Handler processes an incoming sync message.
// Return error to log warning (message is NOT retried).
//
// ctx 是这次投递的 ctx（Subscription.Deliver 交出）：它带着“正在执行这个订阅的回调”的标记，handler 里
// 退订自己时必须把它（或由它派生的 ctx）传给 Subscription.Unsubscribe，否则会等自己。退订不取消它。
type Handler func(ctx context.Context, msg *SyncMsg) error

// IPublisher publishes sync messages to subscribers.
type IPublisher interface {
	Publish(msg *SyncMsg) error
}

// IContextPublisher is an optional publisher capability for transports that can
// honor cancellation and deadlines during publish.
type IContextPublisher interface {
	PublishContext(context.Context, *SyncMsg) error
}

// ISubscriber subscribes to sync topics.
type ISubscriber interface {
	// Subscribe registers a handler for a topic. 返回的 Subscription.Unsubscribe(ctx) 返回 nil 之后，
	// 这个订阅既没有在途回调、也不会再有新回调（A3 ②，见 Subscription）。
	Subscribe(topic string, handler Handler) (*Subscription, error)
}

// ILiveSubscriber 是可确认订阅的能力（Mirror 第 4 步，docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）。
//
// SubscribeLive 返回 nil 即“订阅已确认”：在下述有效信封和保留边界内，短暂断线与处理超时由传输补投。订阅确认之前发布的历史不承诺投递；同一身份重新订阅时可能从上次的
// 游标续投一段，消费方按版本准入处理。传输自己发出的消息照旧不回送。
//
// 保证受 broker MaxAge/MaxBytes 保留边界及消费者 MaxDeliver 次数上限约束；坏信封会 ACK，业务 handler 错误也按既有策略 ACK。
// 退订/停机不再接收，消费者生命周期窗口另见 RR-20261008-25；不能把此接口当无限保留日志。
//
// 普通 NATS 是最多一次，不提供这项能力；JetStream 用 DeliverNew 的 durable 消费者提供。需要推送一致性的
// 调用方先做类型断言，拿不到时显式退化（例如 remoteentity 的快照推送退化为按需读取并记日志）。
type ILiveSubscriber interface {
	SubscribeLive(topic string, handler Handler) (*Subscription, error)
}

// ISyncBus combines publish and subscribe capabilities.
type ISyncBus interface {
	IPublisher
	ISubscriber
}
