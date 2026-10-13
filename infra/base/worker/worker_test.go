package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type workerTestTask struct {
	id       int
	released *atomic.Int64
}

func (t workerTestTask) OnRelease() {
	t.released.Add(1)
}

func TestWorker_ProcessesTasksAndStops(t *testing.T) {
	var released atomic.Int64
	handled := make(chan int, 2)
	w := NewWorker[workerTestTask]("test", 0, 16, func(task workerTestTask) {
		handled <- task.id
	})

	var wg sync.WaitGroup
	wg.Add(1)
	w.Run(&wg)

	w.Cast(workerTestTask{id: 1, released: &released})
	w.Cast(workerTestTask{id: 2, released: &released})

	for i := 0; i < 2; i++ {
		select {
		case <-handled:
		case <-time.After(time.Second):
			t.Fatal("worker did not process task")
		}
	}

	w.Close()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}

	if got := released.Load(); got != 2 {
		t.Fatalf("released tasks: got %d, want 2", got)
	}
}

func TestWorker_AcceptedTasksSurviveConcurrentClose(t *testing.T) {
	// Regression: TryCast checked closed and pushed as two separate steps, so
	// a push racing Close could land after the final drain — the task was
	// accepted (TryCast returned true) but never handled and never released.
	// Every accepted task must be handled exactly once.
	for round := 0; round < 50; round++ {
		var handled atomic.Int64
		var released atomic.Int64
		w := NewWorker[workerTestTask]("test", 0, 1024, func(workerTestTask) {
			handled.Add(1)
		})
		var wg sync.WaitGroup
		wg.Add(1)
		w.Run(&wg)

		var accepted atomic.Int64
		var casters sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 4; g++ {
			casters.Add(1)
			go func() {
				defer casters.Done()
				<-start
				for i := 0; i < 64; i++ {
					if w.TryCast(workerTestTask{id: i, released: &released}) {
						accepted.Add(1)
					}
				}
			}()
		}
		close(start)
		w.Close()
		casters.Wait()
		wg.Wait()

		if handled.Load() != accepted.Load() {
			t.Fatalf("round %d: accepted %d tasks but handled %d", round, accepted.Load(), handled.Load())
		}
		if released.Load() != accepted.Load() {
			t.Fatalf("round %d: accepted %d tasks but released %d", round, accepted.Load(), released.Load())
		}
	}
}

func TestWorker_CloseWakesIdleWorker(t *testing.T) {
	w := NewWorker[workerTestTask]("test", 0, 16, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	w.Run(&wg)

	w.Close()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle worker was not woken by close")
	}
}

// 启动前 / 停止后 TryDispatch 报 ErrWorkerClosed，队列满报 ErrWorkerQueueFull 而不是阻塞。
func TestTryDispatchRefusesBeforeStartAfterStopAndWhenTheQueueIsFull(t *testing.T) {
	var released atomic.Int64
	block := make(chan struct{})
	entered := make(chan struct{}, 4)
	pool := NewPool[workerTestTask](PoolConfig{Name: "guards", WorkerNum: 1, QueueCap: 1}, func(workerTestTask) {
		select {
		case entered <- struct{}{}:
		default: // 排空阶段的后续任务不能卡在这里，否则 Stop 永远等不到
		}
		<-block
	})
	task := workerTestTask{id: 1, released: &released}
	if err := pool.TryDispatch(1, task); !errors.Is(err, ErrWorkerClosed) {
		t.Fatalf("TryDispatch before Start = %v", err)
	}
	pool.Start()
	if err := pool.TryDispatch(1, task); err != nil {
		t.Fatalf("first TryDispatch = %v", err)
	}
	<-entered
	// 队列容量会被向上取整：一直投到被拒为止，被拒的理由必须是队列满而不是阻塞或关闭。
	queued := 0
	var full error
	for i := 0; i < 4096 && full == nil; i++ {
		if err := pool.TryDispatch(1, task); err != nil {
			full = err
		} else {
			queued++
		}
	}
	if !errors.Is(full, ErrWorkerQueueFull) || queued == 0 {
		t.Fatalf("TryDispatch on a full queue = %v after %d queued, want ErrWorkerQueueFull", full, queued)
	}
	close(block)
	pool.Stop()
	if err := pool.TryDispatch(1, task); !errors.Is(err, ErrWorkerClosed) {
		t.Fatalf("TryDispatch after Stop = %v", err)
	}
}

type blockingPoolTask struct {
	released atomic.Bool
}

func (t *blockingPoolTask) OnRelease() {
	t.released.Store(true)
}

func TestPoolStopWithContextReturnsWhenHandlerIsBlocked(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	pool := NewPool[*blockingPoolTask](PoolConfig{Name: "shutdown-test", WorkerNum: 1, QueueCap: 1}, func(*blockingPoolTask) {
		close(started)
		<-unblock
	})
	pool.Start()
	task := &blockingPoolTask{}
	if err := pool.TryDispatch(1, task); err != nil {
		t.Fatalf("TryDispatch: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := pool.StopWithContext(ctx)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopWithContext err = %v, want context deadline", err)
	}
	close(unblock)
}
