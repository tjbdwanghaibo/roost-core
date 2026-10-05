package driver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
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

type JetStreamSyncConfig struct {
	LocalSid int32
	Prefix   string
	// Stream 是承载 Prefix.> 的 JetStream 流名。留空时由 Prefix 派生（JetStreamSyncStream）：
	// 默认 prefix roost.sync 仍是 ROOST_SYNC，其他 prefix 各得其流（RR-20260926-56）。
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

type jetStreamSyncBus struct {
	js  fnats.IJetStream
	cfg JetStreamSyncConfig

	// mu guards topics and is held across the underlying Subscribe of a
	// topic's first subscriber, so concurrent first subscribers cannot both
	// create the consumer and Stop cannot race a creation in flight.
	mu     sync.Mutex
	topics map[string]*topicFanout

	// RR-20261005-NC-172：consume 回调（及其中的本地 handler）在 nats.go 的回调 goroutine 上执行，
	// ConsumeContext.Stop 不等它们。这里是总线自己的准入与在途计数：停止先关准入、停订阅，再在
	// 调用方 ctx 内等在途归零；归零之前总线与连接都不能交还。
	deliveryMu      sync.Mutex
	stopping        bool
	deliveriesInUse int
	deliveriesIdle  chan struct{} // 关闭准入且没有在途回调时关闭
}

// errJetStreamSyncStopping 让停止开始后才到达的投递回到 broker（driver 据此 NAK），由下一次
// 消费同一 durable 的实例处理，不在停止中的总线上执行业务 handler。
var errJetStreamSyncStopping = errors.New("jetstream sync: bus is stopping; delivery returned to the broker")

func (b *jetStreamSyncBus) beginDelivery() bool {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	if b.stopping {
		return false
	}
	b.deliveriesInUse++
	return true
}

func (b *jetStreamSyncBus) endDelivery() {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	b.deliveriesInUse--
	if b.stopping && b.deliveriesInUse == 0 {
		close(b.deliveriesIdle)
	}
}

// closeDeliveries 幂等地关闭准入，返回最后一个在途回调返回时关闭的通道。
func (b *jetStreamSyncBus) closeDeliveries() <-chan struct{} {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	if !b.stopping {
		b.stopping = true
		if b.deliveriesInUse == 0 {
			close(b.deliveriesIdle)
		}
	}
	return b.deliveriesIdle
}

func (b *jetStreamSyncBus) deliveriesStopped() bool {
	b.deliveryMu.Lock()
	defer b.deliveryMu.Unlock()
	return b.stopping
}

// topicFanout is one topic's single durable consumer plus every local
// handler registered on it (U-0209, RR-20260916-03). ISyncBus.Subscribe is
// broadcast: the plain NATS bus gives every local subscriber every message.
// A durable JetStream consumer is a work queue — two Consume calls on the
// same consumer name split the stream between them — so one underlying
// subscription per topic fans out locally instead of one per Subscribe.
type topicFanout struct {
	sub      fnats.IJetStreamSubscription
	handlers map[uint64]fsyncbus.Handler
	nextID   uint64
}

func (f *topicFanout) snapshot() []fsyncbus.Handler {
	ids := make([]uint64, 0, len(f.handlers))
	for id := range f.handlers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]fsyncbus.Handler, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.handlers[id])
	}
	return out
}

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
	return &jetStreamSyncBus{js: js, cfg: cfg, topics: make(map[string]*topicFanout), deliveriesIdle: make(chan struct{})}, nil
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

func (b *jetStreamSyncBus) Subscribe(topic string, handler fsyncbus.Handler) (func(), error) {
	if b == nil || b.js == nil {
		return nil, fmt.Errorf("jetstream sync: bus is not initialized")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, fmt.Errorf("jetstream sync: topic is empty")
	}
	if handler == nil {
		return nil, fmt.Errorf("jetstream sync: handler is nil")
	}
	cfg := b.cfg
	b.mu.Lock()
	defer b.mu.Unlock()
	// 停止开始后不再建消费者：它的投递都会被准入拒绝、一轮轮 NAK 到 MaxDeliver。
	if b.deliveriesStopped() {
		return nil, fmt.Errorf("jetstream sync: bus is stopping or stopped")
	}
	fanout := b.topics[topic]
	if fanout == nil {
		// First local subscriber: create the topic's one durable consumer.
		// Its handler dispatches to whatever handlers are registered at
		// delivery time, so later subscribers only need to register.
		fanout = &topicFanout{handlers: make(map[uint64]fsyncbus.Handler)}
		name := durableSyncName(cfg.Prefix, topic, cfg.LocalSid)
		ctx, cancel := context.WithTimeout(fctx.BaseContext(), cfg.SetupTimeout)
		defer cancel()
		sub, err := b.js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
			Stream:        cfg.Stream,
			Name:          name,
			Durable:       name,
			FilterSubject: b.subject(topic),
			DeliverPolicy: fnats.JetStreamDeliverAll,
			AckWait:       cfg.AckWait,
			MaxDeliver:    cfg.MaxDeliver,
		}, func(_ context.Context, raw *fnats.JetStreamMsg) error {
			if raw == nil {
				return nil
			}
			if !b.beginDelivery() {
				return errJetStreamSyncStopping
			}
			defer b.endDelivery()
			var msg fsyncbus.SyncMsg
			if err := json.Unmarshal(raw.Data, &msg); err != nil {
				slog.Warn("jetstream sync: unmarshal failed", "topic", topic, "err", err)
				return nil
			}
			if msg.FromSid == cfg.LocalSid {
				return nil
			}
			b.mu.Lock()
			handlers := fanout.snapshot()
			b.mu.Unlock()
			for _, h := range handlers {
				// Each handler owns its copy: the plain bus hands every
				// subscriber its own message and a handler may mutate it.
				own := msg
				own.Data = append([]byte(nil), msg.Data...)
				b.invoke(topic, h, &own)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		fanout.sub = sub
		b.topics[topic] = fanout
	}
	fanout.nextID++
	id := fanout.nextID
	fanout.handlers[id] = handler
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(fanout.handlers, id)
			var stop fnats.IJetStreamSubscription
			if len(fanout.handlers) == 0 && b.topics[topic] == fanout {
				delete(b.topics, topic)
				stop = fanout.sub
			}
			b.mu.Unlock()
			if stop != nil {
				stop.Stop() // the last local subscriber releases the shared consumer
			}
		})
	}, nil
}

