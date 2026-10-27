package alliance

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// 缺省模式就跑：用 bus.NewRPCContext 以不带期限的基础 context 模拟 dispatchRpc 交给 handler 的上下文，
// 验证接收端预算本身——期限到了按 ctx 等待的回调结束、迟到的成功不回复、期限内的成功照常回复。
func TestAllianceReceiverBudgetRejectsLateSuccess(t *testing.T) {
	if _, err := receiverBudget(0, func(context.Context, *bus.RpcContext) (any, error) { return true, nil }); !errors.Is(err, errReceiverBudget) {
		t.Fatalf("zero budget accepted: %v", err)
	}
	if _, err := receiverBudget(time.Second, nil); !errors.Is(err, errReceiverBudget) {
		t.Fatalf("nil handler accepted: %v", err)
	}
	injected := errors.New("injected receive failure")
	for _, tc := range []struct {
		name    string
		handle  func(context.Context, *bus.RpcContext) (any, error)
		wantOut any
		wantErr error
	}{
		{"blocked until budget", func(ctx context.Context, _ *bus.RpcContext) (any, error) { <-ctx.Done(); return nil, ctx.Err() }, nil, context.DeadlineExceeded},
		{"late success", func(ctx context.Context, _ *bus.RpcContext) (any, error) { <-ctx.Done(); return true, nil }, nil, context.DeadlineExceeded},
		{"handler error", func(context.Context, *bus.RpcContext) (any, error) { return nil, injected }, nil, injected},
		{"in time", func(context.Context, *bus.RpcContext) (any, error) { return true, nil }, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handle, err := receiverBudget(20*time.Millisecond, func(ctx context.Context, c *bus.RpcContext) (any, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("handler context has no receiver deadline")
				}
				return tc.handle(ctx, c)
			})
			check(t, err)
			out, err := handle(bus.NewRPCContext(context.Background(), "receive", nil, nil))
			if !errors.Is(err, tc.wantErr) || out != tc.wantOut {
				t.Fatalf("out=%v err=%v, want out=%v err=%v", out, err, tc.wantOut, tc.wantErr)
			}
		})
	}
}

// ROOST_SLG_REAL=1，隔离 NATS：真实 Bus 普通 RPC 上核对框架契约——调用方的 deadline 不传到接收端，
// 调用方超时返回时接收端回调仍在运行；接收端用 receiverBudget 自己限时后，迟到的成功不回复成功。
func TestSLGAllianceRPCReceiverBudget(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("run ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh with isolated Mongo, Redis and NATS")
	}
	prefix := namespace()
	if !safeNamespace(prefix) {
		t.Fatal("unsafe namespace")
	}
	home, homeNATS := newBus(t, prefix, "slghome", 1000)
	caller, callerNATS := newBus(t, prefix, "slgmap", 2000)

	// 不限时的接收端：记录收到的 context 有没有期限，按自己的 context 等，最后由测试放行。
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var callerDeadlineSeen, unboundedReturned atomic.Bool
	check(t, home.HandleRpc("unbounded", func(c *bus.RpcContext) (any, error) {
		if _, ok := c.Context().Deadline(); ok {
			callerDeadlineSeen.Store(true)
		}
		entered <- struct{}{}
		defer unboundedReturned.Store(true)
		select {
		case <-release:
		case <-c.Context().Done():
		}
		return true, nil
	}))
	var budgetSeen atomic.Bool
	bounded, err := receiverBudget(200*time.Millisecond, func(ctx context.Context, c *bus.RpcContext) (any, error) {
		if _, ok := ctx.Deadline(); ok {
			budgetSeen.Store(true)
		}
		if c.MsgName == "bounded-late" {
			<-ctx.Done()
		}
		return true, nil // late 路径：预算过后才返回“成功”。
	})
	check(t, err)
	check(t, home.HandleRpc("bounded-late", bounded))
	check(t, home.HandleRpc("bounded-ok", bounded))
	check(t, home.Start())
	check(t, caller.Start())
	flush, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	check(t, homeNATS.FlushContext(flush))
	check(t, callerNATS.FlushContext(flush))
	cancelFlush()

	// 1. 调用方 300ms 超时返回；接收端回调此时仍在运行，且它的 context 没有调用方的期限。
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	var ok bool
	err = caller.CallTo(short, "slghome", 1000, "unbounded", true, &ok)
	cancel()
	if err == nil {
		t.Fatal("caller deadline did not end the call")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("unbounded handler never ran")
	}
	time.Sleep(100 * time.Millisecond) // 给“误传了期限”的实现留出结束回调的时间，再观察它仍在运行。
	if unboundedReturned.Load() {
		t.Fatal("receiver stopped with the caller: plain RPC now propagates the caller deadline, update receiverBudget and SLG-INTEGRATION §7.7")
	}
	if callerDeadlineSeen.Load() {
		t.Fatal("plain RPC handler context carries the caller deadline; the documented contract changed")
	}
	close(release)
	released = true
	waitUntil := time.Now().Add(5 * time.Second)
	for !unboundedReturned.Load() && time.Now().Before(waitUntil) {
		time.Sleep(10 * time.Millisecond)
	}
	if !unboundedReturned.Load() {
		t.Fatal("released handler did not return")
	}

	// 2. 接收端自己限时：调用方给足 5s，接收端 200ms 预算到期后迟到的成功不回复成功。
	long, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	ok = false
	err = caller.CallTo(long, "slghome", 1000, "bounded-late", true, &ok)
	if err == nil || ok {
		t.Fatalf("late success acknowledged: ok=%v err=%v", ok, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("receiver budget did not bound the handler: %v", elapsed)
	}
	if !budgetSeen.Load() {
		t.Fatal("bounded handler saw no receiver deadline")
	}
	// 3. 期限内完成照常回复。
	ok = false
	check(t, caller.CallTo(long, "slghome", 1000, "bounded-ok", true, &ok))
	if !ok {
		t.Fatal("in-time receive not acknowledged")
	}
	t.Log("plain Bus RPC: caller deadline not propagated (handler outlived a 300ms caller); receiver budget 200ms refused late success, in-time success acknowledged")
}
