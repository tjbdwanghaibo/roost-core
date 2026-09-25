package nest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestAsyncCompletionWaitsForEntityRelease(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9980, 10)
	manager := entity.NewEntityManager()
	if err := manager.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	getter := newMockGetter()
	getter.Add(e)
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true)), NestOptionWithPipelinedAsyncCompletion(1, 4))
	releasing, allowRelease := make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	allow := func() { allowOnce.Do(func() { close(allowRelease) }) }
	defer allow()
	defer manager.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) { close(releasing); <-allowRelease })()
	completed := make(chan bool, 1)
	name := NewHandlerName("completion_unlock")
	engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		AfterCommit(func() {
			free := e.GetMutex().TryLock()
			if free {
				e.GetMutex().Unlock()
			}
			completed <- free
		})
		return nil, MarkPersist(e.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined})
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { allow(); _ = engine.Shutdown(context.Background()) }()
	replied := make(chan error, 1)
	go func() { _, err := engine.Request(context.Background(), name, id, nil); replied <- err }()
	select {
	case <-releasing:
	case <-time.After(time.Second):
		t.Fatal("release hook not reached")
	}
	select {
	case free := <-completed:
		t.Errorf("AfterCommit ran before release completed (lock free=%v)", free)
	case <-time.After(20 * time.Millisecond):
	}
	allow()
	select {
	case err := <-replied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing completion reply")
	}
	select {
	case free := <-completed:
		if !free {
			t.Error("AfterCommit still held entity lock")
		}
	default:
	}
}

func TestTickerConcurrentStartStopAlwaysClosesStartedRun(t *testing.T) {
	for i := range 20000 {
		ticker := NewTicker(time.Hour)
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-gate; ticker.Start() })
		wg.Go(func() { <-gate; ticker.Stop() })
		close(gate)
		wg.Wait()
		if !ticker.started {
			continue
		}
		select {
		case <-ticker.done:
		default:
			// 修前可出现 Stop 认为尚未启动，但随后 Start 仍启动；清理泄漏以免污染其它测试。
			select {
			case <-ticker.stopChan:
			default:
				close(ticker.stopChan)
			}
			<-ticker.done
			t.Fatalf("iteration %d: stopped ticker retained a live run", i)
		}
	}
}
