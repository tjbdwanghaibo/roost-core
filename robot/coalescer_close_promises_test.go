package robot_test

import (
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
)

// RR-20261005-NC-265（N12 观察 O10）：Coalescer.Close 的注释承诺“flushes any pending batch”，返回时最后
// 一批已经交给 flush 并处理完。旧实现在 worker 运行时只 close(done) 就返回，worker 异步 drain：调用方随后
// 关闭会话，最后一批确认发在已关闭的会话上（或根本没发）。
func TestCoalescerCloseReturnsAfterTheFinalFlush(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	flushed := map[string]int64{}
	c := robot.NewCoalescer[string](time.Hour, func(batch map[string]int64) {
		once.Do(func() { close(entered) })
		<-release
		mu.Lock()
		for k, v := range batch {
			flushed[k] = v
		}
		mu.Unlock()
	})
	c.Add("topic:1", 7) // 起 worker；间隔 1h，只有 Close 会触发 flush

	closed := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closed)
	}()
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while the final flush had not finished (the batch is still being sent)")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Close never flushed the pending batch")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the final flush finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if flushed["topic:1"] != 7 {
		t.Fatalf("flushed = %v, want topic:1=7", flushed)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}
