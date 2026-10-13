package driver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

const (
	defaultJetStreamSyncPrefix       = "roost.sync"
	defaultJetStreamSyncStream       = "ROOST_SYNC"
	defaultJetStreamSyncAckWait      = 10 * time.Second
	defaultJetStreamSyncMaxDeliver   = 5
	defaultJetStreamSyncMaxAge       = 30 * time.Minute
	defaultJetStreamSyncDuplicates   = 2 * time.Minute
	defaultJetStreamSyncSetupTimeout = 5 * time.Second
	defaultJetStreamSyncPublishTime  = 5 * time.Second
)

// JetStreamSyncConfig 配置 JetStream 同步总线；零值字段在 normalizeJetStreamSyncConfig 中补默认值。
type JetStreamSyncConfig struct {
	LocalSid int32
	Prefix   string
	// Stream 是承载 Prefix.> 的 JetStream 流名。留空时由 Prefix 派生（JetStreamSyncStream）：
	// 默认 prefix roost.sync 对应 ROOST_SYNC，其他 prefix 各得其流。
	Stream       string
	Storage      fnats.JetStreamStorage
	AckWait      time.Duration
	MaxDeliver   int
	StreamMaxAge time.Duration
	Duplicates   time.Duration
	Replicas     int
	MaxBytes     int64
	SetupTimeout time.Duration
	PublishTime  time.Duration
}

func normalizeJetStreamSyncConfig(cfg JetStreamSyncConfig) JetStreamSyncConfig {
	if cfg.Prefix == "" {
		cfg.Prefix = defaultJetStreamSyncPrefix
	}
	if cfg.Stream == "" {
		cfg.Stream = JetStreamSyncStream(cfg.Prefix)
	}
	if cfg.Storage == "" {
		cfg.Storage = fnats.JetStreamStorageFile
	}
	if cfg.AckWait <= 0 {
		cfg.AckWait = defaultJetStreamSyncAckWait
	}
	if cfg.MaxDeliver <= 0 {
		cfg.MaxDeliver = defaultJetStreamSyncMaxDeliver
	}
	if cfg.StreamMaxAge <= 0 {
		cfg.StreamMaxAge = defaultJetStreamSyncMaxAge
	}
	if cfg.Duplicates <= 0 {
		cfg.Duplicates = defaultJetStreamSyncDuplicates
	}
	if cfg.SetupTimeout <= 0 {
		cfg.SetupTimeout = defaultJetStreamSyncSetupTimeout
	}
	if cfg.PublishTime <= 0 {
		cfg.PublishTime = defaultJetStreamSyncPublishTime
	}
	return cfg
}

// jetStreamSyncBus 以每个主题一个 durable 消费者实现 ISyncBus：消息确认后才算投递，
// 本地多个订阅由 topicFanout 在进程内扇出。
type jetStreamSyncBus struct {
	js  fnats.IJetStream
	cfg JetStreamSyncConfig

	// mu 只保护 topic 登记和代次；创建、停止与等待不持全局锁。
	mu     sync.Mutex
	topics map[string]*topicFanout

	// consume 回调（及其中的本地 handler）在 nats.go 的回调 goroutine 上执行，ConsumeContext.Stop
	// 不等它们。deliveries 是总线自己的准入与在途计数（共用的 operation.Lifetime）：停止先关准入、
	// 停订阅，再在调用方 ctx 内等在途归零；归零之前总线与连接都不能交还。consumers 计数尚未退役完的
	// 消费者，停机同样要等它归零。
	deliveries  operation.Lifetime
	consumers   operation.Lifetime
	setupCtx    context.Context
	cancelSetup context.CancelFunc
}

// errJetStreamSyncStopping 让停止开始后才到达的投递回到 broker（driver 据此 NAK），由下一次
// 消费同一 durable 的实例处理，不在停止中的总线上执行业务 handler。
var errJetStreamSyncStopping = errors.New("jetstream sync: bus is stopping; delivery returned to the broker")

// topicFanout is one topic's single durable consumer plus every local
// subscription registered on it. ISyncBus.Subscribe is
// broadcast: the plain NATS bus gives every local subscriber every message.
// A durable JetStream consumer is a work queue — two Consume calls on the
// same consumer name split the stream between them — so one underlying
// subscription per topic fans out locally instead of one per Subscribe.
type topicFanout struct {
	sub    fnats.IJetStreamSubscription
	locals map[uint64]*fsyncbus.Subscription
	nextID uint64
	// ready 在网络创建结束后关闭；retiring 在锁内唯一地移交退役责任。
	ready    chan struct{}
	retiring bool
	calls    operation.Lifetime
}

