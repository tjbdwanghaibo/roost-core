// 同步总线驱动的停机与退订：停机契约骨架、退订等在途回调、Mirror 在旧投递在途时停止与重启。

package driver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	corenats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// JetStream 同步总线的 StopWithContext 套共用停机契约骨架：卡住的工作是 consume 回调里的本地 handler；“资源”是调用方
// （SyncBusMod → NatsMod）在停止返回 nil 之后才交还的总线与连接（stopcontract.CallerReleases）。
// 历史：RR-20261005-NC-172。
func TestJetStreamSyncBusStopContract(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	var bus *jetStreamSyncBus
	entered, release := make(chan struct{}), make(chan struct{})
	delivered := make(chan error, 1)
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			var err error
			bus, err = NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
				if m.Key == 1 {
					close(entered)
					<-release
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(bus.StopWithContext)
		},
		Block: func(testing.TB) {
			go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not enter")
			}
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	if err := <-delivered; err != nil {
		t.Fatalf("the in-flight delivery = %v, want it acknowledged", err)
	}
}

// 排空在传输层：Subscription.Unsubscribe(ctx) 在回调返回前不返回 nil（ctx 到期返回 ctx 错误），放行后重试返回 nil，之后的投递不再调
// handler；回调里用投递 ctx 退订自己不死锁。两个驱动各套一遍停机契约骨架。

// drainBus 给出一个总线与它的投递入口。两个替身的底层退订都不撤掉回调（真实 nats.go 发 UNSUB 之后，
// 已出队的消息仍会交给回调；JetStream fanout 的回调拿着旧快照），所以“退订之后不再调 handler”验证的是
// Subscription 的准入，而不是替身。
type drainBus struct {
	name string
	bus  func(t *testing.T) (fsyncbus.ISyncBus, func(key int64))
}

func drainBuses() []drainBus {
	return []drainBus{
		{name: "jetstream", bus: func(t *testing.T) (fsyncbus.ISyncBus, func(int64)) {
			js := newFakeJetStream()
			bus, err := NewJetStreamSyncBus(t.Context(), js, JetStreamSyncConfig{LocalSid: 7})
			if err != nil {
				t.Fatal(err)
			}
			return bus, func(key int64) {
				// 消费者停掉之后 fake 仍保留回调，模拟 consume 回调拿着旧 fanout 快照。
				if h := js.handlers["roost.sync.state"]; h != nil {
					_ = h(context.Background(), &corenats.JetStreamMsg{Subject: "roost.sync.state", Data: fanoutMsg(t, key), NumDelivered: 1})
				}
			}
		}},
		{name: "nats", bus: func(t *testing.T) (fsyncbus.ISyncBus, func(int64)) {
			client := &capturingNatsClient{}
			return NewNatsSyncBus(client, 7, ""), func(key int64) { client.deliver(t, "roost.sync.state", key) }
		}},
	}
}

