package goroutine

import (
	"strings"
	"testing"
)

// U-0147 · C2 · nightly gap map core `goroutine` 2/2：未启动的池拒绝提交；已关闭的工人拒绝任务。
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