// snapshot 按登记顺序返回当前本地订阅；调用方持 b.mu。
func (f *topicFanout) snapshot() []*fsyncbus.Subscription {
	ids := make([]uint64, 0, len(f.locals))
	for id := range f.locals {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]*fsyncbus.Subscription, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.locals[id])
	}
	return out
}

// NewJetStreamSyncBus 确保承载 Prefix.> 的流存在后创建总线；消费者在首次订阅主题时创建。
func NewJetStreamSyncBus(ctx context.Context, js fnats.IJetStream, cfg JetStreamSyncConfig) (*jetStreamSyncBus, error) {
	if js == nil {
		return nil, fmt.Errorf("jetstream sync: jetstream is nil")
	}
	cfg = normalizeJetStreamSyncConfig(cfg)
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	setupCtx, cancel := context.WithTimeout(ctx, cfg.SetupTimeout)
	defer cancel()
	if err := js.EnsureStream(setupCtx, fnats.JetStreamConfig{
		Name:       cfg.Stream,
		Subjects:   []string{fmt.Sprintf("%s.>", cfg.Prefix)},
		Storage:    cfg.Storage,
		MaxAge:     cfg.StreamMaxAge,
		Duplicates: cfg.Duplicates,
		Replicas:   cfg.Replicas,
		MaxBytes:   cfg.MaxBytes,
	}); err != nil {
		return nil, fmt.Errorf("jetstream sync: ensure stream %s for %s.>: %w", cfg.Stream, cfg.Prefix, err)
	}
	lifetimeCtx, cancelSetup := context.WithCancel(context.Background())
	return &jetStreamSyncBus{js: js, cfg: cfg, topics: make(map[string]*topicFanout), setupCtx: lifetimeCtx, cancelSetup: cancelSetup}, nil
}

func (b *jetStreamSyncBus) Publish(msg *fsyncbus.SyncMsg) error {
	return b.PublishContext(fctx.BaseContext(), msg)
}

func (b *jetStreamSyncBus) PublishContext(ctx context.Context, msg *fsyncbus.SyncMsg) error {
	if b == nil || b.js == nil {
		return fmt.Errorf("jetstream sync: bus is not initialized")
	}
	if msg == nil {
		return fmt.Errorf("jetstream sync: message is nil")
	}
	if strings.TrimSpace(msg.Topic) == "" {
		return fmt.Errorf("jetstream sync: topic is empty")
	}
	owned := *msg
	owned.Data = append([]byte(nil), msg.Data...)
	if owned.FromSid == 0 {
		owned.FromSid = b.cfg.LocalSid
	}
	data, err := json.Marshal(&owned)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.PublishTime)
	defer cancel()
	_, err = b.js.Publish(ctx, b.subject(owned.Topic), data, fnats.JetStreamPublishOptions{MsgID: syncMsgID(&owned)})
	return err
}

func (b *jetStreamSyncBus) Subscribe(topic string, handler fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.subscribe(topic, handler, false)
}

// SubscribeLive 是可确认订阅（fsyncbus.ILiveSubscriber）：这个主题一个 DeliverNew 的
// durable 消费者。CreateOrUpdateConsumer 返回时消费者的起点已定在流的当前末尾，之后入流的消息都会投递
// （AckExplicit，处理超时重投），所以返回 nil 就是“订阅已确认”。durable 名与 Subscribe 的 DeliverAll
// 消费者分开（durableSyncName 的主题加 ".live"）：服务端不允许改已有 durable 的投递策略，共用名字会让
// 已部署的 DeliverAll durable 建不出来；分开后旧 durable 不再被消费，可由运维删除。同一 sid 重启时
// durable 仍在，从上次的确认游标续投（不超过流的 MaxAge）。
func (b *jetStreamSyncBus) SubscribeLive(topic string, handler fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.subscribe(topic, handler, true)
}

