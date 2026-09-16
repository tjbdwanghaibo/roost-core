package room

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
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
	LocalSid     int32
	Prefix       string
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
		cfg.Stream = defaultJetStreamSyncStream
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
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
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
		return nil, fmt.Errorf("jetstream sync: ensure stream: %w", err)
	}
	return &jetStreamSyncBus{js: js, cfg: cfg, topics: make(map[string]*topicFanout)}, nil
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

func (b *jetStreamSyncBus) Stop() {
	if b == nil {
		return
	}
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
