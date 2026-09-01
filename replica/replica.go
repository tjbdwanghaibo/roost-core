package replica

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/cube-core/ctx"
	fsync "github.com/tjbdwanghaibo/cube-core/sync"
)

type Op uint8

const (
	OpUpsert Op = iota + 1
	OpDelete
)

type Envelope struct {
	Topic     string `json:"topic,omitempty"`
	Key       int64  `json:"key,omitempty"`
	Version   int64  `json:"version,omitempty"`
	Op        Op     `json:"op,omitempty"`
	Payload   []byte `json:"payload,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

type Store interface {
	ApplyReplica(ctx context.Context, env Envelope) error
}

type Replicator struct {
	bus     fsync.ISyncBus
	store   Store
	topic   string
	unsub   func()
	started bool
	mu      sync.Mutex
}

func New(bus fsync.ISyncBus, topic string, store Store) *Replicator {
	return &Replicator{bus: bus, topic: topic, store: store}
}

func (r *Replicator) Start() error {
	if r == nil {
		return fmt.Errorf("replica: replicator is nil")
	}
	if r.bus == nil || r.store == nil || r.topic == "" {
		return fmt.Errorf("replica: bus, store and topic are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	unsub, err := r.bus.Subscribe(r.topic, func(msg *fsync.SyncMsg) error {
		if msg == nil {
			return fmt.Errorf("replica: message is nil")
		}
		if msg.Key == 0 {
			return fmt.Errorf("replica: message key is zero")
		}
		if msg.Topic != "" && msg.Topic != r.topic {
			return fmt.Errorf("replica: outer topic mismatch: got %q want %q", msg.Topic, r.topic)
		}
		if len(msg.Data) == 0 {
			env := Envelope{Topic: r.topic, Key: msg.Key, Version: msg.Version, Op: OpDelete}
			return r.store.ApplyReplica(fctx.BaseContext(), env)
		}
		var env Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			return err
		}
		if env.Topic != "" && env.Topic != r.topic {
			return fmt.Errorf("replica: inner topic mismatch: got %q want %q", env.Topic, r.topic)
		}
		if env.Key != 0 && env.Key != msg.Key {
			return fmt.Errorf("replica: inner key mismatch: got %d want %d", env.Key, msg.Key)
		}
		if env.Version != 0 && env.Version != msg.Version {
			return fmt.Errorf("replica: inner version mismatch: got %d want %d", env.Version, msg.Version)
		}
		env.Topic = r.topic
		env.Key = msg.Key
		env.Version = msg.Version
		if env.Op == 0 {
			env.Op = OpUpsert
		}
		if env.Op != OpUpsert && env.Op != OpDelete {
			return fmt.Errorf("replica: unsupported operation %d", env.Op)
		}
		return r.store.ApplyReplica(fctx.BaseContext(), env)
	})
	if err != nil {
		return err
	}
	r.unsub = unsub
	r.started = true
	return nil
}

func (r *Replicator) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unsub != nil {
		r.unsub()
	}
	r.unsub = nil
	r.started = false
}

func (r *Replicator) Publish(ctx context.Context, env Envelope) error {
	if r == nil || r.bus == nil || r.topic == "" {
		return fmt.Errorf("replica: replicator is not initialized")
	}
	if env.Topic == "" {
		env.Topic = r.topic
	} else if env.Topic != r.topic {
		return fmt.Errorf("replica: envelope topic mismatch: got %q want %q", env.Topic, r.topic)
	}
	if env.Key == 0 {
		return fmt.Errorf("replica: envelope key is zero")
	}
	if env.Op == 0 {
		env.Op = OpUpsert
	}
	if env.Op != OpUpsert && env.Op != OpDelete {
		return fmt.Errorf("replica: unsupported operation %d", env.Op)
	}
	if env.UpdatedAt == 0 {
		env.UpdatedAt = time.Now().UnixMilli()
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	msg := &fsync.SyncMsg{
		Topic:   r.topic,
		Key:     env.Key,
		Version: env.Version,
		Data:    raw,
	}
	return publish(ctx, r.bus, msg)
}

func (r *Replicator) PublishDelete(ctx context.Context, key int64, version int64) error {
	if r == nil || r.bus == nil || r.topic == "" {
		return fmt.Errorf("replica: replicator is not initialized")
	}
	if key == 0 {
		return fmt.Errorf("replica: delete key is zero")
	}
	return publish(ctx, r.bus, &fsync.SyncMsg{
		Topic:   r.topic,
		Key:     key,
		Version: version,
	})
}

func publish(ctx context.Context, bus fsync.ISyncBus, msg *fsync.SyncMsg) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if publisher, ok := bus.(fsync.IContextPublisher); ok {
		return publisher.PublishContext(ctx, msg)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return bus.Publish(msg)
}

func MarshalPayload(v any) ([]byte, error) {
	return json.Marshal(v)
}

func UnmarshalPayload[T any](env Envelope) (T, error) {
	var ret T
	err := json.Unmarshal(env.Payload, &ret)
	return ret, err
}
