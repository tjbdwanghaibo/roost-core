// RR-20261004-07（复审 S2 / S1）：RPCClient 停止的两条边界。
//
// S2：停止已经发起后，callback 里的 Stop() 修前因 CAS 立即返回；NC-09 之后它等待
// 唯一停止任务，而停止任务又在等这个 callback 退出，永久互等。
//
// S1：finishPending 在 callbackMu 内先 LoadAndDelete、再 callbacks.Add。停止任务的
// pending.Range 会跳过刚被 reply / timeout 删除、还没 Add 的键，随后 Wait 在计数 0 时
// 立即返回，StopWithContext 报告 nil，而那次终态 callback 之后才执行。
package driver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

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
