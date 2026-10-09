package remoteentity

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// A3 / RR-20261005-NC-174：Assembly.Stop 停复制的部分套共用停机契约骨架。卡住的工作是已进入 ApplyReplica
// 的兴趣续期（测试持有兴趣表的锁）；Assembly 不持有 Redis / SyncBus，“资源”是 kit Mod / App 在 Stop 返回
// nil 之后才停的这些依赖（stopcontract.CallerReleases）。
func TestRemoteAssemblyStopContract(t *testing.T) {
	var a *Assembly
	bus := &handlerBus{}
	applied := make(chan error, 1)
	var unlock func()
	var stop func(context.Context) error
	var released func() bool
	interest := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9472), ConsumerSID: 9, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 1}
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			a, _ = newLifecycleAssembly(t, func(context.Context) error { return nil })
			if err := a.Start(context.Background(), bus); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(a.Stop)
		},
		Block: func(testing.TB) {
			handler := bus.handler(SyncTopicInterest)
			if handler == nil {
				t.Fatal("interest replicator did not subscribe")
			}
			registry := a.Manager.snapshots.interests
			registry.mu.Lock()
			unlock = sync.OnceFunc(registry.mu.Unlock)
			go func() { applied <- handler(interestRenewMessage(t, interest)) }()
			waitForGoroutineIn(t, "remoteInterestRegistry).renewIfNeeded")
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { unlock() },
		Released: func() bool { return released() },
	})
	if err := <-applied; err != nil {
		t.Fatalf("in-flight renew: %v", err)
	}
	if !a.Manager.snapshots.interests.interested(interest.Key) {
		t.Fatal("the admitted renewal was lost")
	}
}