// subscribe 建（或复用）一个主题的 durable 消费者并登记本地订阅。live 选 DeliverNew 的那个消费者；
// 两种消费者在 topics 里用不同的键，互不共享本地订阅。
//
// 每个本地订阅是一个 fsyncbus.Subscription：consume 回调经它的 Deliver 调 handler，退订
// （Subscription.Unsubscribe）先关准入、经 release 从 fanout 删除（最后一个时停消费者），再在调用方 ctx
// 内等这个订阅的在途回调——fanout 快照里还拿着它的旧投递也会被等到或被拒，订阅者不必自己排空。
func (b *jetStreamSyncBus) subscribe(topic string, handler fsyncbus.Handler, live bool) (*fsyncbus.Subscription, error) {
	if b == nil || b.js == nil {
		return nil, fmt.Errorf("jetstream sync: bus is not initialized")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, fmt.Errorf("jetstream sync: topic is empty")
	}
	if handler == nil {
		return nil, fmt.Errorf("jetstream sync: handler is nil")
	}
	fanoutKey, durableTopic, policy := topic, topic, fnats.JetStreamDeliverAll
	if live {
		fanoutKey, durableTopic, policy = topic+"\x00live", topic+".live", fnats.JetStreamDeliverNew
	}
	b.mu.Lock()
	if b.deliveries.Stopping() {
		b.mu.Unlock()
		return nil, errJetStreamSyncStopping
	}
	f := b.topics[fanoutKey]
	create := f == nil
	if create {
		if !b.consumers.Begin() {
			b.mu.Unlock()
			return nil, errJetStreamSyncStopping
		}
		f = &topicFanout{locals: make(map[uint64]*fsyncbus.Subscription), ready: make(chan struct{})}
		b.topics[fanoutKey] = f
	} else {
		if f.retiring {
			b.mu.Unlock()
			return nil, fsyncbus.ErrSubscriptionBusy
		}
		select {
		case <-f.ready:
		default:
			b.mu.Unlock()
			return nil, fsyncbus.ErrSubscriptionBusy
		}
	}
	// 先登记接收者，再开启消费；Subscribe 返回前到达的回调也有明确的准入目标。
	f.nextID++
	id := f.nextID
	local := fsyncbus.NewSubscription(topic, handler, func() {
		b.mu.Lock()
		delete(f.locals, id)
		retire := len(f.locals) == 0 && !f.retiring
		if retire {
			f.retiring = true
			f.calls.Stop()
		}
		b.mu.Unlock()
		if retire {
			b.retire(fanoutKey, f)
		}
	})
	f.locals[id] = local
	b.mu.Unlock()
	if !create {
		return local, nil
	}
	cfg := b.cfg
	name := durableSyncName(cfg.Prefix, durableTopic, cfg.LocalSid)
	ctx, cancel := context.WithTimeout(b.setupCtx, cfg.SetupTimeout)
	defer cancel()
	sub, err := b.js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream: cfg.Stream, Name: name, Durable: name, FilterSubject: b.subject(topic), DeliverPolicy: policy,
		AckWait: cfg.AckWait, MaxDeliver: cfg.MaxDeliver,
		// 保留总投递次数上限；退役空窗内的在飞消息按退避 NAK，不改变 RPC/Remote 的重试策略。
		NakBackoffMin: time.Second, NakBackoffMax: max(time.Second, cfg.AckWait),
	}, func(_ context.Context, raw *fnats.JetStreamMsg) error { return b.deliver(f, topic, raw) })
	if err == nil && sub == nil {
		err = errors.New("jetstream sync: transport returned a nil subscription")
	}
	b.mu.Lock()
	f.sub = sub
	close(f.ready)
	retire := (err != nil || b.deliveries.Stopping()) && !f.retiring
	if retire {
		f.retiring = true
		f.calls.Stop()
	}
	stopped := f.retiring
	b.mu.Unlock()
	if retire {
		b.retire(fanoutKey, f)
	}
	if err != nil || stopped {
		_ = local.Unsubscribe(ctx)
		if err != nil {
			return nil, err
		}
		return nil, errJetStreamSyncStopping
	}
	return local, nil
}

// retire 每代只发起一次。release 不能等待自身 handler；若尚未排空，由一个退役者接续。
// Closed 之外还等自己的回调计数，避免替身或底层提前报告 Closed 时与下一代重叠。
func (b *jetStreamSyncBus) retire(key string, f *topicFanout) {
	finish := func() {
		b.mu.Lock()
		if b.topics[key] == f {
			delete(b.topics, key)
		}
		b.mu.Unlock()
		b.consumers.End()
	}
	stop := func() {
		if f.sub != nil {
			f.sub.Stop()
		}
		idle := f.calls.Stop()
		var closed <-chan struct{}
		if f.sub != nil {
			closed = f.sub.Closed()
		} else {
			closed = idle
		}
		select {
		case <-closed:
			select {
			case <-idle:
				finish()
				return
			default:
			}
		default:
		}
		go func() { <-closed; <-idle; finish() }()
	}
	select {
	case <-f.ready:
		stop()
	default:
		go func() { <-f.ready; stop() }()
	}
}

