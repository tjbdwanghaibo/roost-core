package operation

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestLifetimeStopDrainsOnlyAdmittedCalls(t *testing.T) {
	var lifetime Lifetime
	for range 8 {
		if !lifetime.Begin() {
			t.Fatal("admission refused")
		}
	}
	drained := lifetime.Stop()
	if lifetime.Begin() {
		t.Fatal("accepted after stop")
	}
	select {
	case <-drained:
		t.Fatal("premature drain")
	default:
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(lifetime.End)
	}
	group.Wait()
	<-drained
	if lifetime.Stop() != drained {
		t.Fatal("stop changed lifetime")
	}
}

// A3：Wait 是“三步停机”第二步——关闭准入、在 ctx 内等排空、超时保留计数可重试、排空后任何 ctx 都返回 nil。
func TestLifetimeWaitIsBoundedRetryableAndReportsTheRealDrain(t *testing.T) {
	var lifetime Lifetime
	if !lifetime.Begin() {
		t.Fatal("admission refused before stop")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lifetime.Wait(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait with a call in flight = %v, want context.Canceled", err)
	}
	if !lifetime.Stopping() || lifetime.Begin() {
		t.Fatal("Wait did not close admission")
	}
	if err := lifetime.Wait(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry Wait with the call still in flight = %v, want context.Canceled", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- lifetime.Wait(context.Background()) }()
	lifetime.End()
	if err := <-waited; err != nil {
		t.Fatalf("Wait after the call ended = %v", err)
	}
	// 已排空：即使 ctx 已结束也如实返回 nil（不在两个就绪分支间随机）。
	for range 64 {
		if err := lifetime.Wait(expired); err != nil {
			t.Fatalf("Wait after the drain with an expired ctx = %v, want nil", err)
		}
	}
}

func TestLifetimeZeroValueWaitWithoutCallsReturnsNil(t *testing.T) {
	var lifetime Lifetime
	if lifetime.Stopping() {
		t.Fatal("a fresh lifetime reports stopping")
	}
	if err := lifetime.Wait(nil); err != nil { // nil ctx 按 Background 处理
		t.Fatalf("Wait = %v", err)
	}
}
