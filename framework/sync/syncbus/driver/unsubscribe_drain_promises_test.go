package driver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	corenats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

// A3 ②（docs/feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md）：排空下沉到传输层。
//
// 修前（旧接口 Subscribe 返回 func()）：handler 卡在屏障上时 unsub() 立即返回，回调仍在跑——订阅者
// 若此时释放依赖，回调就在被释放的依赖上运行（JetStream 与普通 NATS 都红，文本见方案 §9）。
// 承诺：Subscription.Unsubscribe(ctx) 在回调返回前不返回 nil（ctx 到期返回 ctx 错误），放行后重试返回
// nil，之后的投递不再调 handler；回调里用投递 ctx 退订自己不死锁。两个驱动各套一遍停机契约骨架。

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
