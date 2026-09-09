package cache

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// U-0155 · C2 · RR-20260908-02：MaxWaitersPerKey 限制的是"正在等这次 load 的跟随者"，不是"这次
// load 期间累计进过门的次数"。一个跟随者因取消离开后名额要归还，否则慢后端叠加短超时重试
// 会在首个 load 结束前一直拒绝健康请求；真的满额时仍要拒绝。
func TestReadThroughReturnsACanceledWaitersSlot(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	store := NewReadThroughStore[int, int](nil, nil, func(context.Context, int) (int, bool, error) {
		close(started)
		<-release
		return 7, true, nil
	}, StoreConfig[int, int]{KeyOf: func(v int) int { return v }}, ReadThroughOptions{MaxWaitersPerKey: 1})
	go func() { defer close(finished); _, _, _ = store.Get(context.Background(), 7) }()
	<-started
	defer func() { close(release); <-finished }()

	wait := func() (context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, _, err := store.Get(ctx, 7); done <- err }()
		return cancel, done
	}
	awaitCoalesced := func(n uint64, done chan error) {
		for store.Stats().Coalesced < n {
			select {
			case err := <-done:
				t.Fatalf("waiter %d refused: %v", n, err)
			default:
				runtime.Gosched()
			}
		}
	}
	cancelFirst, first := wait()
	awaitCoalesced(1, first)
	// The slot is taken: a second live follower is refused.
	if _, _, err := store.Get(context.Background(), 7); !errors.Is(err, ErrLoadWaitersExceeded) {
		t.Fatalf("second follower with the slot taken = %v, want ErrLoadWaitersExceeded", err)
	}
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower = %v", err)
	}
	// The follower left, so the slot is free again.
	cancelNext, next := wait()
	awaitCoalesced(2, next)
	cancelNext()
	if err := <-next; !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement follower = %v", err)
	}
}
