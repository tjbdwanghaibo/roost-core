package driver

import (
	"context"
	"errors"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRPCBudgetAssemblyCloseBoundsCallbackDrain(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); r.Stop() })
	r.pending.Store(int64(1), &pendingCall{cb: func([]byte, error) { close(entered); <-release }})
	r.finishPending(1, nil, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback never entered; not a bug counterexample")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	asm := &Assembly{RPC: r}
	closed := make(chan error, 1)
	go func() { closed <- asm.Close(ctx) }()
	returned := false
	var err error
	select {
	case err = <-closed:
		returned = true
	case <-time.After(100 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	if !returned {
		select {
		case err = <-closed:
		case <-time.After(time.Second):
			t.Fatal("close did not finish even after releasing callback")
		}
	}
	t.Logf("returned_before_callback_release=%v close_error=%v", returned, err)
	if !returned || !errors.Is(err, context.Canceled) {
		t.Fatal("Assembly.Close ignored cancellation while draining a confirmed running callback")
	}
}

func TestRPCBudgetPendingTerminalRacesCompleteOnce(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 2)
	defer r.Stop()
	var calls atomic.Int32
	done := make(chan struct{}, 1)
	r.pending.Store(int64(7), &pendingCall{cb: func([]byte, error) { calls.Add(1); done <- struct{}{} }})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; r.finishPending(7, nil, fnats.ErrTimeout) }()
	}
	close(start)
	wg.Wait()
	r.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminal callback lost")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestRPCBudgetAssemblyClosesCompletedPool(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	if err := (&Assembly{RPC: r}).Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRPCBudgetStopRetainsFullQueueFallbackAndAllowsRetry(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	workerEntered, workerRelease := make(chan struct{}), make(chan struct{})
	fallbackEntered, fallbackRelease := make(chan struct{}), make(chan struct{})
	var workerOnce, fallbackOnce sync.Once
	t.Cleanup(func() {
		fallbackOnce.Do(func() { close(fallbackRelease) })
		workerOnce.Do(func() { close(workerRelease) })
		r.Stop()
	})
	r.dispatchCallback(1, &rpcTask{cb: func([]byte, error) { close(workerEntered); <-workerRelease }})
	select {
	case <-workerEntered:
	case <-time.After(time.Second):
		t.Fatal("worker callback did not enter")
	}
	var queued, fallback atomic.Int32
	for i := 0; i < 256; i++ {
		r.dispatchCallback(1, &rpcTask{cb: func([]byte, error) { queued.Add(1) }})
	}
	r.pending.Store(int64(2), &pendingCall{cb: func(_ []byte, err error) {
		if !errors.Is(err, fnats.ErrCancelled) {
			t.Error(err)
		}
		fallback.Add(1)
		close(fallbackEntered)
		<-fallbackRelease
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	asm := &Assembly{RPC: r}
	// RR-20261004-07（复审 S4）：有界等待，让一个忽略取消的 Close 变成断言失败而不是挂到 -timeout。
	closed := make(chan error, 1)
	go func() { closed <- asm.Close(ctx) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Assembly.Close with a canceled budget blocked on the full-queue fallback instead of returning")
	}
	select {
	case <-fallbackEntered:
	case <-time.After(time.Second):
		t.Fatal("full queue did not execute the stop fallback")
	}
	if err := r.StopWithContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry while blocked=%v", err)
	}
	if r.pool.Stats().Stopped {
		t.Fatal("pool reported stopped before pending fallback completed")
	}
	fallbackOnce.Do(func() { close(fallbackRelease) })
	workerOnce.Do(func() { close(workerRelease) })
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			budget, done := context.WithTimeout(context.Background(), time.Second)
			defer done()
			if err := asm.Close(budget); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if fallback.Load() != 1 || queued.Load() != 256 {
		t.Fatalf("fallback=%d queued=%d", fallback.Load(), queued.Load())
	}
}

func TestRPCBudgetCallbackCanCancelItsOwnStopWait(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	defer r.Stop()
	done := make(chan error, 1)
	r.dispatchCallback(1, &rpcTask{cb: func([]byte, error) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done <- r.StopWithContext(ctx)
	}})
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback waited on its own exit")
	}
}

