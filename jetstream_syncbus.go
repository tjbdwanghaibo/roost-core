package room

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/cube-core/fctx"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/cube-core/syncbus"
)

const (
	defaultJetStreamSyncPrefix       = "cube.sync"
	defaultJetStreamSyncStream       = "CUBE_SYNC"
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

	mu   sync.Mutex
	subs []*syncSubscription
}

type syncSubscription struct {
	sub fnats.IJetStreamSubscription
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
	return &jetStreamSyncBus{js: js, cfg: cfg}, nil
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
	name := durableSyncName(topic, cfg.LocalSid)
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
		if err := handler(&msg); err != nil {
			slog.Warn("jetstream sync: handler error", "topic", topic, "key", msg.Key, "version", msg.Version, "err", err)
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	entry := &syncSubscription{sub: sub}
	b.mu.Lock()
	b.subs = append(b.subs, entry)
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			sub.Stop()
			b.mu.Lock()
			for i, candidate := range b.subs {
				if candidate == entry {
					b.subs = append(b.subs[:i], b.subs[i+1:]...)
					break
				}
			}
			b.mu.Unlock()
		})
	}, nil
}

func (b *jetStreamSyncBus) Stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	subs := append([]*syncSubscription(nil), b.subs...)
	b.subs = nil
	b.mu.Unlock()
	for _, entry := range subs {
		if entry != nil && entry.sub != nil {
			entry.sub.Stop()
		}
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
	// The "sync:" prefix is wire state, not a package name: it is the
	// JetStream dedup MessageID. Renaming it would make old and new nodes
	// compute different IDs for the same logical message during a rolling
	// upgrade, which is exactly the duplicate delivery dedup exists to stop.
	return fmt.Sprintf("sync:%s:%d:%d:%d:%d", msg.Topic, msg.Key, msg.Version, msg.FromSid, msg.Part)
}

func durableSyncName(topic string, sid int32) string {
	raw := fmt.Sprintf("%s\x00%d", topic, sid)
	hash := sha256.Sum256([]byte(raw))
	prefix := sanitizeSyncName(fmt.Sprintf("sync_%s_%d", topic, sid))
	if len(prefix) > 180 {
		prefix = prefix[:180]
	}
	return fmt.Sprintf("%s_%x", strings.TrimRight(prefix, "_"), hash[:8])
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
