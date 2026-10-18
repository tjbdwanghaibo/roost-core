package dataengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// EffectConsumer 是一个事务性 effect 消费者（outbox 的消费端，DESIGN-TODO D12 方向 A）的参数。流、主题前缀与回执库
// 不在这里：SubscribeEffects 按 DataEngine Mod 的声明（dataengine.effects.*、dataengine.database）取，生产方与消费方
// 不会对“effect 发到哪里”各写一份。
type EffectConsumer struct {
	// Durable 是 JetStream 持久消费者名；同一服务的全部副本共用，每个 effect 投给其中一个。必填。
	Durable string
	// Topic 是生产方 nest.Effect.Topic（不带主题前缀）。必填。
	Topic string
	// Inbox 是回执集合（dataengine.database 库里），不写取 "_effect_inbox_" + Durable。两个消费者共用一个集合会按
	// EffectID 互相去重，所以缺省每个 durable 一个。
	Inbox string
	// AckWait 不写取 30s。handler 的 ctx 以它为截止：超时取消并 nak，避免 broker 重投给别的副本时这边还在跑。
	AckWait time.Duration
	// MaxDeliver 不写取 25000（配合退避上限约八天半，与 saga 步骤消费者相同）；耗尽后 broker 侧 Term，
	// 计 nats.jetstream.terminal.total{reason="max_deliver"}，这个 effect 丢弃。
	MaxDeliver    int
	MaxAckPending int           // 不写取 256
	NakBackoffMin time.Duration // 不写取 250ms
	NakBackoffMax time.Duration // 不写取 30s
	// ReceiptTTL 不写取 30 天，必须长于 dataengine.effects.max_age（流里的 effect 还可能被重投时回执不能先过期）。
	ReceiptTTL time.Duration
}

// 缺省值。
const (
	DefaultEffectAckWait       = 30 * time.Second
	DefaultEffectMaxDeliver    = 25_000
	DefaultEffectMaxAckPending = 256
	DefaultEffectReceiptTTL    = 30 * 24 * time.Hour
)

// PoisonEffect 把 handler 的错误标记为毒消息：这个 effect 永远处理不了（载荷解不开、内容不合法），重投没有意义。
// SubscribeEffects 对它直接 Term（不再重投）、记 ERROR 日志并计 dataengine.effect_consumer.handled_total{result="poison"}，
// 回执不写——这个 effect 就此丢弃，**非零要人核对**。基础设施错误不要标记，返回原错误让它重投。
func PoisonEffect(err error) error {
	if err == nil {
		return nil
	}
	return fnats.Permanent(err)
}

// SubscribeEffects 按 DataEngine Mod 的声明订阅 effect 流上 consumer.Topic 的 effect，业务只给处理函数。
//
// 处理语义：
//   - 至少一次投递、按 EffectID 至多一次生效：handler 在 Mongo 事务里运行（nestwal.MongoEffectInbox），经传入的 ctx 做的
//     Mongo 写与回执原子提交；同一 effect 再到达时读到回执就跳过（result=duplicate）。Mongo 驱动可能重跑事务回调，
//     不在事务里的副作用（调用另一个服务）必须按 EffectEnvelope.EffectID 幂等，例如把它当作 RequestID。
//   - handler 返回 nil：回执提交后 ack（result=ok）。
//   - 返回普通错误：事务回滚、不写回执，nak 后按 NakBackoffMin..NakBackoffMax 退避重投（result=retry）。
//   - 返回 PoisonEffect(err)、信封解不开或同一 EffectID 内容不同（nestwal.ErrEffectIdentityConflict）：Term，不重投
//     （result=poison，ERROR 日志点名 durable、主题与流序号）。
//   - 重投到 MaxDeliver 仍失败：broker 侧 Term，见 EffectConsumer.MaxDeliver。
//
// 返回的订阅由调用方 Drain（服务 Shutdown 里）；回执 TTL 不长于 dataengine.effects.max_age 时拒绝订阅。
func SubscribeEffects(ctx context.Context, registry *app.Registry, consumer EffectConsumer, handler nestwal.EffectHandler) (fnats.IJetStreamSubscription, error) {
	if registry == nil || handler == nil {
		return nil, errors.New("dataengine effects: registry and handler are required")
	}
	durable, topic := strings.TrimSpace(consumer.Durable), strings.Trim(strings.TrimSpace(consumer.Topic), ".")
	if durable == "" || topic == "" || strings.ContainsAny(durable, ". *>\t\n") {
		return nil, fmt.Errorf("dataengine effects: consumer %q on topic %q: durable (no '.', '*', '>' or spaces) and topic are required", consumer.Durable, consumer.Topic)
	}
	consumer = consumer.withDefaults()
	var settings config
	if err := app.LoadConfig(registry.Config(), &settings); err != nil {
		return nil, fmt.Errorf("dataengine effects %s: %w", durable, err)
	}
	if consumer.ReceiptTTL <= settings.Effects.MaxAge {
		return nil, fmt.Errorf("dataengine effects %s: receipt ttl %s must exceed dataengine.effects.max_age (%s): an effect still on the stream "+
			"could be redelivered after its receipt expired and run again", durable, consumer.ReceiptTTL, settings.Effects.MaxAge)
	}
	jetstream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetstream == nil {
		return nil, fmt.Errorf("dataengine effects %s: capability %q not found", durable, mods.ModNatsJetStream)
	}
	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return nil, fmt.Errorf("dataengine effects %s: capability %q not found", durable, mods.ModMongo)
	}
	collection := strings.TrimSpace(consumer.Inbox)
	if collection == "" {
		collection = "_effect_inbox_" + durable
	}
	inbox, err := nestwal.NewMongoEffectInbox(mongoClient, settings.Database, collection, nestwal.EffectInboxOptions{ReceiptTTL: consumer.ReceiptTTL})
	if err != nil {
		return nil, fmt.Errorf("dataengine effects %s: %w", durable, err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		return nil, fmt.Errorf("dataengine effects %s: prepare inbox: %w", durable, err)
	}
	return jetstream.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream: settings.Effects.Stream, Name: durable, Durable: durable,
		FilterSubject: settings.Effects.subjectPrefix() + "." + topic, DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: consumer.AckWait, MaxDeliver: consumer.MaxDeliver, MaxAckPending: consumer.MaxAckPending,
		NakBackoffMin: consumer.NakBackoffMin, NakBackoffMax: consumer.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		return handleEffect(messageCtx, durable, consumer.AckWait, inbox, message, handler)
	})
}

