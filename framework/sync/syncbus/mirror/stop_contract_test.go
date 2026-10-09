package mirror

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// A3 / RR-20261005-NC-174：Replicator.StopWithContext 套共用停机契约骨架。卡住的工作是已进入 ApplyReplica
// 的 handler；Replicator 自己不持有 Store 的依赖，“资源”是调用方在停止返回 nil 之后才释放的依赖
// （stopcontract.CallerReleases）。
func TestReplicatorStopContract(t *testing.T) {
	bus := &lingeringBus{}
	store := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	rep := New(bus, "topic", store)
	done := make(chan error, 1)
	stop, released := stopcontract.CallerReleases(rep.StopWithContext)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			if err := rep.Start(); err != nil {
				t.Fatal(err)
			}
		},
		Block: func(testing.TB) {
			handler := bus.last()
			go func() { done <- handler(replicaMsg(t, 1)) }()
			select {
			case <-store.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not enter the store")
			}
		},
		Stop:     stop,
		Release:  func() { close(store.release) },
		Released: released,
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