func TestUnsubscribeWaitsForInFlightHandler(t *testing.T) {
	for _, tc := range drainBuses() {
		t.Run(tc.name, func(t *testing.T) {
			bus, deliver := tc.bus(t)
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			sub, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
				calls.Add(1)
				if m.Key == 1 {
					close(entered)
					<-release
					close(returned)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			go deliver(1)
			<-entered
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := sub.Unsubscribe(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Unsubscribe with the handler still running = %v, want DeadlineExceeded", err)
			}
			select {
			case <-returned:
				t.Fatal("handler returned before release")
			default:
			}
			close(release)
			if err := sub.Unsubscribe(context.Background()); err != nil {
				t.Fatalf("retry after release = %v, want nil", err)
			}
			select {
			case <-returned:
			default:
				t.Fatal("Unsubscribe returned nil while the handler was still running")
			}
			deliver(2)
			if got := calls.Load(); got != 1 {
				t.Fatalf("handler calls = %d, want 1 (no delivery after Unsubscribe returned nil)", got)
			}
			if err := sub.Unsubscribe(context.Background()); err != nil {
				t.Fatalf("repeated Unsubscribe = %v, want nil", err)
			}
		})
	}
}

func TestUnsubscribeFromOwnHandlerDoesNotDeadlock(t *testing.T) {
	for _, tc := range drainBuses() {
		t.Run(tc.name, func(t *testing.T) {
			bus, deliver := tc.bus(t)
			var sub *fsyncbus.Subscription
			ready := make(chan struct{})
			result := make(chan error, 1)
			var calls atomic.Int32
			s, err := bus.Subscribe("state", func(ctx context.Context, _ *fsyncbus.SyncMsg) error {
				<-ready
				calls.Add(1)
				result <- sub.Unsubscribe(ctx)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			sub = s
			close(ready)
			done := make(chan struct{})
			go func() { deliver(1); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Unsubscribe inside the subscription's own handler deadlocked")
			}
			if err := <-result; err != nil {
				t.Fatalf("self Unsubscribe = %v, want nil", err)
			}
			deliver(2)
			if calls.Load() != 1 {
				t.Fatalf("handler calls = %d, want 1", calls.Load())
			}
		})
	}
}

func TestUnsubscribeStopContract(t *testing.T) {
	for _, tc := range drainBuses() {
		t.Run(tc.name, func(t *testing.T) {
			var deliver func(int64)
			var sub *fsyncbus.Subscription
			entered, release := make(chan struct{}), make(chan struct{})
			var stop func(context.Context) error
			var released func() bool
			stopcontract.Check(t, stopcontract.Hooks{
				Start: func(testing.TB) {
					var bus fsyncbus.ISyncBus
					bus, deliver = tc.bus(t)
					var err error
					sub, err = bus.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error {
						close(entered)
						<-release
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					stop, released = stopcontract.CallerReleases(sub.Unsubscribe)
				},
				Block: func(testing.TB) {
					go deliver(1)
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("handler did not enter")
					}
				},
				Stop:     func(ctx context.Context) error { return stop(ctx) },
				Release:  func() { close(release) },
				Released: func() bool { return released() },
			})
		})
	}
}

// capturingNatsClient 记下普通 NATS 订阅的回调，由测试按需投递。
type capturingNatsClient struct {
	recordingNatsClient
	mu       sync.Mutex
	handlers map[string]corenats.MsgHandler
}

func (c *capturingNatsClient) Subscribe(subject string, h corenats.MsgHandler) (corenats.ISubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handlers == nil {
		c.handlers = make(map[string]corenats.MsgHandler)
	}
	c.handlers[subject] = h
	return capturingNatsSub{c: c, subject: subject}, nil
}

func (c *capturingNatsClient) deliver(t *testing.T, subject string, key int64) {
	data, err := json.Marshal(&fsyncbus.SyncMsg{Topic: "state", Key: key, Version: 1, FromSid: 99, Data: []byte("x")})
	if err != nil {
		t.Error(err)
		return
	}
	c.mu.Lock()
	h := c.handlers[subject]
	c.mu.Unlock()
	if h != nil {
		h(&corenats.Msg{Subject: subject, Data: data})
	}
}

type capturingNatsSub struct {
	c       *capturingNatsClient
	subject string
}

// Unsubscribe 和 nats.go 一样只发 UNSUB、不等回调；回调登记留着，模拟 UNSUB 之后仍交出的已出队消息。
func (s capturingNatsSub) Unsubscribe() error { return nil }
func (s capturingNatsSub) IsValid() bool      { return true }

// N05：正式 JetStreamSyncBus 适配器上的 Stop 只退订，不排空已进入的 Store。
// 允许旧在途完成；重订阅、同 topic 其他订阅和最后一次退订的资源责任须保持正确。
type lifecycleMirrorStore struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	version int64
}

func lifecycleMirrorMessage(t *testing.T, version int64) []byte {
	t.Helper()
	env, err := json.Marshal(mirror.Envelope{Topic: "state", Key: 1, Version: version, Op: mirror.OpUpsert})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fsyncbus.SyncMsg{Topic: "state", Key: 1, Version: version, FromSid: 99, Data: env})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func (s *lifecycleMirrorStore) ApplyReplica(_ context.Context, e mirror.Envelope) error {
	if e.Version == 1 {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Version > s.version {
		s.version = e.Version
	}
	return nil
}
func TestMirrorStopAndRestartWithOldDeliveryInFlight(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	b, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.StopWithContext(context.Background()) })
	s := &lifecycleMirrorStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := mirror.New(b, "state", s)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	oldMessage := lifecycleMirrorMessage(t, 1)
	oldDone := make(chan error, 1)
	go func() { oldDone <- js.deliver("roost.sync.state", oldMessage) }()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("old callback did not enter")
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(s.release) }) })
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop unexpectedly waited for an in-flight handler")
	}
	select {
	case <-oldDone:
		t.Fatal("old callback finished before release")
	default:
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatal("last subscriber did not release underlying consumer")
	}
	// RR-25：退役代还在执行时重订必须明确失败，不能把新 consumer 开在旧回调旁边。
	if err := r.Start(); !errors.Is(err, fsyncbus.ErrSubscriptionBusy) {
		t.Fatalf("restart during retirement = %v", err)
	}
	if len(js.subs) != 1 {
		t.Fatal("retirement started another consumer")
	}
	releaseOnce.Do(func() { close(s.release) })
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	waitRetired(t, b, "state")
	for range 2 {
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if len(js.subs) != 2 {
		t.Fatalf("restart not idempotent: subscriptions=%d", len(js.subs))
	}
	if err := js.deliver("roost.sync.state", lifecycleMirrorMessage(t, 2)); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	version := s.version
	s.mu.Unlock()
	if version != 2 {
		t.Fatalf("new delivery lost to old callback: version=%d", version)
	}
	r.Stop()
	r.Stop()
	if js.subs[1].stops.Load() != 1 {
		t.Fatal("repeated Stop did not release exactly once")
	}
}
