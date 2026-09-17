package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// U-0219 · C2 · RR-20260916-06：Start 途中收到 Stop，最后一个（含唯一一个）管理器成功启动后必须被
// 恰好一方清理。旧实现只在"下一轮循环顶部"检查 stopping：Stop 取走当时为空的 started 后返回，
// 随后 Start 把刚成功的管理器追加进 started 并整体返回 nil——一次正常的关闭流程结束后它还在运行。
type gatedManager struct {
	name             string
	entered, release chan struct{}
	starts, stops    atomic.Int32
}

func (m *gatedManager) Name() string { return m.name }
func (m *gatedManager) Start(*app.Registry) error {
	m.starts.Add(1)
	if m.entered != nil {
		close(m.entered)
		<-m.release
	}
	return nil
}
func (m *gatedManager) Stop() { m.stops.Add(1) }

func waitFor(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout")
	}
}

func TestStopDuringTheLastStartStillStopsWhatStarted(t *testing.T) {
	for _, another := range []bool{false, true} {
		name := "last"
		if another {
			name = "has_next"
		}
		t.Run(name, func(t *testing.T) {
			slow := &gatedManager{name: "slow", entered: make(chan struct{}), release: make(chan struct{})}
			managers := []app.IManager{slow}
			if another {
				managers = append(managers, &gatedManager{name: "next"})
			}
			e := NewEngine(managers...)
			if err := e.Provide(app.NewRegistry(viper.New())); err != nil {
				t.Fatal(err)
			}
			startDone := make(chan struct{})
			var startErr error
			go func() { startErr = e.Start(); close(startDone) }()
			waitFor(t, slow.entered)

			stopDone := make(chan struct{})
			go func() { _ = e.Stop(context.Background()); close(stopDone) }()
			deadline := time.Now().Add(3 * time.Second)
			for !e.stopRequested() {
				if time.Now().After(deadline) {
					t.Fatal("Stop never recorded the request")
				}
				time.Sleep(time.Millisecond)
			}
			// Stop has drained started (empty so far) and returned; now let the
			// in-flight Start finish. Whoever sees it complete owns its cleanup.
			close(slow.release)
			waitFor(t, startDone)
			waitFor(t, stopDone)
			if startErr == nil {
				t.Fatalf("Start reported success although shutdown was requested before it finished")
			}
			if slow.stops.Load() != 1 {
				t.Fatalf("the manager that finished starting after Stop was stopped %d times, want exactly 1 (starts=%d)", slow.stops.Load(), slow.starts.Load())
			}
		})
	}
}
