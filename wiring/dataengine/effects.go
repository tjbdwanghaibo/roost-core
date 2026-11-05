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
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
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
	// Delivery 必须与生产方声明的相同（roost add effect 生成的 effects.Delivery<Topic>）：reliable（缺省）订阅效果流
	// （JetStream durable + Mongo inbox，至少一次、按 EffectID 至多一次生效）；best_effort 用 NATS 队列订阅（队列组 =
	// Durable），没有 inbox、没有重投，handler 出错只记日志与 handled_total{result="failed"}（见 effects_best_effort.go）。
	// best_effort 时 Inbox、MaxDeliver、NakBackoff*、ReceiptTTL 不起作用；AckWait 仍是 handler ctx 的截止，MaxAckPending
	// 仍是已接纳未处理完的上界。
	Delivery coredata.EffectDelivery
	// Order 是进程内的处理顺序：entity（缺省，空值同）——同一 Key 按到达顺序逐条、不同 Key 并发；none——不按 Key 排队，
	// 只受 Workers 上限（Key 只是数据）。跨副本、全局顺序都不保证（全局顺序见生产端 dataengine.projection.effect_order）。
	Order EffectOrder
	// Workers 是本进程里这个消费者同时在跑的 handler 数上限；不写（0）取 dataengine.effect_consumer.workers（缺省 16），
	// 负数拒绝订阅。1 = 在 nats.go 的回调里逐条处理（引入并发之前的行为）。并发规则见 SubscribeEffects。
	Workers int
}

// EffectOrder 是 effect 消费者在本进程里的处理顺序，见 EffectConsumer.Order。
type EffectOrder string