// handleEffect 处理一次投递并给出 ack / nak / Term 的结论（返回值交给 JetStream 适配层结算）。
func handleEffect(ctx context.Context, durable string, ackWait time.Duration, inbox *nestwal.MongoEffectInbox, message *fnats.JetStreamMsg, handler nestwal.EffectHandler) error {
	var envelope nestwal.EffectEnvelope
	if message == nil || len(message.Data) == 0 {
		return poisonEffect(durable, message, errors.New("empty effect message"))
	}
	if err := json.Unmarshal(message.Data, &envelope); err != nil || envelope.EffectID == "" {
		if err == nil {
			err = errors.New("effect envelope has no effect_id")
		}
		return poisonEffect(durable, message, fmt.Errorf("decode effect: %w", err))
	}
	processCtx, cancel := context.WithTimeout(ctx, ackWait)
	defer cancel()
	duplicate, err := inbox.Handle(processCtx, envelope, handler)
	switch {
	case errors.Is(err, nestwal.ErrEffectIdentityConflict), fnats.IsPermanent(err):
		return poisonEffect(durable, message, fmt.Errorf("effect %s: %w", envelope.EffectID, err))
	case err != nil:
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": durable, "result": "retry"}, 1)
		slog.Warn("dataengine effects: handler failed; the effect will be redelivered", "consumer", durable, "effect_id", envelope.EffectID,
			"topic", envelope.Topic, "deliveries", message.NumDelivered, "err", err)
		return err
	case duplicate:
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": durable, "result": "duplicate"}, 1)
	default:
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": durable, "result": "ok"}, 1)
	}
	return nil
}

func poisonEffect(durable string, message *fnats.JetStreamMsg, err error) error {
	metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": durable, "result": "poison"}, 1)
	var subject string
	var sequence, deliveries uint64
	if message != nil {
		subject, sequence, deliveries = message.Subject, message.StreamSeq, message.NumDelivered
	}
	slog.Error("dataengine effects: dropping an effect that can never be processed", "consumer", durable, "subject", subject,
		"stream_sequence", sequence, "deliveries", deliveries, "err", err)
	return fnats.Permanent(err)
}

func (consumer EffectConsumer) withDefaults() EffectConsumer {
	if consumer.AckWait <= 0 {
		consumer.AckWait = DefaultEffectAckWait
	}
	if consumer.MaxDeliver <= 0 {
		consumer.MaxDeliver = DefaultEffectMaxDeliver
	}
	if consumer.MaxAckPending <= 0 {
		consumer.MaxAckPending = DefaultEffectMaxAckPending
	}
	if consumer.NakBackoffMin <= 0 {
		consumer.NakBackoffMin = 250 * time.Millisecond
	}
	if consumer.NakBackoffMax <= 0 {
		consumer.NakBackoffMax = 30 * time.Second
	}
	if consumer.NakBackoffMax < consumer.NakBackoffMin {
		consumer.NakBackoffMax = consumer.NakBackoffMin
	}
	if consumer.ReceiptTTL <= 0 {
		consumer.ReceiptTTL = DefaultEffectReceiptTTL
	}
	return consumer
}
