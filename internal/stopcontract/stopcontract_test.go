package stopcontract

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/operation"
)

// A3：骨架本身要能抓住已知错法。每个故意写错的停机对象都必须让 Check 变红，并且红在对应的承诺上；
// 按共用 operation.Lifetime 写的正确对象必须是绿的。

// recorder 代替 *testing.T 收集 Check 的失败；Fatalf 结束当前 goroutine，与 testing 的语义一致。
type recorder struct {
	testing.TB
	mu       sync.Mutex
	failures []string
	cleanups []func()
}

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}
func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}
func (r *recorder) Cleanup(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanups = append(r.cleanups, f)
}

// runCheck 在独立 goroutine 里跑 Check，返回它报告的全部失败。
func runCheck(t *testing.T, hooks Hooks) []string {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Check(r, hooks)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Check itself did not return")
	}
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	return r.failures
}

// stopObject 是一个最小停机对象：一个在途工作（work 关闭表示返回）和一个依赖（dependency）。
type stopObject struct {
	work       chan struct{}
	dependency atomic.Bool // true = 已释放
}

func newStopObject() *stopObject { return &stopObject{work: make(chan struct{})} }

// correctStopper 按共用 Lifetime 写：发起关闭、在 ctx 内等、排空后才释放。
type correctStopper struct {
	*stopObject
	lifetime operation.Lifetime
	once     sync.Once
}

func (s *correctStopper) block() {
	s.lifetime.Begin()
	go func() {
		<-s.work
		s.lifetime.End()
	}()
}

func (s *correctStopper) Stop(ctx context.Context) error {
	if err := s.lifetime.Wait(ctx); err != nil {
		return err
	}
	s.once.Do(func() { s.dependency.Store(true) })
	return nil
}

// fieldClearingStopper 是 NC-170 / NC-171 / NC-173 / NC-83 的错法：先把在途句柄取走（清空字段），
// 超时之后重试看到字段为空就报告“已停”，依赖永远不会被释放（或在工作仍在跑时被调用方释放）。
type fieldClearingStopper struct {
	*stopObject
	mu      sync.Mutex
	pending chan struct{}
}

func (s *fieldClearingStopper) Stop(ctx context.Context) error {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	if pending == nil {
		return nil // “字段已清空 = 已停”
	}
	select {
	case <-pending:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.dependency.Store(true)
	return nil
}

// releaseFirstStopper 是 NC-173 Assembly.Close 的错法：等待失败也照样释放依赖。
type releaseFirstStopper struct{ *stopObject }

func (s *releaseFirstStopper) Stop(ctx context.Context) error {
	s.dependency.Store(true)
	select {
	case <-s.work:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unboundedStopper 是 NC-173 Deregister 的错法：裸 <-done，不看 ctx。
type unboundedStopper struct{ *stopObject }

func (s *unboundedStopper) Stop(context.Context) error {
	<-s.work
	s.dependency.Store(true)
	return nil
}

func hooksFor(object *stopObject, stop func(context.Context) error, block func()) Hooks {
	return Hooks{
		Block: func(testing.TB) {
			if block != nil {
				block()
			}
		},
		Stop:     stop,
		Release:  func() { close(object.work) },
		Released: object.dependency.Load,
		Patience: 500 * time.Millisecond,
	}
}

func TestCheckPassesAStopperBuiltOnTheSharedLifetime(t *testing.T) {
	s := &correctStopper{stopObject: newStopObject()}
	if failures := runCheck(t, hooksFor(s.stopObject, s.Stop, s.block)); len(failures) != 0 {
		t.Fatalf("a correct stopper failed the contract:\n%s", strings.Join(failures, "\n"))
	}
}

func TestCheckCatchesKnownWrongStoppers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func() Hooks
		want  []string // 每条都必须出现在某个失败里
	}{
		{
			name: "cleared field treated as stopped",
			hooks: func() Hooks {
				s := &fieldClearingStopper{stopObject: newStopObject()}
				s.pending = s.work
				return hooksFor(s.stopObject, s.Stop, nil)
			},
			want: []string{
				"retry Stop while the work is still in flight = <nil>",
				"Stop returned without releasing the resource after the work drained",
			},
		},
		{
			name: "dependency released before the drain",
			hooks: func() Hooks {
				s := &releaseFirstStopper{stopObject: newStopObject()}
				return hooksFor(s.stopObject, s.Stop, nil)
			},
			want: []string{"first Stop released the resource while the work was still in flight"},
		},
		{
			name: "bare channel receive ignores ctx",
			hooks: func() Hooks {
				s := &unboundedStopper{stopObject: newStopObject()}
				return hooksFor(s.stopObject, s.Stop, nil)
			},
			want: []string{"first Stop did not return within", "the stop ignored its context"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failures := runCheck(t, tc.hooks())
			if len(failures) == 0 {
				t.Fatal("the contract passed a wrong stopper")
			}
			joined := strings.Join(failures, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("failures do not name %q:\n%s", want, joined)
				}
			}
			t.Logf("contract failures (expected):\n%s", joined)
		})
	}
}