const (
	// EffectOrderEntity（缺省）：同一信封 Key 按到达顺序逐条处理，不同 Key 并发。
	EffectOrderEntity EffectOrder = "entity"
	// EffectOrderNone：不按 Key 排队，只受 Workers 上限。
	EffectOrderNone EffectOrder = "none"
)

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
// 进程内并发（Workers > 1，维护者 2026-11-05）：
//   - 同一实体（信封的 Key）的 effect 在本进程按到达顺序逐条处理：前一条的 handler 返回、结算之后才开始下一条。
//     不同 Key 的 effect 并发，同时在跑的 handler 不超过 Workers。Key 为空的 effect 各自处理、不承诺顺序（没有实体可排）。
//   - 同一进程里不同实体的 effect 处理先后不再与流顺序相同；多个实例共享 durable 时本来就不成立。失败重投（nak 退避）
//     本来就让同一实体的 effect 可能晚于它后面的那条生效，这一点不变。
//   - 已接纳、还没结算（排队与在跑）的 effect 不超过 MaxAckPending：满了以后回调停在准入处等一条结算（背压），这条既不
//     确认也不 nak，nats.go 不再投后面的；订阅 Stop 时它 nak 交还 broker。
//   - 排队期间每 AckWait/2 发一次 in-progress；排队超过 AckWait/10 的，开始处理前再发一次，broker 侧的重投期限不早于
//     handler 的期限（AckWait）减 AckWait/10。
//   - 结算推迟到 handler 返回（JetStreamMsg.DeferSettle），回执、去重与 ack / nak / Term 规则与逐条处理相同。传输不支持
//     推迟结算（测试替身）时在回调里逐条处理。
//
// 返回的订阅登记成 App 的输入：App 停机第一步 Drain 并等在途 effect 处理完（业务 Shutdown 里再 Drain() 是空操作，可以留着）；
// 回执 TTL 不长于 dataengine.effects.max_age 时拒绝订阅。
func SubscribeEffects(ctx context.Context, registry *app.Registry, consumer EffectConsumer, handler nestwal.EffectHandler) (fnats.IJetStreamSubscription, error) {
	if registry == nil || handler == nil {
		return nil, errors.New("dataengine effects: registry and handler are required")
	}
	durable, topic := strings.TrimSpace(consumer.Durable), strings.Trim(strings.TrimSpace(consumer.Topic), ".")
	if durable == "" || topic == "" || strings.ContainsAny(durable, ". *>\t\n") {
		return nil, fmt.Errorf("dataengine effects: consumer %q on topic %q: durable (no '.', '*', '>' or spaces) and topic are required", consumer.Durable, consumer.Topic)
	}
	if consumer.Workers < 0 {
		return nil, fmt.Errorf("dataengine effects %s: workers %d must not be negative (0 takes dataengine.effect_consumer.workers)", durable, consumer.Workers)
	}
	switch consumer.Order {
	case "", EffectOrderEntity, EffectOrderNone:
	default:
		return nil, fmt.Errorf("dataengine effects %s: order %q is not entity | none", durable, consumer.Order)
	}
	switch consumer.Delivery {
	case coredata.EffectReliable, coredata.EffectBestEffort:
	default:
		return nil, fmt.Errorf("dataengine effects %s: unknown delivery %s", durable, consumer.Delivery)
	}
	consumer = consumer.withDefaults()
	var settings config
	if err := app.LoadConfig(registry.Config(), &settings); err != nil {
		return nil, fmt.Errorf("dataengine effects %s: %w", durable, err)
	}
	workers := consumer.Workers
	if workers == 0 {
		workers = settings.EffectConsumer.Workers
	}
	if consumer.Delivery == coredata.EffectBestEffort {
		return subscribeBestEffortEffects(registry, consumer, durable, topic, settings, workers, handler)
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
	callback := func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		return handleEffect(messageCtx, durable, consumer.AckWait, inbox, message, handler)
	}
	if workers > 1 {
		callback = newEffectDispatcher(durable, consumer, workers, inbox, handler).deliver
	}
	subscription, err := jetstream.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream: settings.Effects.Stream, Name: durable, Durable: durable,
		FilterSubject: settings.Effects.subjectPrefix() + "." + topic, DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: consumer.AckWait, MaxDeliver: consumer.MaxDeliver, MaxAckPending: consumer.MaxAckPending,
		NakBackoffMin: consumer.NakBackoffMin, NakBackoffMax: consumer.NakBackoffMax,
	}, callback)
	if err != nil || subscription == nil {
		return subscription, err
	}
	return registerEffectIngress(registry, durable, subscription)
}

// registerEffectIngress 把 effect 订阅登记成 App 的外部输入：停机第一步 Drain 并等在途 effect 处理完，之后才停 Nest /
// DataEngine（App 生命周期方案）。业务 Shutdown 里的 Drain() 之后是空操作。App 已经在关输入时拒绝登记，撤掉刚建的订阅。
func registerEffectIngress(registry *app.Registry, durable string, subscription fnats.IJetStreamSubscription) (fnats.IJetStreamSubscription, error) {
	if err := app.AddIngress(registry, "effect consumer "+durable, drainOnClose{subscription}); err != nil {
		subscription.Stop()
		return nil, fmt.Errorf("dataengine effects %s: %w", durable, err)
	}
	return subscription, nil
}

// subscribeBestEffortEffects 是 best_effort 消费者：不建 inbox、不看回执 TTL，NATS 队列订阅 BestEffortSubject。
func subscribeBestEffortEffects(registry *app.Registry, consumer EffectConsumer, durable, topic string, settings config, workers int, handler nestwal.EffectHandler) (fnats.IJetStreamSubscription, error) {
	client, ok := app.Lookup[fnats.IClient](registry, mods.ModNats)
	if !ok || client == nil {
		return nil, fmt.Errorf("dataengine effects %s: capability %q not found", durable, mods.ModNats)
	}
	subscription, err := subscribeBestEffort(client, BestEffortSubject(settings.Effects.subjectPrefix(), topic), durable, consumer, workers, handler)
	if err != nil {
		return nil, fmt.Errorf("dataengine effects %s: %w", durable, err)
	}
	return registerEffectIngress(registry, durable, subscription)
}

