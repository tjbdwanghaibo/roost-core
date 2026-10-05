// Package stopcontract 是停机对象共用的契约测试骨架（维护者决定 A3，2026-10-05）。
//
// roost-coding 的“三步停机”：①发起关闭（幂等）→ ②在调用方 ctx 内等待排空（可重试）→ ③排空后才释放。
// 同一不变量在 RR-20261004-07/08、NC-04、NC-09、NC-83、NC-90、NC-170～174 里被各自的实现反复打破，
// 典型错法是“字段已清空 / once 已执行 / 列表已取走 = 已停”：第一次超时之后的重试在工作仍在跑时
// 报告成功，并提前释放它的依赖。Check 用同一组断言覆盖每个停机对象：
//
//  1. 工作卡住时，带短预算的 Stop 在预算附近返回 ctx 错误，资源仍被保留；
//  2. 仍未放行时再用新的短预算重试，同样返回 ctx 错误、资源仍被保留（抓“重试凭清空的字段报告成功”）；
//  3. 放行之后用新 ctx 重试返回 nil，资源确实已释放；
//  4. 再次调用返回 nil。
//
// 每一次 Stop 都在独立 goroutine 里调用并有界等待：无视 ctx 的停止（裸通道接收、普通 Mutex）
// 以“Stop ignored its context”失败，而不是拖到包超时。
//
// 本包只给测试用（导入 testing），放在 internal 下不成为公开 API。生成工程（另一个模块）不能导入它，
// codegen 的生成 TCP 契约用例把本文件按源码注入生成包再运行（见 codegen/internal/roost）。
package stopcontract

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hooks 描述一个被测停机对象。Stop、Release、Released 必填；Start、Block 可为 nil。
type Hooks struct {
	// Start 构造并启动被测对象。
	Start func(t testing.TB)
	// Block 投递一个会卡住的工作，返回时该工作已经在途（由钩子自己有界等待确认）。
	// 被测对象启动后本身就有卡住的工作时可为 nil。
	Block func(t testing.TB)
	// Stop 调用被测的停止入口。
	Stop func(ctx context.Context) error
	// Release 放行卡住的工作。骨架保证只调用一次（失败时由 Cleanup 兜底调用）。
	Release func()
	// Released 报告停止负责释放的资源 / 依赖是否已经释放。必须观察真实后果（依赖被关闭、
	// 连接被交还、登记被撤销），不能读被测对象自己的“已停”字段。
	Released func() bool

	// Budget 是第 1、2 步每次停止的预算，默认 50ms。
	Budget time.Duration
	// Patience 是骨架等待一次 Stop 返回的额外宽限（在预算之外），默认 5s；超过即判定 Stop 无视 ctx。
	Patience time.Duration
}

// Check 按“三步停机”契约检查 hooks 描述的停机对象，失败通过 t 报告。
func Check(t testing.TB, hooks Hooks) {
	t.Helper()
	if hooks.Stop == nil || hooks.Release == nil || hooks.Released == nil {
		t.Fatalf("stopcontract: Stop, Release and Released hooks are required")
		return
	}
	budget := hooks.Budget
	if budget <= 0 {
		budget = 50 * time.Millisecond
	}
	patience := hooks.Patience
	if patience <= 0 {
		patience = 5 * time.Second
	}
	release := sync.OnceFunc(hooks.Release)
	t.Cleanup(release) // 断言失败提前退出时也放行，不留卡住的 goroutine

	if hooks.Start != nil {
		hooks.Start(t)
	}
	if hooks.Block != nil {
		hooks.Block(t)
	}

	// 1. 第一次停止：预算在等卡住的工作时耗尽。
	if err := stopWithin(t, hooks.Stop, budget, patience, "first Stop"); !isContextError(err) {
		t.Errorf("stopcontract: first Stop with work in flight = %v, want the ctx error (the stop must not report a drain that did not happen)", err)
	}
	if hooks.Released() {
		t.Errorf("stopcontract: first Stop released the resource while the work was still in flight")
	}

	// 2. 仍未放行时用新的短预算重试：必须再等同一批工作，不能凭已清空的字段 / 已执行的 once 报告成功。
	if err := stopWithin(t, hooks.Stop, budget, patience, "retry Stop before release"); !isContextError(err) {
		t.Errorf("stopcontract: retry Stop while the work is still in flight = %v, want the ctx error (a retry must wait again, not trust state cleared by the first attempt)", err)
	}
	if hooks.Released() {
		t.Errorf("stopcontract: retry Stop released the resource while the work was still in flight")
	}

	// 3. 放行后用新 ctx 重试：等到排空、释放资源、返回 nil。
	release()
	if err := stopWithin(t, hooks.Stop, 0, patience, "retry Stop after release"); err != nil {
		t.Errorf("stopcontract: retry Stop after the work returned = %v, want nil", err)
	}
	if !hooks.Released() {
		t.Errorf("stopcontract: Stop returned without releasing the resource after the work drained")
	}

	// 4. 已停完的对象再调用返回 nil。
	if err := stopWithin(t, hooks.Stop, 0, patience, "Stop after a completed stop"); err != nil {
		t.Errorf("stopcontract: Stop after a completed stop = %v, want nil", err)
	}
}

// CallerReleases 给只负责排空、自己不持有依赖的停机对象接上调用方协议：只有停止返回 nil 之后，调用方
// （App 的 Mod 停机、kit Mod 交还总线 / 连接）才释放依赖。返回的 stop 在被测停止返回 nil 时把依赖标记为
// 已释放，released 报告这个标记。被测停止在工作仍在途时返回 nil，依赖就会在工作运行时被释放，Check 的
// 第 1、2 步据此变红。
func CallerReleases(stop func(context.Context) error) (wrapped func(context.Context) error, released func() bool) {
	var dependencyReleased atomic.Bool
	wrapped = func(ctx context.Context) error {
		err := stop(ctx)
		if err == nil {
			dependencyReleased.Store(true)
		}
		return err
	}
	return wrapped, dependencyReleased.Load
}

// stopWithin 在独立 goroutine 里调用 stop：budget > 0 时 ctx 带该超时，否则不限时；
// 等待 budget + patience，超过即判定停止无视 ctx（Fatalf，Cleanup 会放行卡住的工作）。
func stopWithin(t testing.TB, stop func(context.Context) error, budget, patience time.Duration, label string) error {
	t.Helper()
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if budget > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), budget)
	}
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- stop(ctx) }()
	timer := time.NewTimer(budget + patience)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatalf("stopcontract: %s did not return within %v (budget %v): the stop ignored its context", label, budget+patience, budget)
		return nil
	}
}

func isContextError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
