package worker

import (
	"errors"
	"sync/atomic"
	"testing"
)

// U-0147 · C2 · nightly gap map core `worker` 3/3：启动前 / 停止后 TryDispatch 报
// ErrWorkerClosed，队列满报 ErrWorkerQueueFull 而不是阻塞。`pickWorker` 里
// `idx >= len(workers)` 在 workerNum == len(workers) 下不可达，保留。
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
