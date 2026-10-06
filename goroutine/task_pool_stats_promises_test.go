package goroutine

import (
	"runtime"
	"testing"
	"time"
)

// RR-20261006-20（N13 观察 O9，维护者决定 NONCORE-46 选 A）：GetStats 读到的统计是一致的——
// 任何一次读到的 completed + failed 都不超过 total（已结束的任务一定已被计入提交数）。
//
// 旧行为：
//   - Submit 先把任务送进 worker channel、再 totalTasks.Add(1)；两步之间 worker 已执行完并
//     completedTasks.Add(1)，此时读统计得到 completed=1、total=0；
//   - GetStats 先读 total、再读 completed；两次读之间有任务提交并完成，同样读到 completed > total。
//
// 两个窗口都用测试缝（submitEnqueuedHook / statsFirstLoadedHook）停住，不靠 sleep 碰概率。

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