// deliver 是 consume 回调：解码一次，跳过本服消息，再给每个本地订阅一份独立的 Data 副本。
// 没有任何订阅接受时返回 errJetStreamSyncStopping，让 broker 重投而不是 ACK 掉。
func (b *jetStreamSyncBus) deliver(f *topicFanout, topic string, raw *fnats.JetStreamMsg) error {
	if raw == nil {
		return nil
	}
	if !b.deliveries.Begin() {
		return errJetStreamSyncStopping
	}
	defer b.deliveries.End()
	b.mu.Lock()
	if !f.calls.Begin() {
		b.mu.Unlock()
		return errJetStreamSyncStopping
	}
	locals := f.snapshot()
	b.mu.Unlock()
	defer f.calls.End()
	var msg fsyncbus.SyncMsg
	if err := json.Unmarshal(raw.Data, &msg); err != nil {
		slog.Warn("jetstream sync: unmarshal failed", "topic", topic, "err", err)
		return nil
	}
	if msg.FromSid == b.cfg.LocalSid {
		return nil
	}
	accepted := false
	for _, local := range locals {
		own := msg
		own.Data = append([]byte(nil), msg.Data...)
		if b.invoke(local, &own) {
			accepted = true
		}
	}
	if !accepted {
		return errJetStreamSyncStopping
	}
	return nil
}

// invoke 返回实际准入结果；业务错误和 panic 沿用记录后继续的策略，不能据此重投给兄弟订阅。
func (b *jetStreamSyncBus) invoke(local *fsyncbus.Subscription, msg *fsyncbus.SyncMsg) (accepted bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			accepted = true // TryDeliver 只在进入 handler 后传播其 panic，仍沿用记录并 ACK 的策略。
			slog.Error("jetstream sync: handler panic", "topic", local.Topic(), "key", msg.Key, "version", msg.Version, "panic", recovered)
		}
	}()
	var err error
	accepted, err = local.TryDeliver(fctx.BaseContext(), msg)
	if accepted && err != nil {
		slog.Warn("jetstream sync: handler error", "topic", local.Topic(), "key", msg.Key, "version", msg.Version, "err", err)
	}
	return accepted
}

// StopWithContext 按三步停机：
//  1. 关闭投递准入并停掉全部订阅（幂等；之后到达的投递返回 errJetStreamSyncStopping，由 broker 重投）；
//  2. 在 ctx 内等创建收尾、consumer Closed 和已准入回调，超时保留旧代，重试继续等同一批；
//  3. 返回 nil 之后调用方才能释放总线与它下面的 NATS 连接（wiring 的 SyncBusMod 出错时保留总线）。
//
// 不配合的 handler 不会被终止。这里等的是传输自己的 consume 回调（交还连接的前提）；单个订阅的排空
// 由 Subscription.Unsubscribe 负责，两者互不依赖。
func (b *jetStreamSyncBus) StopWithContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.deliveries.Stop()
	b.cancelSetup()
	b.stopSubscriptions()
	if err := b.consumers.Wait(ctx); err != nil {
		return err
	}
	return b.deliveries.Wait(ctx)
}

func (b *jetStreamSyncBus) stopSubscriptions() {
	b.mu.Lock()
	retiring := make(map[string]*topicFanout)
	for key, f := range b.topics {
		if !f.retiring {
			f.retiring = true
			f.calls.Stop()
			retiring[key] = f
		}
	}
	b.mu.Unlock()
	for key, f := range retiring {
		b.retire(key, f)
	}
}

func (b *jetStreamSyncBus) subject(topic string) string {
	return fmt.Sprintf("%s.%s", b.cfg.Prefix, topic)
}

