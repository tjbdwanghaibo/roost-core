package nats

import (
	"context"
	"time"
)

type JetStreamStorage string

const (
	JetStreamStorageFile   JetStreamStorage = "file"
	JetStreamStorageMemory JetStreamStorage = "memory"
)

type JetStreamDeliverPolicy string

const (
	JetStreamDeliverAll JetStreamDeliverPolicy = "all"
	JetStreamDeliverNew JetStreamDeliverPolicy = "new"
)

type IJetStream interface {
	EnsureStream(context.Context, JetStreamConfig) error
	Publish(context.Context, string, []byte, JetStreamPublishOptions) (JetStreamPublishAck, error)
	Subscribe(context.Context, JetStreamConsumerConfig, JetStreamHandler) (IJetStreamSubscription, error)
}

type JetStreamConfig struct {
	Name       string
	Subjects   []string
	Storage    JetStreamStorage
	MaxAge     time.Duration
	Duplicates time.Duration
	Replicas   int
	MaxBytes   int64
}

type JetStreamPublishOptions struct {
	MsgID string
}

type JetStreamPublishAck struct {
	Stream    string
	Sequence  uint64
	Duplicate bool
}

type JetStreamConsumerConfig struct {
	Stream        string
	Name          string
	Durable       string
	FilterSubject string
	DeliverPolicy JetStreamDeliverPolicy
	AckWait       time.Duration
	MaxDeliver    int
	MaxAckPending int
	NakBackoffMin time.Duration
	NakBackoffMax time.Duration
}

type JetStreamHandler func(context.Context, *JetStreamMsg) error

type JetStreamMsg struct {
	Subject      string
	Data         []byte
	Stream       string
	Consumer     string
	StreamSeq    uint64
	ConsumerSeq  uint64
	NumDelivered uint64
	// InProgress 告诉 broker 这次投递仍在处理，重置它的 AckWait 计时（JetStream 的 +WPI）。处理时间可能超过
	// AckWait 的消费者在处理期间定期调用它，否则 broker 会把未确认的消息重投给别的实例并发执行。
	// 测试替身或不支持的传输为 nil。
	InProgress func() error
	// DeferSettle 把这次投递的结算（ack / nak / term）从 handler 返回的时刻推迟到返回的 settle 被调用的时刻：handler 调用
	// 它、把 settle 交给自己的后台工作，然后返回 nil，传输不在回调返回时 ack，回调立刻处理这个 durable 的下一条消息。
	//   - settle(nil) ack；settle(err) 与 handler 返回 err 同一规则（Permanent / 达到 MaxDeliver → term，否则按退避 nak）。
	//     settle 幂等、可在任何 goroutine 调用，也可以在 handler 返回之前调用。
	//   - DeferSettle 之后 handler 仍返回非 nil 错误（或 panic）时，传输按这个错误立即结算，之后的 settle 不再生效。
	//   - 返回 nil 表示不能推迟（订阅已在结束、或同一投递第二次调用）：handler 照旧同步处理，按返回值结算。
	//   - 推迟中的投递仍是未确认消息：受消费者的 MaxAckPending 约束，超过 AckWait 未结算会被 broker 重投。订阅的 Closed
	//     在全部推迟的结算完成之后才关闭（Drain 等它们；Stop 取消 handler 的 ctx，后台工作应据此尽快结算），所以 settle
	//     必须最终被调用。
	// 不调用它的 handler 行为与以前完全相同（自动按返回值结算）。测试替身或不支持的传输为 nil。
	DeferSettle func() (settle func(error))
}

type IJetStreamSubscription interface {
	Stop()
	Drain()
	Closed() <-chan struct{}
}
