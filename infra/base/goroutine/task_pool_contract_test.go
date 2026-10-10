package goroutine

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubmitRefusesAPoolThatIsNotRunningAndAClosedWorker(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 1})
	task := &TaskFunc{ID: 1, Func: func() error { return nil }}
	if err := pool.Submit(task); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("Submit before Start = %v", err)
	}
	pool.Start()
	defer pool.Shutdown()
	if err := pool.Submit(task); err != nil {
		t.Fatalf("Submit after Start = %v", err)
	}
	worker := pool.workers[0]
	worker.closed.Store(true)
	err := worker.submitTask(task)
	worker.closed.Store(false)
	if err == nil || !strings.Contains(err.Error(), "is closed") {
		t.Fatalf("submitTask on a closed worker = %v", err)
	}
}

func TestTaskPoolStartAfterShutdownIsRefused(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 2, MaxTaskCount: 8, ShutdownTimeout: time.Second})
	pool.Start()
	if err := pool.Shutdown(); err != nil {
		t.Fatal(err)
	}
	pool.Start()
	if pool.IsRunning() {
		t.Fatal("IsRunning = true after Start on a shut-down pool, while every Submit is refused")
	}
	if err := pool.SubmitFunc(1, "late", func() error { return nil }); err == nil {
		t.Fatal("Submit on a shut-down pool accepted")
	}
}

func TestTaskPoolShutdownBeforeStartStillStopsALaterStart(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 2, MaxTaskCount: 8, ShutdownTimeout: 200 * time.Millisecond})
	if err := pool.Shutdown(); err != nil {
		t.Fatal(err)
	}
	pool.Start()
	if err := pool.Shutdown(); err != nil {
		t.Fatalf("Shutdown after Shutdown-then-Start = %v, want nil (the workers started after the first Shutdown must still stop)", err)
	}
	if pool.IsRunning() {
		t.Fatal("pool still running after Shutdown")
	}
}

func TestSubmitRacingShutdownNeverPanics(t *testing.T) {
	var (
		mu     sync.Mutex
		panics []string
	)
	for round := 0; round < 2000; round++ {
		pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 1, MaxTaskCount: 1 << 16})
		pool.Start()
		var group sync.WaitGroup
		var accepted atomic.Int64
		start := make(chan struct{})
		for range 4 {
			group.Go(func() {
				defer func() {
					if r := recover(); r != nil {
						mu.Lock()
						panics = append(panics, fmt.Sprint(r))
						mu.Unlock()
					}
				}()
				<-start
				for range 200 {
					if pool.SubmitFunc(1, "", func() error { return nil }) == nil {
						accepted.Add(1)
					}
				}
			})
		}
		close(start)
		// Shut down while the submitters are mid-loop, not before they start.
		for accepted.Load() == 0 {
			runtime.Gosched()
		}
		if err := pool.Shutdown(); err != nil {
			t.Fatalf("round %d: shutdown: %v", round, err)
		}
		group.Wait()
	}
	if len(panics) > 0 {
		t.Fatalf("Submit panicked %d times while Shutdown ran; first: %s", len(panics), panics[0])
	}
}

func setTaskPoolHook(t *testing.T, hook *func(), fn func()) {
	t.Helper()
	*hook = fn
	t.Cleanup(func() { *hook = nil })
}

// waitCompleted 等到池里结束的任务数达到 want（有界等待一个事件，不是用 sleep 制造交错）。
func waitCompleted(t *testing.T, pool *TaskPool, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for pool.completedTasks.Load()+pool.failedTasks.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("task did not finish within 5s (completed=%d failed=%d, want %d)",
				pool.completedTasks.Load(), pool.failedTasks.Load(), want)
		}
		runtime.Gosched()
	}
}

func assertStatsConsistent(t *testing.T, total, completed, failed int64) {
	t.Helper()
	if completed+failed > total {
		t.Fatalf("GetStats = total %d, completed %d, failed %d: more tasks finished than submitted", total, completed, failed)
	}
}

// 提交方停在“已进 channel”之后：任务在另一 goroutine 里执行完，读方此时读统计。
func TestTaskPoolStatsCountSubmitBeforeTheTaskCanFinish(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 1, MaxTaskCount: 4, ShutdownTimeout: time.Second})
	pool.Start()
	defer pool.Shutdown()

	paused := make(chan struct{})
	resume := make(chan struct{})
	setTaskPoolHook(t, &submitEnqueuedHook, func() {
		close(paused)
		<-resume
	})
	submitted := make(chan error, 1)
	go func() { submitted <- pool.SubmitFunc(1, "stats", func() error { return nil }) }()

	<-paused
	waitCompleted(t, pool, 1)
	total, completed, failed, _ := pool.GetStats()
	close(resume)
	if err := <-submitted; err != nil {
		t.Fatalf("Submit = %v", err)
	}
	assertStatsConsistent(t, total, completed, failed)

	total, completed, failed, _ = pool.GetStats()
	if total != 1 || completed != 1 || failed != 0 {
		t.Fatalf("after Submit returned: GetStats = total %d, completed %d, failed %d, want 1 / 1 / 0", total, completed, failed)
	}
}

// 读方停在“读完第一个计数”之后：期间另一 goroutine 提交一个任务并等它执行完。
func TestTaskPoolStatsReadFinishedBeforeSubmitted(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 1, MaxTaskCount: 4, ShutdownTimeout: time.Second})
	pool.Start()
	defer pool.Shutdown()

	setTaskPoolHook(t, &statsFirstLoadedHook, func() {
		done := make(chan error, 1)
		go func() { done <- pool.SubmitFunc(1, "stats", func() error { return nil }) }()
		if err := <-done; err != nil {
			t.Errorf("Submit = %v", err)
			return
		}
		waitCompleted(t, pool, 1)
	})
	total, completed, failed, _ := pool.GetStats()
	assertStatsConsistent(t, total, completed, failed)
}

// 被拒绝的提交不计入 total（提交前先计数时，拒绝要撤回）。
func TestTaskPoolRejectedSubmitIsNotCounted(t *testing.T) {
	pool := NewTaskPool(&TaskPoolConfig{WorkerCount: 1, MaxTaskCount: 1, ShutdownTimeout: time.Second})
	pool.Start()
	defer pool.Shutdown()

	started := make(chan struct{})
	release := make(chan struct{})
	if err := pool.SubmitFunc(1, "holds the worker", func() error { close(started); <-release; return nil }); err != nil {
		t.Fatalf("first Submit = %v", err)
	}
	<-started // worker 已取走第一个，队列（容量 1）空
	if err := pool.SubmitFunc(2, "fills the queue", func() error { return nil }); err != nil {
		t.Fatalf("second Submit = %v", err)
	}
	if err := pool.SubmitFunc(3, "rejected", func() error { return nil }); err == nil {
		t.Fatal("third Submit into a full queue was accepted")
	}
	total, _, _, _ := pool.GetStats()
	close(release)
	if total != 2 {
		t.Fatalf("total = %d after two accepted and one rejected Submit, want 2", total)
	}
}
