package app

import (
	"context"
	"errors"
	"testing"
)

// RR-20261005-NC-232：停机开始之后才发生的 RuntimeFailure（DataEngine / Remote fatal、续期判定失锁）
// 也要出现在 run 的返回值里。安全动作（OnFail 围栏 Nest、失锁不 Release）照旧由 RuntimeFailure 自己做；
// 返回值是进程退出码与部署脚本能看到的唯一信号，启动期间的同类失败已经经 startupFailure 返回错误。
//
// 旧行为：run 在 select 里因信号醒来之后不再读 Done、也不看 Err，停机期间的失败只写一条 Error 日志，
// run 返回 nil，进程以 0 退出（N01/S4 观察 O1）。
func TestRuntimeFailureDuringShutdownIsReturned(t *testing.T) {
	shared := &singletonProbeMod{name: "probe_shared"}
	h := newSingletonHarness(t, []Mod{shared}, nil)
	cause := errors.New("dataengine fatal while draining")
	h.svc.onShutdown = func(context.Context) error {
		h.runtimeFailure(t).Fail(cause)
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	err := h.stop(t, result)
	if !errors.Is(err, cause) {
		t.Fatalf("run error = %v, want the runtime failure that happened during shutdown", err)
	}
	// 单实例锁的 Release 规则不变：这次失败不是失锁，Mod 全部停完，照常释放。
	if shared.stops.Load() != 1 || len(h.store.releaseCalls()) != 1 {
		t.Fatalf("mod stops=%d releases=%+v, want the normal shutdown to finish and release once", shared.stops.Load(), h.store.releaseCalls())
	}
}

// 第一次失败已经由 select 的 Done 分支并入返回值时不重复追加。
func TestRuntimeFailureThatStartsTheShutdownIsReturnedOnce(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	cause := errors.New("remote fatal")
	result := h.start()
	h.awaitServed(t, result)
	h.runtimeFailure(t).Fail(cause)
	err := awaitRunResult(t, result)
	if !errors.Is(err, cause) {
		t.Fatalf("run error = %v, want the runtime failure", err)
	}
	if n := countJoined(err, cause); n != 1 {
		t.Fatalf("run error mentions the failure %d times: %v", n, err)
	}
}

// countJoined 数 errors.Join 树里 target 出现的次数。
func countJoined(err, target error) int {
	if err == nil {
		return 0
	}
	if err == target {
		return 1
	}
	n := 0
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			n += countJoined(inner, target)
		}
	case interface{ Unwrap() error }:
		n += countJoined(e.Unwrap(), target)
	}
	return n
}
