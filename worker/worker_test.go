package worker

import (
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
