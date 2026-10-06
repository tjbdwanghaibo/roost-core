package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// N05：第二个订阅失败时，必须释放第一个订阅；同一 Assembly 的重试须恢复
// 两个订阅，重复 Start 不重复绑定，重复 Stop 后资源最终为零。
type retrySubscriptionBus struct {
	calls, active int
	failure       error
}

func (*retrySubscriptionBus) Publish(*fsyncbus.SyncMsg) error { return nil }

// SubscribeLive 让快照推送开着（Mirror 第 4 步），两个订阅都经同一个计数。
func (b *retrySubscriptionBus) SubscribeLive(topic string, h fsyncbus.Handler) (func(), error) {
	return b.Subscribe(topic, h)
}

func (b *retrySubscriptionBus) Subscribe(string, fsyncbus.Handler) (func(), error) {
	b.calls++
	if b.calls == 2 {
		return nil, b.failure
	}
	b.active++
	return sync.OnceFunc(func() { b.active-- }), nil
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
	if b.active != 2 || b.calls != 4 {
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
