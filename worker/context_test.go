package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fctx "github.com/tjbdwanghaibo/cube-core/ctx"
)

type contextTestTask struct {
	id       int
	released *atomic.Int64
}

func (t contextTestTask) OnRelease() { t.released.Add(1) }

type observedWorkerContext struct {
	id       int
	source   string
	handler  string
	playerID int64
	leaked   bool
	config   any
}

func TestWorkerCreatesFreshContextForEveryTask(t *testing.T) {
	previousConfig := fctx.RuntimeConfig()
	runtimeConfig := &struct{ name string }{name: "worker-runtime"}
	fctx.SetRuntimeConfig(runtimeConfig)
	t.Cleanup(func() { fctx.SetRuntimeConfig(previousConfig) })

	parent, releaseParent := fctx.NewContext(fctx.WithMeta(fctx.RequestMeta{
		Source:   "request",
		Handler:  "parent",
		PlayerID: 42,
	}))
	parent.Set("business", "must-not-propagate")
	defer releaseParent()

	var released atomic.Int64
	observed := make(chan observedWorkerContext, 2)
	worker := NewWorker[contextTestTask]("context-worker", 0, 4, func(task contextTestTask) {
		current := fctx.CurrentContext()
		_, leaked := current.Get("business")
		observed <- observedWorkerContext{
			id:       task.id,
			source:   current.Meta.Source,
			handler:  current.Meta.Handler,
			playerID: current.Meta.PlayerID,
			leaked:   leaked,
			config:   current.Config,
		}
		// This value must be cleared before the same worker handles the next task.
		current.Set("business", "task-local")
	})

	var wg sync.WaitGroup
	wg.Add(1)
	worker.Run(&wg)
	worker.Cast(contextTestTask{id: 1, released: &released})
	worker.Cast(contextTestTask{id: 2, released: &released})
	worker.Close()
	wg.Wait()

	for wantID := 1; wantID <= 2; wantID++ {
		select {
		case got := <-observed:
			if got.id != wantID {
				t.Fatalf("task order = %d, want %d", got.id, wantID)
			}
			if got.source != "worker" || got.handler != "context-worker" {
				t.Fatalf("worker metadata = source %q handler %q", got.source, got.handler)
			}
			if got.playerID != 0 || got.leaked {
				t.Fatalf("parent or previous task context leaked: %+v", got)
			}
			if got.config != runtimeConfig {
				t.Fatalf("runtime config = %v, want current runtime config", got.config)
			}
		case <-time.After(time.Second):
			t.Fatal("worker did not handle task")
		}
	}
	if got := released.Load(); got != 2 {
		t.Fatalf("released = %d, want 2", got)
	}
}

func TestPoolGoCreatesDetachedContext(t *testing.T) {
	parent, releaseParent := fctx.NewContext(fctx.WithMeta(fctx.RequestMeta{PlayerID: 99}))
	parent.Set("business", "must-not-propagate")
	defer releaseParent()

	var released atomic.Int64
	done := make(chan observedWorkerContext, 1)
	pool := NewPool[contextTestTask](PoolConfig{Name: "go-worker", WorkerNum: 1, QueueCap: 1}, nil)
	pool.Start()
	pool.Go(contextTestTask{id: 1, released: &released}, func(task contextTestTask) {
		current := fctx.CurrentContext()
		_, leaked := current.Get("business")
		done <- observedWorkerContext{
			id:       task.id,
			source:   current.Meta.Source,
			handler:  current.Meta.Handler,
			playerID: current.Meta.PlayerID,
			leaked:   leaked,
		}
	})
	pool.Stop()

	select {
	case got := <-done:
		if got.source != "worker" || got.handler != "go-worker" || got.playerID != 0 || got.leaked {
			t.Fatalf("detached worker context = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Pool.Go did not complete")
	}
	if got := released.Load(); got != 1 {
		t.Fatalf("released = %d, want 1", got)
	}
}

func TestPoolGoRejectsUntrackedLifetime(t *testing.T) {
	var released atomic.Int64
	executed := make(chan struct{}, 1)
	pool := NewPool[contextTestTask](PoolConfig{Name: "lifetime-worker"}, nil)
	task := contextTestTask{id: 1, released: &released}

	if err := pool.TryGo(task, func(contextTestTask) { executed <- struct{}{} }); err != ErrWorkerClosed {
		t.Fatalf("TryGo before Start = %v, want ErrWorkerClosed", err)
	}
	if got := released.Load(); got != 0 {
		t.Fatalf("TryGo transferred ownership on rejection: released=%d", got)
	}
	pool.Go(task, func(contextTestTask) { executed <- struct{}{} })
	if got := released.Load(); got != 1 {
		t.Fatalf("Go did not release rejected task: released=%d", got)
	}
	select {
	case <-executed:
		t.Fatal("task executed outside pool lifetime")
	case <-time.After(20 * time.Millisecond):
	}

	pool.Start()
	pool.Stop()
	pool.Go(task, func(contextTestTask) { executed <- struct{}{} })
	if got := released.Load(); got != 2 {
		t.Fatalf("Go after Stop did not release task: released=%d", got)
	}
}