// JetStreamSyncStream 是 prefix 缺省对应的流名。
//
// 流名必须跟随 prefix：prefix 不同的两个部署共用一个 NATS 时，若共用一个流名，EnsureStream
// （CreateOrUpdateStream）会把流的 subjects 改成后启动者的 prefix，先启动者的发布从此不再入流。
// 映射规则：
//   - 默认 prefix（roost.sync，或留空）仍是 ROOST_SYNC——已部署的流与其上的 durable 游标不变；
//   - 只由小写字母、数字和单个 "." 分隔组成的 prefix 大写并把 "." 换成 "_"
//     （zz3640.sync → ZZ3640_SYNC），这一映射可逆，不同 prefix 不会相撞；
//   - 其他 prefix（含 "_"、"-"、大写等）在同样折叠后追加 prefix 的摘要，
//     折叠后相同的两个 prefix（zz.sync 与 zz_sync）仍得到不同的流。
func JetStreamSyncStream(prefix string) string {
	if prefix == "" || prefix == defaultJetStreamSyncPrefix {
		return defaultJetStreamSyncStream
	}
	name := make([]byte, 0, len(prefix))
	reversible := true
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		switch {
		case c >= 'a' && c <= 'z':
			name = append(name, c-'a'+'A')
		case c >= '0' && c <= '9':
			name = append(name, c)
		case c == '.':
			// 开头、结尾或连续的 "." 不是合法的 subject 分段，也就不可逆。
			if i == 0 || i == len(prefix)-1 || prefix[i-1] == '.' {
				reversible = false
			}
			name = append(name, '_')
		case c >= 'A' && c <= 'Z', c == '_', c == '-':
			name = append(name, c)
			reversible = false
		default:
			name = append(name, '_')
			reversible = false
		}
	}
	stream := strings.Trim(string(name), "_")
	if len(stream) > 200 {
		stream, reversible = stream[:200], false
	}
	if reversible {
		return stream
	}
	if stream == "" {
		stream = "SYNC"
	}
	sum := sha256.Sum256([]byte(prefix))
	return fmt.Sprintf("%s_%X", stream, sum[:8])
}

// syncMsgID 返回 JetStream 去重键：优先用消息自带的 MessageID，没有时退回由
// (topic, key, version, sid, part) 拼出的键；缺任一字段则不设去重键。
func syncMsgID(msg *fsyncbus.SyncMsg) string {
	if msg == nil {
		return ""
	}
	if strings.TrimSpace(msg.MessageID) != "" {
		return msg.MessageID
	}
	if msg.Topic == "" || msg.Key == 0 || msg.Version == 0 || msg.FromSid == 0 {
		return ""
	}
	// 同一主题的所有发布端必须拼出相同的去重键；"room:" 前缀是既有 wire 字面量，保持不变。
	return fmt.Sprintf("room:%s:%d:%d:%d:%d", msg.Topic, msg.Key, msg.Version, msg.FromSid, msg.Part)
}

// durableSyncName is the consumer identity for one (prefix, topic, sid). The
// hash covers the FULL subject the consumer filters on whenever the prefix is
// not the default: otherwise two buses on one Stream with different prefixes
// would collapse onto one durable name with two different FilterSubjects. The
// default prefix hashes the topic alone, so the durable name of a default
// deployment stays stable and its ACK cursor survives restarts — renaming it
// would orphan the cursor and replay the retained stream.
func durableSyncName(prefix, topic string, sid int32) string {
	identity := topic
	if prefix != defaultJetStreamSyncPrefix {
		identity = prefix + "." + topic
	}
	raw := fmt.Sprintf("%s\x00%d", identity, sid)
	hash := sha256.Sum256([]byte(raw))
	readable := sanitizeSyncName(fmt.Sprintf("sync_%s_%d", topic, sid))
	if len(readable) > 180 {
		readable = readable[:180]
	}
	return fmt.Sprintf("%s_%x", strings.TrimRight(readable, "_"), hash[:8])
}

// PublishConfirmed satisfies syncstream's confirmation capability. JetStream
// Publish returns only after the server acknowledges persistence.
func (b *jetStreamSyncBus) PublishConfirmed(msg *fsyncbus.SyncMsg) error { return b.Publish(msg) }

func sanitizeSyncName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	b.Grow(len(s))
	lastUnderscore := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 200 {
		out = out[:200]
	}
	if out == "" {
		return "sync"
	}
	return out
}

var (
	_ fsyncbus.ISyncBus        = (*jetStreamSyncBus)(nil)
	_ fsyncbus.ILiveSubscriber = (*jetStreamSyncBus)(nil)
)
