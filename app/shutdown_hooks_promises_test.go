package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/lifecycle"
)

// RR-20261005-NC-231：停机阶段的生命周期 hook（service.stopping / service.stopped）也受
// shutdown.total_timeout 约束。hook 不配合 ctx 时 App 不能杀掉它，但 run 必须在预算内如实返回
// “不完整”；service.stopping 的 hook 还没返回时它可能仍在用 Service 与 Mod，所以和 Service.Shutdown
// 不完整一样：不停 Service、不停 Mod、不释放单实例锁。
//
// 旧行为：run 同步调用 EmitAll。一个忽略 ctx 的 hook 让 run 永远不返回——Service.Shutdown 没有被
// 调用、Mod 不停、续期一直在跑、锁一直持有，进程挂到部署侧 SIGKILL（k8s 宽限期 = total + 5s）。

const shutdownHookBudget = 300 * time.Millisecond

// registerBlockingHook 在 phase 上登记一个忽略 ctx、卡到 release 关闭的 hook；entered 在 hook 开始时关闭。
func registerBlockingHook(t *testing.T, r *Registry, phase lifecycle.Phase, release <-chan struct{}, entered chan<- struct{}) {
	t.Helper()
	hooks := MustLookup[*lifecycle.Registry](r, ModLifecycle)
	var once sync.Once
	if err := hooks.Register(lifecycle.Hook{
		Name: "blocking-" + string(phase), Phase: phase,
		Handler: func(context.Context, lifecycle.Event) error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownLifecycleHooksStayWithinTheShutdownBudget(t *testing.T) {
	for _, tc := range []struct {
		phase lifecycle.Phase
		// stopping hook 卡住时 Service 与 Mod 都不能动；stopped hook 在 Mod 停完之后才运行。
		wantShutdowns, wantModStops int32
	}{
		{phase: lifecycle.PhaseServiceStopping, wantShutdowns: 0, wantModStops: 0},
		{phase: lifecycle.PhaseServiceStopped, wantShutdowns: 1, wantModStops: 1},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			shared := &singletonProbeMod{name: "probe_shared"}
			h := newSingletonHarness(t, []Mod{shared}, nil)
			h.app.cfg.Set("shutdown.total_timeout", shutdownHookBudget.String())
			release := make(chan struct{})
			entered := make(chan struct{})
			h.svc.onInit = func(r *Registry) error {
				registerBlockingHook(t, r, tc.phase, release, entered)
				return nil
			}
			var shutdownCalls atomic.Int32
			h.svc.onShutdown = func(context.Context) error {
				shutdownCalls.Add(1)
				return nil
			}
			result := h.start()
			// 在 harness 的收尾之后登记、因而先执行：先放行 hook，run 才能在收尾里退出。
			t.Cleanup(func() { close(release) })
			h.awaitServed(t, result)
			started := time.Now()
			h.signals <- os.Interrupt
			err, returned := awaitRunWithin(t, result, shutdownHookBudget+testWaitLimit)
			select {
			case <-entered:
			default:
				t.Fatalf("the %s hook never ran", tc.phase)
			}
			if !returned {
				t.Fatalf("run did not return %s after a %s hook that ignores its ctx (shutdown.total_timeout %s)",
					shutdownHookBudget+testWaitLimit, tc.phase, shutdownHookBudget)
			}
			if elapsed := time.Since(started); elapsed > shutdownHookBudget+2*time.Second {
				t.Fatalf("run took %s, budget %s", elapsed, shutdownHookBudget)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("run error = %v, want the shutdown budget's DeadlineExceeded", err)
			}
			if calls := shutdownCalls.Load(); calls != tc.wantShutdowns || shared.stops.Load() != tc.wantModStops {
				t.Fatalf("Service.Shutdown calls=%d mod stops=%d, want %d / %d", calls, shared.stops.Load(), tc.wantShutdowns, tc.wantModStops)
			}
			if tc.phase == lifecycle.PhaseServiceStopping && len(h.store.releaseCalls()) != 0 {
				t.Fatalf("singleton released (%+v) while a service.stopping hook was still running", h.store.releaseCalls())
			}
		})
	}
}
