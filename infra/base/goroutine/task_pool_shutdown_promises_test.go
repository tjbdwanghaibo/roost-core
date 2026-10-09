package goroutine

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// RR-20261005-NC-183：Submit 与 Shutdown 并发时，Submit 要么受理、要么返回
// 错误，不会 panic。
//
// 旧行为：taskWorker.submitTask 先读 closed、再向 taskChan 发送，两步之间
// Shutdown 关闭 channel，发送方 panic “send on closed channel”，把调用方
// goroutine（通常是业务请求）带走。窗口很窄，所以按轮次重复。
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
