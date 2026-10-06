package goroutine

import (
	"testing"
	"time"
)

// RR-20261005-NC-269（N13 观察 O9）：TaskPool 是一次性的，Shutdown 之后 Start 不能把它“救活”成一个
// 报告 running、却永远拒绝任务的池；先 Shutdown（从未 Start）再 Start 之后，Shutdown 必须仍能停下它。
//
// 旧行为：
//   - Shutdown 后 Start：running CAS 成功、起读已关闭 channel 的 worker，IsRunning 为 true，之后 Submit
//     全部报 worker closed；
//   - 未 Start 就 Shutdown：closeOnce 执行但没关任何 worker；随后 Start 起的 worker 再也关不掉，之后每次
//     Shutdown 都等满超时返回错误。
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
