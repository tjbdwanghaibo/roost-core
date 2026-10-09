package syncbus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

// A3 ②（docs/feature/A3-2-SYNCBUS-DRAINING-UNSUBSCRIBE-2026-10-07.md）：排空在传输层的 Subscription 里实现一次。
//
// 承诺：Unsubscribe(ctx) 返回 nil 之后没有在途回调、也不会再有新回调；ctx 先结束返回 ctx 错误，放行后重试
// 返回 nil；重复退订返回 nil；回调里用投递 ctx 退订自己不死锁，且仍等其他在途回调。各驱动的同一组承诺
// 见 sync/syncbus/driver 的 unsubscribe_drain_promises_test.go。

func TestSubscriptionUnsubscribeStopContract(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var sub *Subscription
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			sub = NewSubscription("state", func(context.Context, *SyncMsg) error {
				close(entered)
				<-release
				return nil
			}, nil)
			stop, released = stopcontract.CallerReleases(sub.Unsubscribe)
		},
		Block: func(testing.TB) {
			go func() { _ = sub.Deliver(context.Background(), &SyncMsg{Key: 1}) }()
			<-entered
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
}

func TestSubscriptionRefusesDeliveriesAfterUnsubscribe(t *testing.T) {
	var calls atomic.Int32
	released := false
	sub := NewSubscription("state", func(context.Context, *SyncMsg) error { calls.Add(1); return nil }, func() { released = true })
	if err := sub.Deliver(context.Background(), &SyncMsg{Key: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sub.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("release was not called")
	}
	if err := sub.Deliver(context.Background(), &SyncMsg{Key: 2}); !errors.Is(err, ErrUnsubscribed) {
		t.Fatalf("Deliver after Unsubscribe = %v, want ErrUnsubscribed", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1 (no call after Unsubscribe returned)", calls.Load())
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sub.Unsubscribe(canceled); err != nil {
		t.Fatalf("repeated Unsubscribe with a canceled ctx = %v, want nil", err)
	}
	var nilSub *Subscription
	if err := nilSub.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 回调里退订自己：用投递 ctx 时不等自己（不死锁），但仍等同一订阅的其他在途回调。
func TestSubscriptionSelfUnsubscribeWaitsOnlyForOthers(t *testing.T) {
	otherEntered, otherRelease := make(chan struct{}), make(chan struct{})
	selfResult := make(chan error, 1)
	var sub *Subscription
	sub = NewSubscription("state", func(ctx context.Context, msg *SyncMsg) error {
		switch msg.Key {
		case 1: // 另一个在途回调
			close(otherEntered)
			<-otherRelease
		case 2: // 退订自己
			selfResult <- sub.Unsubscribe(ctx)
		}
		return nil
	}, nil)
	go func() { _ = sub.Deliver(context.Background(), &SyncMsg{Key: 1}) }()
	<-otherEntered
	selfDone := make(chan struct{})
	go func() { _ = sub.Deliver(context.Background(), &SyncMsg{Key: 2}); close(selfDone) }()
	select {
	case err := <-selfResult:
		t.Fatalf("self Unsubscribe returned %v while another callback was in flight", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(otherRelease)
	select {
	case err := <-selfResult:
		if err != nil {
			t.Fatalf("self Unsubscribe = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("self Unsubscribe deadlocked")
	}
	<-selfDone
	if err := sub.Unsubscribe(context.Background()); err != nil {
		t.Fatalf("Unsubscribe after the self-unsubscribing callback returned = %v, want nil", err)
	}
}

// 同步重入：handler 同步投递回自己（内存总线发布即投递），内层回调退订自己，外层调用也不能等自己。
func TestSubscriptionSelfUnsubscribeInReentrantDelivery(t *testing.T) {
	result := make(chan error, 1)
	var sub *Subscription
	sub = NewSubscription("state", func(ctx context.Context, msg *SyncMsg) error {
		if msg.Key == 1 {
			return sub.Deliver(ctx, &SyncMsg{Key: 2})
		}
		result <- sub.Unsubscribe(ctx)
		return nil
	}, nil)
	done := make(chan error, 1)
	go func() { done <- sub.Deliver(context.Background(), &SyncMsg{Key: 1}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reentrant self Unsubscribe deadlocked")
	}
	if err := <-result; err != nil {
		t.Fatalf("self Unsubscribe = %v, want nil", err)
	}
	if err := sub.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 回调里退订自己却传入无关 ctx：等自己，带期限的 ctx 到期返回 ctx 错误（不是永久死锁）；
// 回调返回之后重试返回 nil。
func TestSubscriptionSelfUnsubscribeWithForeignContextTimesOut(t *testing.T) {
	result := make(chan error, 1)
	var sub *Subscription
	sub = NewSubscription("state", func(context.Context, *SyncMsg) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		result <- sub.Unsubscribe(ctx)
		return nil
	}, nil)
	if err := sub.Deliver(context.Background(), &SyncMsg{Key: 1}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("self Unsubscribe with a foreign ctx = %v, want DeadlineExceeded", err)
	}
	if err := sub.Unsubscribe(context.Background()); err != nil {
		t.Fatalf("retry after the callback returned = %v, want nil", err)
	}
}

// handler panic 时准入照样归还，退订不会卡住。
func TestSubscriptionPanicReleasesAdmission(t *testing.T) {
	sub := NewSubscription("state", func(context.Context, *SyncMsg) error { panic("boom") }, nil)
	func() {
		defer func() { _ = recover() }()
		_ = sub.Deliver(context.Background(), &SyncMsg{Key: 1})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sub.Unsubscribe(ctx); err != nil {
		t.Fatalf("Unsubscribe after a panicking handler = %v, want nil", err)
	}
}
