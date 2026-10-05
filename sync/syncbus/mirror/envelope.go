package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
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
	bus     fsyncbus.ISyncBus
	ids     *fsyncbus.DeliveryIDs
	store   Store
	topic   string
	unsub   func()
	started bool
	mu      sync.Mutex
	// gate 是当前一次 Start 的投递准入与在途计数；Stop 关掉它并移进 draining，
	// StopWithContext 等 draining 里的在途 handler 全部返回（RR-20261005-NC-174）。
	gate     *deliveryGate
	draining []*deliveryGate
}

// deliveryGate 是一次订阅的 handler 准入与在途计数。退订（ISyncBus 的 unsub）不保证在途回调已返回，
// JetStream fanout 的回调还可能在退订之后用旧快照调到这个 handler；关闭后到达的投递不再进入 Store。
type deliveryGate struct {
	mu      sync.Mutex
	closed  bool
	running int
	idle    chan struct{} // 关闭准入且没有在途 handler 时关闭
}

func newDeliveryGate() *deliveryGate { return &deliveryGate{idle: make(chan struct{})} }

func (g *deliveryGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.running++
	return true
}

func (g *deliveryGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	if g.closed && g.running == 0 {
		close(g.idle)
	}
}

// close 幂等地关闭准入。
func (g *deliveryGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	if g.running == 0 {
		close(g.idle)
	}
}

func New(bus fsyncbus.ISyncBus, topic string, store Store) *Replicator {
	return &Replicator{bus: bus, ids: fsyncbus.NewDeliveryIDs("mirror"), topic: topic, store: store}
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
	gate := newDeliveryGate()
	unsub, err := r.bus.Subscribe(r.topic, func(msg *fsyncbus.SyncMsg) error {
		if !gate.enter() {
			return nil // 这次订阅已停止：等同于退订先一步生效
		}
		defer gate.leave()
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
	r.gate = gate
	r.started = true
	return nil
}

// Stop 发起停止：关闭当前订阅的准入并退订，不等待已进入 Store 的 handler（幂等）。
// 需要“返回即已静止”的调用方用 StopWithContext。
func (r *Replicator) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gate != nil {
		r.gate.close()
		r.draining = append(r.draining, r.gate)
		r.gate = nil
	}
	if r.unsub != nil {
		r.unsub()
	}
	r.unsub = nil
	r.started = false
}

// StopWithContext 按三步停机（RR-20261005-NC-174）：①Stop 关闭准入并退订（幂等）；②在 ctx 内等此前
// 每次订阅已准入的 handler 返回，超时返回 ctx 错误，重试再等同一批；③全部返回后才报告 nil，调用方
// 此后可以释放 Store 的依赖。不配合的 Store 不会被终止。
func (r *Replicator) StopWithContext(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.Stop()
	r.mu.Lock()
	pending := append([]*deliveryGate(nil), r.draining...)
	r.mu.Unlock()
	for _, gate := range pending {
		select {
		case <-gate.idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.draining = slices.DeleteFunc(r.draining, func(gate *deliveryGate) bool {
		select {
		case <-gate.idle:
			return true
		default:
			return false
		}
	})
	r.mu.Unlock()
	return nil
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
	// MessageID is the delivery identity, minted per publish. The version is
	// not one: an upsert and a delete at the same version must be two messages
	// to the transport, or broker dedup drops whichever arrives second.
	msg := &fsyncbus.SyncMsg{
		MessageID: r.ids.Next(),
		Topic:     r.topic,
		Key:       env.Key,
		Version:   env.Version,
		Data:      raw,
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
	return publish(ctx, r.bus, &fsyncbus.SyncMsg{
		MessageID: r.ids.Next(),
		Topic:     r.topic,
		Key:       key,
		Version:   version,
	})
}

func publish(ctx context.Context, bus fsyncbus.ISyncBus, msg *fsyncbus.SyncMsg) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if publisher, ok := bus.(fsyncbus.IContextPublisher); ok {
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
