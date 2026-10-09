package gateway

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
)

// 多次取消 Stop 只取消调用方的等待；不能为同一批仍未完成的请求累积等待协程。
func TestGateStopRetriesShareOneDrainWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := &Gate{}
		gate.forwards.Add(1)
		defer gate.forwards.Done()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for range 20 {
			if err := gate.Stop(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Stop = %v, want canceled while dispatch remains active", err)
			}
		}
		// 等全部新增协程真正阻塞，不依赖 sleep 或任意调度时机。
		synctest.Wait()
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		if n == len(stack) {
			t.Fatal("goroutine stack truncated")
		}
		waiters := 0
		for _, block := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(block, "gateway.waitRuntime.func") {
				waiters++
			}
		}
		if waiters != 1 {
			t.Fatalf("same Stop phase created %d waiters; want one", waiters)
		}
	})
}
