package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// RR-20261005-NC-193：启动失败时，Service 已经启动的部分必须先停下来，App 才停 Mod、释放单实例锁；
// 停不下来（超出收尾预算）就和正常停机一样保留 Mod 与锁，不在它们底下拆掉依赖。
//
// 旧行为：
//   - Service.Init 返回错误时 App 不调用 Shutdown，直接逆序停 Mod 并 Release。game-demo 的 Init 在
//     startActivity 之后失败（例如 phase 消费者订阅失败），World tick 与窗口循环、场景、spawner 等
//     还在跑，NATS / Nest / Redis 已在它们底下停掉，锁也已释放（N11 观察 O8）。
//   - Init 成功之后的启动失败（启动期间 RuntimeFailure、PhaseServiceStarted 失败）同步调用 Shutdown：
//     不配合 ctx 的 Shutdown 让 run 永远不返回；Shutdown 超出 5s 收尾预算返回错误后，仍照常停 Mod、
//     Release——与正常停机“Shutdown 不完整就保留依赖、不释放”的规则相反。

// startupCleanupWait 比收尾预算（5s）长，用来区分“按预算返回”与“永远不返回”。
const startupCleanupWait = 3 * startupCleanupTimeoutForTest

const startupCleanupTimeoutForTest = 5 * time.Second

func awaitRunWithin(t *testing.T, result <-chan error, limit time.Duration) (error, bool) {
	t.Helper()
	select {
	case err := <-result:
		return err, true
	case <-time.After(limit):
		return nil, false
	}
}

func TestServiceInitFailureStopsWhatInitStartedBeforeTheMods(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	var workerRunning atomic.Bool
	stopWorker := make(chan struct{})
	workerDone := make(chan struct{})
	h.svc.onInit = func(*Registry) error {
		// Init 已经启动了一个后台循环（game-demo 的 activity / scene / spawner），随后的步骤失败。
		workerRunning.Store(true)
		go func() {
			defer close(workerDone)
			<-stopWorker
			workerRunning.Store(false)
		}()
		return errors.New("phase consumer: subscribe failed")
	}
	var shutdowns atomic.Int32
	h.svc.onShutdown = func(context.Context) error {
		shutdowns.Add(1)
		close(stopWorker)
		<-workerDone
		return nil
	}
	var workerAtModStop, heldAtModStop atomic.Bool
	shared.onStop = func() {
		workerAtModStop.Store(workerRunning.Load())
		heldAtModStop.Store(len(h.store.value(testSingletonKey)) > 0)
	}
	err := awaitRunResult(t, h.start())
	if err == nil || !strings.Contains(err.Error(), "subscribe failed") {
		t.Fatalf("run error = %v, want the init failure", err)
	}
	if shutdowns.Load() != 1 {
		t.Fatalf("Service.Shutdown called %d times after a failed Init, want 1: what Init started keeps running", shutdowns.Load())
	}
	if shared.stops.Load() != 1 || workerAtModStop.Load() || !heldAtModStop.Load() {
		t.Fatalf("mod stops=%d workerRunningAtModStop=%v heldAtModStop=%v; want the mods stopped once, after the service's worker, while still holding the lock",
			shared.stops.Load(), workerAtModStop.Load(), heldAtModStop.Load())
	}
	if len(h.store.releaseCalls()) != 1 {
		t.Fatalf("releases = %+v, want one release after everything stopped", h.store.releaseCalls())
	}
}

func TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shutdown func(release <-chan struct{}) func(context.Context) error
	}{
		{
			// 配合 ctx：超出收尾预算后返回 ctx 错误。
			name: "cooperative_timeout",
			shutdown: func(<-chan struct{}) func(context.Context) error {
				return func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
			},
		},
		{
			// 不配合 ctx：一直卡住，run 也必须在预算内返回。
			name: "uncooperative",
			shutdown: func(release <-chan struct{}) func(context.Context) error {
				return func(context.Context) error { <-release; return nil }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared := &singletonProbeMod{name: "probe_shared"}
			h := newSingletonHarness(t, []Mod{shared}, nil)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			cause := errors.New("dataengine fatal right after init")
			h.svc.onInit = func(r *Registry) error {
				// Init 成功返回之后的启动检查点发现 RuntimeFailure，走启动失败收尾。
				MustLookup[*RuntimeFailure](r, ModRuntimeFailure).Fail(cause)
				return nil
			}
			h.svc.onShutdown = tc.shutdown(release)
			started := time.Now()
			err, returned := awaitRunWithin(t, h.start(), startupCleanupWait)
			if !returned {
				t.Fatalf("run did not return %s after a startup failure whose Service.Shutdown never finished", startupCleanupWait)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("run error = %v, want the runtime failure", err)
			}
			if elapsed := time.Since(started); elapsed > startupCleanupWait {
				t.Fatalf("run took %s", elapsed)
			}
			if n := shared.stops.Load(); n != 0 {
				t.Fatalf("mods stopped %d times while Service.Shutdown had not finished; the service may still be using them", n)
			}
			if releases := h.store.releaseCalls(); len(releases) != 0 {
				t.Fatalf("singleton released (%+v) while Service.Shutdown had not finished", releases)
			}
		})
	}
}
