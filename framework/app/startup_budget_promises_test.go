package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
)

// RR-20261007-06：启动 hook 未返回时期限与 SIGTERM 都能结束 App 等待，
// 但不能释放该 hook 仍可能访问的 Mod 和单实例锁。
func TestStartupHookCancellationPreservesDependencies(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "signal"}[interrupted], func(t *testing.T) {
			shared := &singletonProbeMod{name: "probe_shared"}
			h := newSingletonHarness(t, []Mod{shared}, nil)
			h.app.cfg.Set("startup.timeout", "200ms")
			entered, release := make(chan struct{}), make(chan struct{})
			h.svc.onInit = func(r *Registry) error {
				registerBlockingHook(t, r, lifecycle.PhaseServiceStarted, release, entered)
				return nil
			}
			result := h.start()
			t.Cleanup(func() { close(release) })
			select {
			case <-entered:
			case <-time.After(testWaitLimit):
				t.Fatal("startup hook never entered")
			}
			want := context.DeadlineExceeded
			if interrupted {
				want = context.Canceled
				h.signals <- os.Interrupt
			}
			err, returned := awaitRunWithin(t, result, time.Second)
			if !returned || !errors.Is(err, want) {
				t.Fatalf("startup returned=%v err=%v, want %v", returned, err, want)
			}
			if shared.stops.Load() != 0 || len(h.store.releaseCalls()) != 0 {
				t.Fatal("startup callback still running but its dependencies were released")
			}
		})
	}
}

func TestSignalDuringModStartNeverReachesServiceInit(t *testing.T) {
	entered, release, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
	shared := &singletonProbeMod{name: "probe_shared", onStart: func() {
		close(entered)
		<-release
		close(exited)
	}}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	result := h.start()
	t.Cleanup(func() { close(release); <-exited })
	select {
	case <-entered:
	case <-time.After(testWaitLimit):
		t.Fatal("mod never started")
	}
	h.signals <- os.Interrupt
	err, returned := awaitRunWithin(t, result, time.Second)
	if !returned || !errors.Is(err, context.Canceled) {
		t.Fatalf("startup returned=%v err=%v", returned, err)
	}
	if h.svc.registry != nil || shared.stops.Load() != 0 || len(h.store.releaseCalls()) != 0 {
		t.Fatal("interrupted startup entered service Init or released live dependencies")
	}
}