// drainOnClose 是 effect 订阅的 app.IngressCloser：Drain（停止拉取、交完已取到的）后在 ctx 内等订阅关闭（在途 handler
// 返回、推迟的结算完成）。ctx 先结束返回 ctx 错误，订阅保留，再调用接着等。
type drainOnClose struct{ subscription fnats.IJetStreamSubscription }

func (d drainOnClose) CloseIngress(ctx context.Context) error {
	d.subscription.Drain()
	select {
	case <-d.subscription.Closed():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// handleEffect 处理一次投递并给出 ack / nak / Term 的结论（返回值交给 JetStream 适配层结算）。
func handleEffect(ctx context.Context, durable string, ackWait time.Duration, inbox *nestwal.MongoEffectInbox, message *fnats.JetStreamMsg, handler nestwal.EffectHandler) error {
	envelope, err := decodeEffect(durable, message)
	if err != nil {
		return err
	}
	return processEffect(ctx, durable, ackWait, inbox, message, envelope, handler)
}

// decodeEffect 解开信封；解不开或没有 EffectID 返回毒消息错误（Term）。
func decodeEffect(durable string, message *fnats.JetStreamMsg) (nestwal.EffectEnvelope, error) {
	var envelope nestwal.EffectEnvelope
	if message == nil || len(message.Data) == 0 {
		return envelope, poisonEffect(durable, message, errors.New("empty effect message"))
	}
	if err := json.Unmarshal(message.Data, &envelope); err != nil || envelope.EffectID == "" {
		if err == nil {
			err = errors.New("effect envelope has no effect_id")
		}
		return envelope, poisonEffect(durable, message, fmt.Errorf("decode effect: %w", err))
	}
	return envelope, nil
}

// processEffect 在 inbox 事务里运行 handler（ctx 以 AckWait 为截止），按结果计数并给出结算结论。
func processEffect(ctx context.Context, durable string, ackWait time.Duration, inbox *nestwal.MongoEffectInbox, message *fnats.JetStreamMsg,
	envelope nestwal.EffectEnvelope, handler nestwal.EffectHandler) error {
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

// effectDispatcher 是 Workers > 1 时的投递回调（规则见 SubscribeEffects“进程内并发”）：回调解开信封、在准入处取一个
// 在途名额（满了就等，背压）、推迟结算，按信封 Key 交给 worker.KeyedDispatcher（与总线 RPC 的按顺序键派发同一份实现）
// 后立即返回，nats.go 接着投下一条；派发的任务等一个 worker 名额、处理、结算。
//
// 为什么按 Key 排队而不是按 Key 哈希到固定 worker：handler 会同步调用别的服务（联盟成员变化 effect 调守卫所在实例），
// 固定 worker 让一个慢实体挡住同 worker 上的其他实体；按 Key 排队时一个慢 handler 只挡住自己的实体和一个 worker 名额。
type effectDispatcher struct {
	durable string
	ackWait time.Duration
	inbox   *nestwal.MongoEffectInbox
	handler nestwal.EffectHandler
	ordered bool // Order != none：按信封 Key 排队

	inFlight chan struct{} // 已接纳、还没结算的 effect（排队与在跑），容量 MaxAckPending
	running  chan struct{} // 在跑的 handler，容量 Workers
	keyed    *worker.KeyedDispatcher
}

func newEffectDispatcher(durable string, consumer EffectConsumer, workers int, inbox *nestwal.MongoEffectInbox, handler nestwal.EffectHandler) *effectDispatcher {
	return &effectDispatcher{
		durable: durable, ackWait: consumer.AckWait, inbox: inbox, handler: handler, ordered: consumer.Order != EffectOrderNone,
		inFlight: make(chan struct{}, consumer.MaxAckPending),
		running:  make(chan struct{}, workers),
		// 真正的上界是 inFlight。派发器自己的计数在任务返回时才减，比 inFlight 的释放（任务里 settle 之后）晚一步：
		// 已释放名额、还没返回的任务至多 MaxAckPending 个，所以它的上限取两倍，永远不会先到。
		keyed: worker.NewKeyedDispatcher(2 * consumer.MaxAckPending),
	}
}

// deliver 是 nats.go 的回调。返回 nil 表示已接纳（结算由派发的任务完成）；返回错误时按错误立即结算。
func (d *effectDispatcher) deliver(ctx context.Context, message *fnats.JetStreamMsg) error {
	if message == nil || message.DeferSettle == nil {
		return handleEffect(ctx, d.durable, d.ackWait, d.inbox, message, d.handler)
	}
	envelope, err := decodeEffect(d.durable, message)
	if err != nil {
		return err
	}
	select {
	case d.inFlight <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	settle := message.DeferSettle()
	if settle == nil {
		// 订阅正在结束，不能推迟：在回调里处理。
		defer func() { <-d.inFlight }()
		return processEffect(ctx, d.durable, d.ackWait, d.inbox, message, envelope, d.handler)
	}
	admitted := time.Now()
	stopInProgress := fnats.KeepInProgress(ctx, message, d.ackWait/2, d.inProgressFailed(message))
	run := func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("dataengine effects: handler panic: %v", recovered)
				slog.Error("dataengine effects: handler panicked; the effect will be redelivered", "consumer", d.durable, "effect_id", envelope.EffectID, "err", err)
			}
			settle(err)
			<-d.inFlight
		}()
		err = d.run(ctx, message, envelope, admitted, stopInProgress)
	}
	key := envelope.Key
	if !d.ordered {
		key = "" // order=none：不按 Key 排队，只受 Workers 上限
	}
	if err := d.keyed.Dispatch(key, startEffectTask, run); err != nil {
		// 不会发生（派发上限是准入名额的两倍，见 newEffectDispatcher）；按错误立即结算（nak）。
		stopInProgress()
		<-d.inFlight
		return fmt.Errorf("dataengine effects %s: dispatch effect %s: %w", d.durable, envelope.EffectID, err)
	}
	return nil
}

// run 等一个 worker 名额后处理一条已接纳的 effect；订阅 Stop（ctx 取消）时不再处理，返回 ctx 错误（nak）。
func (d *effectDispatcher) run(ctx context.Context, message *fnats.JetStreamMsg, envelope nestwal.EffectEnvelope, admitted time.Time, stopInProgress func()) error {
	select {
	case d.running <- struct{}{}:
	case <-ctx.Done():
		stopInProgress()
		return ctx.Err()
	}
	defer func() { <-d.running }()
	stopInProgress()
	if err := ctx.Err(); err != nil {
		return err
	}
	// 排队久的先重置 broker 的 AckWait 计时，让它的重投期限跟上 handler 的期限（AckWait）。
	if message.InProgress != nil && time.Since(admitted) >= d.ackWait/10 {
		if err := message.InProgress(); err != nil {
			d.inProgressFailed(message)(err)
		}
	}
	return processEffect(ctx, d.durable, d.ackWait, d.inbox, message, envelope, d.handler)
}

func (d *effectDispatcher) inProgressFailed(message *fnats.JetStreamMsg) func(error) {
	return func(err error) {
		slog.Warn("dataengine effects: in-progress ack failed; the broker may redeliver the effect after ack wait", "consumer", d.durable,
			"subject", message.Subject, "stream_sequence", message.StreamSeq, "err", err)
	}
}

// startEffectTask 给派发的任务起 goroutine。不进池：每个任务都持着一次推迟的结算，订阅的 Closed 等全部结算完成，
// 生命周期由订阅管。
func startEffectTask(task func()) error {
	go task()
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