func TestRPCBudgetStopFallbackPanicDoesNotLoseOtherPending(t *testing.T) {
	r := &RPCClient{}
	var completed atomic.Int32
	r.pending.Store(int64(1), &pendingCall{cb: func([]byte, error) { panic("fallback") }})
	r.pending.Store(int64(2), &pendingCall{cb: func([]byte, error) { completed.Add(1) }})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.StopWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	if completed.Load() != 1 {
		t.Fatal("pending callback was lost after panic")
	}
}

func TestRPCBudgetStopWaitsForTerminalClaimedBeforeShutdown(t *testing.T) {
	r := &RPCClient{}
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }); <-finished; r.Stop() })
	r.pending.Store(int64(1), &pendingCall{cb: func([]byte, error) { close(entered); <-release }})
	go func() { defer close(finished); r.finishPending(1, nil, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("fallback did not enter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := r.StopWithContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("already claimed callback disappeared from drain: %v", err)
	}
	once.Do(func() { close(release) })
	r.Stop()
}

func TestRPCStopInsideCallbackAfterStopRequestedReturns(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	entered, release, inner := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	r.pending.Store(int64(1), &pendingCall{cb: func([]byte, error) {
		close(entered)
		<-release
		r.Stop()
		close(inner)
	}})
	r.finishPending(1, nil, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("callback never entered; not a counterexample")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.StopWithContext(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a canceled budget = %v", err)
	}
	once.Do(func() { close(release) })
	select {
	case <-inner:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop inside a callback waited for the drain that is waiting for this callback")
	}
	budget, cancelBudget := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelBudget()
	if err := r.StopWithContext(budget); err != nil {
		t.Fatalf("stop after the callback exited = %v", err)
	}
}

func TestRPCStopWaitsForTerminalClaimSkippedByDrainRange(t *testing.T) {
	r := NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
	drainEntered, drainRelease := make(chan struct{}), make(chan struct{})
	claimEntered, claimRelease := make(chan struct{}), make(chan struct{})
	var drainOnce, claimOnce sync.Once
	releaseDrain := func() { drainOnce.Do(func() { close(drainRelease) }) }
	releaseClaim := func() { claimOnce.Do(func() { close(claimRelease) }) }
	t.Cleanup(func() { releaseDrain(); releaseClaim() })
	r.testBeforeDrain = func() { close(drainEntered); <-drainRelease }
	r.testAfterClaim = func() { close(claimEntered); <-claimRelease }

	var callbackRan atomic.Bool
	r.pending.Store(int64(1), &pendingCall{cb: func([]byte, error) { callbackRan.Store(true) }})

	type stopResult struct {
		err         error
		callbackRan bool
	}
	stopped := make(chan stopResult, 1)
	go func() {
		err := r.StopWithContext(context.Background())
		stopped <- stopResult{err: err, callbackRan: callbackRan.Load()}
	}()
	select {
	case <-drainEntered: // admission closed, the stop task has not ranged pending yet
	case <-time.After(time.Second):
		t.Fatal("stop task never started")
	}
	go r.finishPending(1, nil, nil) // the reply arrives while the stop is in progress
	select {
	case <-claimEntered: // the reply removed the entry and holds callbackMu before Add
	case <-time.After(time.Second):
		t.Fatal("reply never claimed the pending call")
	}
	releaseDrain()

	var got stopResult
	returnedWhileClaimHeld := false
	select {
	case got = <-stopped:
		returnedWhileClaimHeld = true
	case <-time.After(200 * time.Millisecond):
	}
	releaseClaim()
	if !returnedWhileClaimHeld {
		select {
		case got = <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("stop did not finish after the claimed callback was released")
		}
	}
	t.Logf("returned_while_claim_held=%v stop_err=%v callback_ran_before_return=%v", returnedWhileClaimHeld, got.err, got.callbackRan)
	if got.err != nil || !got.callbackRan {
		t.Fatal("StopWithContext reported a finished drain before a claimed terminal callback ran")
	}
}
