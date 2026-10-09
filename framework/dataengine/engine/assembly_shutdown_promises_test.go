package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// slowClaimOutboxStore parks the outbox worker inside its first Claim until
// the test releases it, so a bounded shutdown context runs out while the
// worker is still active.
type slowClaimOutboxStore struct {
	*projectorOutboxFake
	entered chan struct{}
	release chan struct{}
}

func (s *slowClaimOutboxStore) Claim(ctx context.Context, _ string, _ time.Time, _ int, _ time.Duration) ([]OutboxItem, error) {
	close(s.entered)
	<-s.release
	return nil, ctx.Err()
}

// U-0159 · C8 · RR-20260909-03：Shutdown 的 context 先到期、worker 还没退出时，Assembly 此前照样
// 把 runtime 置 nil；再调一次 Shutdown 看到 nil 直接答成功，而 outbox worker 仍在跑。停机没完成
// 就不能忘掉 runtime：重试必须继续等同一批组件，等到了才算停下、才释放引用。
func TestAssemblyKeepsTheRuntimeUntilShutdownCompletes(t *testing.T) {
	store := &slowClaimOutboxStore{projectorOutboxFake: newProjectorOutboxFake(), entered: make(chan struct{}), release: make(chan struct{})}
	worker, err := NewOutboxWorker(store, failingOutboxPublisher{}, OutboxWorkerOptions{Owner: "review"})
	if err != nil {
		t.Fatal(err)
	}
	worker.Start(context.Background())
	<-store.entered
	released := false
	defer func() {
		if !released {
			close(store.release)
		}
		if err := worker.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	runtime := &Runtime{Outbox: worker}
	assembly := &Assembly{runtime: runtime}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := assembly.Shutdown(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Shutdown with a canceled context = %v, want context.Canceled", err)
	}
	select {
	case <-worker.done:
		t.Fatal("the worker exited before the test released it; the scenario did not hold")
	default:
	}
	if assembly.Runtime() != runtime {
		t.Fatalf("Runtime() = %v after an incomplete shutdown, want the still-running runtime", assembly.Runtime())
	}

	retryCtx, retryCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer retryCancel()
	if err := assembly.Shutdown(retryCtx); err == nil {
		t.Fatalf("second Shutdown answered success while the outbox worker is still active; Runtime()=%v", assembly.Runtime())
	}
	if assembly.Runtime() != runtime {
		t.Fatal("lost runtime ownership after a second incomplete shutdown")
	}

	// Once the worker can exit, a retry completes and only then forgets the runtime.
	close(store.release)
	released = true
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after the worker was released = %v, want nil", err)
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("Shutdown returned nil while the worker had not exited")
	}
	if assembly.Runtime() != nil {
		t.Fatalf("Runtime() = %v after a completed shutdown, want nil", assembly.Runtime())
	}
	if err := assembly.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after completion = %v, want nil (idempotent)", err)
	}
}