// invoke runs one local handler with the bus's existing error contract (log
// and continue) plus panic isolation, so one subscriber cannot take the
// delivery away from its siblings.
func (b *jetStreamSyncBus) invoke(topic string, handler fsyncbus.Handler, msg *fsyncbus.SyncMsg) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("jetstream sync: handler panic", "topic", topic, "key", msg.Key, "version", msg.Version, "panic", recovered)
		}
	}()
	if err := handler(msg); err != nil {
		slog.Warn("jetstream sync: handler error", "topic", topic, "key", msg.Key, "version", msg.Version, "err", err)
	}
}

// Stop 是不限时的 StopWithContext，给没有停机预算的调用方。
func (b *jetStreamSyncBus) Stop() {
	_ = b.StopWithContext(context.Background())
}

// StopWithContext 按三步停机（RR-20261005-NC-172）：
//  1. 关闭投递准入并停掉全部订阅（幂等；之后到达的投递返回 errJetStreamSyncStopping，由 broker 重投）；
//  2. 在 ctx 内等已准入的 consume 回调返回，超时返回 ctx 错误，重试再等同一批；
//  3. 返回 nil 之后调用方才能释放总线与它下面的 NATS 连接（kit SyncBusMod 出错时保留总线）。
//
// 不配合的 handler 不会被终止；单个本地订阅的退订函数不等待，等待只在总线停止时发生。
func (b *jetStreamSyncBus) StopWithContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	idle := b.closeDeliveries()
	b.stopSubscriptions()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *jetStreamSyncBus) stopSubscriptions() {
	b.mu.Lock()
	subs := make([]fnats.IJetStreamSubscription, 0, len(b.topics))
	for _, fanout := range b.topics {
		if fanout != nil && fanout.sub != nil {
			subs = append(subs, fanout.sub)
		}
	}
	b.topics = make(map[string]*topicFanout)
	b.mu.Unlock()
	for _, sub := range subs {
		sub.Stop()
	}
}

func (b *jetStreamSyncBus) subject(topic string) string {
	return fmt.Sprintf("%s.%s", b.cfg.Prefix, topic)
}

// JetStreamSyncStream 是 prefix 缺省对应的流名（RR-20260926-56）。
//
// 流名曾固定为 ROOST_SYNC：prefix 不同的两个部署共用一个 NATS 时，EnsureStream
// （CreateOrUpdateStream）把同一个流的 subjects 改成后启动者的 prefix，先启动者的发布从此
// 不再入流。现在流名跟随 prefix：
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

func syncMsgID(msg *fsyncbus.SyncMsg) string {
	// MessageID was added after the first SyncMsg release. Read it
	// reflectively so core and kit can be rolled out independently.
	if msg != nil {
		value := reflect.ValueOf(msg).Elem().FieldByName("MessageID")
		if value.IsValid() && value.Kind() == reflect.String && strings.TrimSpace(value.String()) != "" {
			return value.String()
		}
	}
	if msg == nil || msg.Topic == "" || msg.Key == 0 || msg.Version == 0 || msg.FromSid == 0 {
		return ""
	}
	// MessageID is the JetStream dedup key; every publisher of a topic must build
	// it identically, so the format is versioned by the package name, not the
	// deployment.
	return fmt.Sprintf("room:%s:%d:%d:%d:%d", msg.Topic, msg.Key, msg.Version, msg.FromSid, msg.Part)
}

// durableSyncName is the consumer identity for one (prefix, topic, sid). The
// hash covers the FULL subject the consumer filters on whenever the prefix is
// not the default: two buses on one Stream with different prefixes used to
// collapse onto one durable name with two different FilterSubjects
// (U-0210, RR-20260916-02). The default prefix keeps the pre-U-0210 input
// byte for byte, so deployed consumers keep their ACK cursors across the
// upgrade — renaming them would orphan the cursor and replay the retained
// stream into every default deployment.
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

var _ fsyncbus.ISyncBus = (*jetStreamSyncBus)(nil)
