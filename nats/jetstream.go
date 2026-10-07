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
	// AckWait 的消费者在处理期间定期调用它，否则 broker 会把未确认的消息重投给别的实例并发执行
	// （RR-20261006-70）。测试替身或不支持的传输为 nil。
	InProgress func() error
}

type IJetStreamSubscription interface {
	Stop()
	Drain()
	Closed() <-chan struct{}
}
