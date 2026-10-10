package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type handlerBus struct {
	mu       sync.Mutex
	handlers map[string]*fsyncbus.Subscription
}

func (*handlerBus) Publish(*fsyncbus.SyncMsg) error { return nil }
func (b *handlerBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handlers == nil {
		b.handlers = make(map[string]*fsyncbus.Subscription)
	}
	sub := fsyncbus.NewSubscription(topic, h, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.handlers, topic)
	})
	b.handlers[topic] = sub
	return sub, nil
}

// SubscribeLive：测试直接投递，订阅即确认（Mirror 第 4 步的推送模式）。
func (b *handlerBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

// handler 返回 topic 当前订阅的投递入口；没有订阅时返回 nil。
func (b *handlerBus) handler(topic string) func(*fsyncbus.SyncMsg) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub := b.handlers[topic]
	if sub == nil {
		return nil
	}
	return func(msg *fsyncbus.SyncMsg) error { return sub.Deliver(context.Background(), msg) }
}

func interestRenewMessage(t *testing.T, interest entity.RemoteSnapshotInterest) *fsyncbus.SyncMsg {
	t.Helper()
	raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: interest})
	if err != nil {
		t.Fatal(err)
	}
	env := mirror.Envelope{Topic: SyncTopicInterest, Key: remoteInterestReplicaKey(interest), Version: interest.ExpiresAt, Op: mirror.OpUpsert, Payload: raw}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &fsyncbus.SyncMsg{Topic: SyncTopicInterest, Key: env.Key, Version: env.Version, Data: data}
}

// waitForGoroutineIn 等到某个 goroutine 的栈里出现 frame（被测试持有的锁卡住），有界。
func waitForGoroutineIn(t *testing.T, frame string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), frame) {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine reached %s", frame)
}

// awaitStopOrCancelWhileWaiting 返回 stopped 的结果；若停止先进入 waitingFrame（在等排空），先 cancel 再取结果。
func awaitStopOrCancelWhileWaiting(t *testing.T, stopped <-chan error, cancel context.CancelFunc, waitingFrame string) error {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			return err
		default:
		}
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), waitingFrame) {
			cancel()
			select {
			case err := <-stopped:
				return err
			case <-time.After(5 * time.Second):
				t.Fatal("Stop ignored its context while waiting for the handler")
			}
		}
		runtime.Gosched()
	}
	t.Fatal("Stop neither returned nor waited")
	return nil
}

func TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler(t *testing.T) {
	a, _ := newLifecycleAssembly(t, func(context.Context) error { return nil })
	bus := &handlerBus{}
	if err := a.Start(context.Background(), bus); err != nil {
		t.Fatal(err)
	}
	handler := bus.handler(SyncTopicInterest)
	if handler == nil {
		t.Fatal("interest replicator did not subscribe")
	}
	interest := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9471), ConsumerSID: 9, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 1}

	// 测试持有兴趣表的锁：投递进入 ApplyReplica 后卡在 renew 上。
	registry := a.Manager.snapshots.interests
	registry.mu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(registry.mu.Unlock) }
	defer unlock()
	applied := make(chan error, 1)
	go func() { applied <- handler(interestRenewMessage(t, interest)) }()
	waitForGoroutineIn(t, "remoteInterestRegistry).renewIfNeeded")

	// 第一次停止：在它等待在途 handler 时取消（预算耗尽）；修前 Stop 不等，直接返回。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- a.Stop(ctx) }()
	stopErr := awaitStopOrCancelWhileWaiting(t, stopped, cancel, "mirror.(*Replicator).StopWithContext")
	if stopErr == nil {
		t.Errorf("Stop reported success while a replica handler was still applying")
	} else if !errors.Is(stopErr, context.Canceled) {
		t.Errorf("Stop = %v, want context.Canceled", stopErr)
	}
	select {
	case err := <-applied:
		t.Fatalf("handler finished while the test still held the registry lock: %v", err)
	default:
	}

	// 退订已经发生：新投递不再进入；handler 放行后用新 ctx 重试，等到它返回才报告成功。
	if bus.handler(SyncTopicInterest) != nil {
		t.Error("timed-out Stop left the interest subscription in place")
	}
	unlock()
	if err := <-applied; err != nil {
		t.Fatalf("in-flight renew: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop after the handler returned = %v", err)
	}
	if !a.Manager.snapshots.interests.interested(interest.Key) {
		t.Fatal("the admitted renewal was lost")
	}
}

type retrySubscriptionBus struct {
	calls, active int
	failure       error
}

func (*retrySubscriptionBus) Publish(*fsyncbus.SyncMsg) error { return nil }

// SubscribeLive 让快照推送开着（Mirror 第 4 步），全部订阅都经同一个计数。
func (b *retrySubscriptionBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

func (b *retrySubscriptionBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.calls++
	if b.calls == 2 {
		return nil, b.failure
	}
	b.active++
	return fsyncbus.NewSubscription(topic, h, func() { b.active-- }), nil
}

func TestRemoteAssemblyRetryAfterSecondSubscriptionFailure(t *testing.T) {
	a, _ := newLifecycleAssembly(t, func(context.Context) error { return nil })
	failure := errors.New("second subscription refused")
	b := &retrySubscriptionBus{failure: failure}
	if err := a.Start(context.Background(), b); !errors.Is(err, failure) {
		t.Fatalf("first Start: %v", err)
	}
	if b.active != 0 || b.calls != 2 {
		t.Fatalf("failed Start leaked: active=%d calls=%d", b.active, b.calls)
	}
	for range 2 {
		if err := a.Start(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	// 重试订阅全部三个（快照、兴趣、兴趣续租请求），总共 2 + 3 次调用。
	if b.active != liveSubscriptions || b.calls != 2+liveSubscriptions {
		t.Fatalf("retry binding: active=%d calls=%d", b.active, b.calls)
	}
	for range 2 {
		if err := a.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if b.active != 0 {
		t.Fatalf("Stop leaked subscriptions: %d", b.active)
	}
}
