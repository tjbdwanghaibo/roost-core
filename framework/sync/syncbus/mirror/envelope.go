package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
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

// ErrLiveSubscribeUnsupported 表示总线不能确认订阅（没有实现 fsyncbus.ILiveSubscriber），NewLive 的
// 复制器无法启动。调用方据此显式退化，不要改用普通订阅假装有推送一致性。
var ErrLiveSubscribeUnsupported = errors.New("replica: the sync bus cannot confirm subscriptions (fsyncbus.ILiveSubscriber)")

type Replicator struct {
	bus   fsyncbus.ISyncBus
	ids   *fsyncbus.DeliveryIDs
	store Store
	topic string
	// live 为 true 时经 fsyncbus.ILiveSubscriber 订阅（NewLive）：Start 返回 nil 之后发布的消息不被静默丢掉。
	live bool
	mu   sync.Mutex
	// subs 是还没确认排空的订阅，started 时最后一个是当前订阅。排空由传输负责（A3 ②）：
	// Subscription.Unsubscribe(ctx) 返回 nil 即这个订阅不再有在途或新的 Store 调用（RR-20261005-NC-174
	// 原来在这里自己维护的准入门已删除）。每次 Start 一个新订阅，Replicator 可重启。
	subs    []*fsyncbus.Subscription
	started bool
}

func New(bus fsyncbus.ISyncBus, topic string, store Store) *Replicator {
	return &Replicator{bus: bus, ids: fsyncbus.NewDeliveryIDs("mirror"), topic: topic, store: store}
}

// NewLive 建一个用可确认订阅的复制器（Mirror 第 4 步）：Start 经 fsyncbus.ILiveSubscriber 订阅，
// 返回 nil 即订阅已确认；总线没有这项能力时 Start 返回 ErrLiveSubscribeUnsupported。发布与 New 相同。
func NewLive(bus fsyncbus.ISyncBus, topic string, store Store) *Replicator {
	r := New(bus, topic, store)
	r.live = true
	return r
}

// Live 报告这个复制器的订阅是否可确认。
func (r *Replicator) Live() bool { return r != nil && r.live }

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
	subscribe := r.bus.Subscribe
	if r.live {
		live, ok := r.bus.(fsyncbus.ILiveSubscriber)
		if !ok {
			return ErrLiveSubscribeUnsupported
		}
		subscribe = live.SubscribeLive
	}
	sub, err := subscribe(r.topic, func(ctx context.Context, msg *fsyncbus.SyncMsg) error {
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
			return r.store.ApplyReplica(ctx, env)
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
		return r.store.ApplyReplica(ctx, env)
	})
	if err != nil {
		return err
	}
	r.subs = append(r.subs, sub)
	r.started = true
	return nil
}

// Stop 发起停止：退订当前订阅（关准入、撤传输登记），不等已进入 Store 的 handler（幂等）。
// 需要“返回即已静止”的调用方用 StopWithContext。
func (r *Replicator) Stop() {
	_ = r.StopWithContext(initiateOnly) // 只做第 1 步；排空由之后的 StopWithContext 等
}

// initiateOnly 是已取消的 ctx：Unsubscribe 用它只发起退订、不等待（已排空时照样返回 nil）。
var initiateOnly = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// StopWithContext 按三步停机（RR-20261005-NC-174）：①退订（幂等）；②在 ctx 内等此前每次订阅的在途
// handler 返回（传输的 Subscription.Unsubscribe，A3 ②），超时返回 ctx 错误，重试再等同一批；③全部返回后
// 才报告 nil，调用方此后可以释放 Store 的依赖。不配合的 Store 不会被终止。
func (r *Replicator) StopWithContext(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.started = false
	pending := slices.Clone(r.subs)
	r.mu.Unlock()
	// 每个订阅都要发起退订：ctx 到期后剩下的 Unsubscribe 只发起、立即返回 ctx 错误。
	var firstErr error
	for _, sub := range pending {
		if err := sub.Unsubscribe(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// 只移除已排空的：等待期间 Start 可能又追加了新的订阅，留给下一次停止。
	r.mu.Lock()
	r.subs = slices.DeleteFunc(r.subs, func(sub *fsyncbus.Subscription) bool { return slices.Contains(pending, sub) })
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
