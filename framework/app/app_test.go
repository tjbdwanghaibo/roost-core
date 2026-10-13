// App 生命周期：Serve / Shutdown / Mod 停止错误的返回、停机顺序与截止、启动取消与清理、停机 hook 预算、退出原因日志。

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lifecycle"
)

var errServeFailed = errors.New("serve failed")

type errService struct{}

func (s *errService) Name() ServiceName { return "game" }
func (s *errService) Init(*Registry) error {
	return nil
}
func (s *errService) Serve(context.Context) error {
	return errServeFailed
}
func (s *errService) Shutdown(context.Context) error {
	return nil
}

func TestAppReturnsServeError(t *testing.T) {
	a := newTestApp(t, &errService{})
	a.RootCmd().SetArgs([]string{"game"})

	err := a.Execute()
	if !errors.Is(err, errServeFailed) {
		t.Fatalf("Execute err = %v, want %v", err, errServeFailed)
	}
}

func TestAppExplicitConfigPathFailsClosed(t *testing.T) {
	a := newTestApp(t, &errService{})
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	a.RootCmd().SetArgs([]string{"--config", missing, "game"})

	err := a.Execute()
	if err == nil || !strings.Contains(err.Error(), "read config") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("explicit missing config error = %v", err)
	}
}

func TestAppInvalidDefaultConfigFailsClosed(t *testing.T) {
	oldWorkingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "configs", "service")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.game.yaml"), []byte("sid: [invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDirectory) })

	a := newTestApp(t, &errService{})
	a.RootCmd().SetArgs([]string{"game"})
	if err := a.Execute(); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("invalid default config error = %v", err)
	}
}

var (
	errShutdownFailed = errors.New("shutdown failed")
	errModStopFailed  = errors.New("mod stop failed")
)

type shutdownErrService struct{}

func (s *shutdownErrService) Name() ServiceName { return "game" }
func (s *shutdownErrService) Init(*Registry) error {
	return nil
}
func (s *shutdownErrService) Serve(context.Context) error {
	return nil
}
func (s *shutdownErrService) Shutdown(context.Context) error {
	return errShutdownFailed
}

func TestAppReturnsShutdownError(t *testing.T) {
	a := newTestApp(t, &shutdownErrService{})
	a.RootCmd().SetArgs([]string{"game"})

	err := a.Execute()
	if !errors.Is(err, errShutdownFailed) {
		t.Fatalf("Execute err = %v, want shutdown error", err)
	}
}

type stoppingLifecycleErrorMod struct{ err error }

func (*stoppingLifecycleErrorMod) Name() ModName           { return "stopping_lifecycle_error" }
func (*stoppingLifecycleErrorMod) Init(*viper.Viper) error { return nil }
func (*stoppingLifecycleErrorMod) Start() error            { return nil }
func (*stoppingLifecycleErrorMod) Stop()                   {}
func (mod *stoppingLifecycleErrorMod) Provide(registry *Registry) error {
	hooks, ok := Lookup[*lifecycle.Registry](registry, ModLifecycle)
	if !ok {
		return errors.New("lifecycle registry unavailable")
	}
	return hooks.Register(lifecycle.Hook{
		Name: "fail-stopping", Phase: lifecycle.PhaseServiceStopping,
		Handler: func(context.Context, lifecycle.Event) error { return mod.err },
	})
}

func TestAppReturnsServiceStoppingLifecycleError(t *testing.T) {
	want := errors.New("service stopping lifecycle failed")
	a := newTestApp(t, &errService{})
	a.Mods(&stoppingLifecycleErrorMod{err: want})
	a.RootCmd().SetArgs([]string{"game"})
	if err := a.Execute(); !errors.Is(err, want) {
		t.Fatalf("Execute err=%v, want lifecycle failure", err)
	}
}

type failingStopMod struct{}

func (m *failingStopMod) Name() ModName           { return "failing_stop" }
func (m *failingStopMod) Init(*viper.Viper) error { return nil }
func (m *failingStopMod) Provide(*Registry) error { return nil }
func (m *failingStopMod) Start() error            { return nil }
func (m *failingStopMod) Stop()                   {}
func (m *failingStopMod) StopWithContext(context.Context) error {
	return errModStopFailed
}

func TestAppReturnsModStopError(t *testing.T) {
	a := newTestApp(t, &shutdownDeadlineService{}, &failingStopMod{})
	a.RootCmd().SetArgs([]string{"game"})

	err := a.Execute()
	if !errors.Is(err, errModStopFailed) {
		t.Fatalf("Execute err = %v, want mod stop error", err)
	}
}

type shutdownDeadlineService struct {
	deadlineRemaining time.Duration
}

func (s *shutdownDeadlineService) Name() ServiceName { return "game" }
func (s *shutdownDeadlineService) Init(*Registry) error {
	return nil
}
func (s *shutdownDeadlineService) Serve(context.Context) error {
	return nil
}
func (s *shutdownDeadlineService) Shutdown(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("shutdown context has no deadline")
	}
	s.deadlineRemaining = time.Until(deadline)
	return nil
}

func TestAppUsesConfiguredShutdownTimeout(t *testing.T) {
	svc := &shutdownDeadlineService{}
	a := newTestApp(t, svc)
	a.cfg.Set("shutdown.total_timeout", 25*time.Millisecond)
	a.RootCmd().SetArgs([]string{"game"})

	if err := a.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if svc.deadlineRemaining > 500*time.Millisecond {
		t.Fatalf("shutdown deadline remaining = %v, want configured short timeout", svc.deadlineRemaining)
	}
}

type slowSignalService struct {
	started        chan struct{}
	serveExited    chan struct{}
	shutdownCalled atomic.Bool
}

func newSlowSignalService() *slowSignalService {
	return &slowSignalService{
		started:     make(chan struct{}),
		serveExited: make(chan struct{}),
	}
}

func (s *slowSignalService) Name() ServiceName { return "game" }
func (s *slowSignalService) Init(*Registry) error {
	return nil
}
func (s *slowSignalService) Serve(ctx context.Context) error {
	close(s.started)
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	close(s.serveExited)
	return nil
}
func (s *slowSignalService) Shutdown(context.Context) error {
	s.shutdownCalled.Store(true)
	return nil
}

type serveOrderingMod struct {
	serveExited     <-chan struct{}
	stoppedTooEarly atomic.Bool
}

func (m *serveOrderingMod) Name() ModName           { return "serve_ordering" }
func (m *serveOrderingMod) Init(*viper.Viper) error { return nil }
func (m *serveOrderingMod) Provide(*Registry) error { return nil }
func (m *serveOrderingMod) Start() error            { return nil }
func (m *serveOrderingMod) Stop() {
	select {
	case <-m.serveExited:
	default:
		m.stoppedTooEarly.Store(true)
	}
}

func TestAppShutsServiceBeforeStoppingDependencies(t *testing.T) {
	svc := newSlowSignalService()
	mod := &serveOrderingMod{serveExited: svc.serveExited}
	a := newTestApp(t, svc, mod)
	a.RootCmd().SetArgs([]string{"game"})
	signals := make(chan os.Signal, 1)
	a.signalSource = func() (<-chan os.Signal, func()) {
		return signals, func() {}
	}

	go func() {
		<-svc.started
		signals <- os.Interrupt
	}()

	if err := a.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !svc.shutdownCalled.Load() {
		t.Fatal("Shutdown was not called")
	}
	if mod.stoppedTooEarly.Load() {
		t.Fatal("dependency mod stopped while Serve was still running")
	}
}

type uncooperativeService struct {
	started chan struct{}
	release chan struct{}
}

func (s *uncooperativeService) Name() ServiceName    { return "game" }
func (s *uncooperativeService) Init(*Registry) error { return nil }
func (s *uncooperativeService) Serve(context.Context) error {
	close(s.started)
	<-s.release
	return nil
}
func (s *uncooperativeService) Shutdown(context.Context) error { return nil }

func TestAppKeepsDependenciesAliveWhenServeMissesShutdownDeadline(t *testing.T) {
	svc := &uncooperativeService{started: make(chan struct{}), release: make(chan struct{})}
	mod := &contextStopMod{}
	a := newTestApp(t, svc, mod)
	a.cfg.Set("shutdown.total_timeout", 20*time.Millisecond)
	a.RootCmd().SetArgs([]string{"game"})
	signals := make(chan os.Signal, 1)
	a.signalSource = func() (<-chan os.Signal, func()) { return signals, func() {} }
	go func() {
		<-svc.started
		signals <- os.Interrupt
	}()
	err := a.Execute()
	close(svc.release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want shutdown deadline", err)
	}
	if mod.stopCalled.Load() || mod.stopContextCalled.Load() {
		t.Fatal("dependencies stopped while Serve was still running")
	}
}

type contextStopMod struct {
	stopCalled        atomic.Bool
	stopContextCalled atomic.Bool
}

func (m *contextStopMod) Name() ModName           { return "context_stop" }
func (m *contextStopMod) Init(*viper.Viper) error { return nil }
func (m *contextStopMod) Provide(*Registry) error { return nil }
func (m *contextStopMod) Start() error            { return nil }
func (m *contextStopMod) Stop()                   { m.stopCalled.Store(true) }
func (m *contextStopMod) StopWithContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil stop context")
	}
	m.stopContextCalled.Store(true)
	return nil
}

func TestStopModsReversePrefersContextStopper(t *testing.T) {
	mod := &contextStopMod{}

	if err := stopModsReverseWithContext(context.Background(), []Mod{mod}, "test stop"); err != nil {
		t.Fatalf("stopModsReverseWithContext: %v", err)
	}

	if !mod.stopContextCalled.Load() {
		t.Fatal("StopWithContext was not called")
	}
	if mod.stopCalled.Load() {
		t.Fatal("Stop should not be called when StopWithContext is available")
	}
}

type panicStopMod struct{ stopped atomic.Bool }

func (m *panicStopMod) Name() ModName           { return "panic_stop" }
func (m *panicStopMod) Init(*viper.Viper) error { return nil }
func (m *panicStopMod) Provide(*Registry) error { return nil }
func (m *panicStopMod) Start() error            { return nil }
func (m *panicStopMod) Stop() {
	m.stopped.Store(true)
	panic("stop boom")
}

func TestStopModsReverseContainsPanicAndContinues(t *testing.T) {
	quick := &contextStopMod{}
	panicking := &panicStopMod{}
	err := stopModsReverseWithContext(context.Background(), []Mod{quick, panicking}, "test stop")
	if err == nil || !strings.Contains(err.Error(), "panic: stop boom") {
		t.Fatalf("panic stop error = %v", err)
	}
	if !panicking.stopped.Load() || !quick.stopContextCalled.Load() {
		t.Fatalf("reverse stop did not continue: panic=%v quick=%v", panicking.stopped.Load(), quick.stopContextCalled.Load())
	}
}

type hangingStopMod struct {
	entered chan struct{}
	release chan struct{}
}

func (m *hangingStopMod) Name() ModName           { return "hanging_stop" }
func (m *hangingStopMod) Init(*viper.Viper) error { return nil }
func (m *hangingStopMod) Provide(*Registry) error { return nil }
func (m *hangingStopMod) Start() error            { return nil }
func (m *hangingStopMod) Stop()                   {}
func (m *hangingStopMod) StopWithContext(context.Context) error {
	close(m.entered)
	<-m.release
	return nil
}

func TestStopModsReversePreservesDependenciesAfterTimeout(t *testing.T) {
	hanging := &hangingStopMod{entered: make(chan struct{}), release: make(chan struct{})}
	quick := &contextStopMod{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := stopModsReverseWithContext(ctx, []Mod{quick, hanging}, "test stop")
	close(hanging.release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error = %v, want deadline", err)
	}
	if quick.stopContextCalled.Load() {
		t.Fatal("dependency was stopped while predecessor stop remained incomplete")
	}
}

type failingProvideMod struct {
	stopped atomic.Bool
}

func (m *failingProvideMod) Name() ModName           { return "failing_provide" }
func (m *failingProvideMod) Init(*viper.Viper) error { return nil }
func (m *failingProvideMod) Provide(r *Registry) error {
	if err := r.Register("partial_capability", m); err != nil {
		return err
	}
	return errors.New("provide failed after resource creation")
}
func (m *failingProvideMod) Start() error { return nil }
func (m *failingProvideMod) Stop()        { m.stopped.Store(true) }

func TestAppStopsModWhoseProvideFails(t *testing.T) {
	mod := &failingProvideMod{}
	a := newTestApp(t, &errService{}, mod)
	a.RootCmd().SetArgs([]string{"game"})

	err := a.Execute()
	if err == nil || !strings.Contains(err.Error(), "provide failed") {
		t.Fatalf("Execute error = %v", err)
	}
	if !mod.stopped.Load() {
		t.Fatal("mod was not stopped after partial Provide failure")
	}
}

func TestAppPassesLogCallerConfig(t *testing.T) {
	dir := t.TempDir()
	a := New("roost-test", "0.0.0")
	a.RegisterServer("game", &shutdownErrService{})
	a.cfg.Set("log.file", true)
	a.cfg.Set("log.stdout", false)
	a.cfg.Set("log.dir", dir)
	a.cfg.Set("log.caller", true)
	a.cfg.Set("log.rotate_interval", 0)
	a.RootCmd().SetArgs([]string{"game"})

	err := a.Execute()
	if !errors.Is(err, errShutdownFailed) {
		t.Fatalf("Execute err = %v, want shutdown error", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "game-1000.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	line := string(raw)
	if !strings.Contains(line, "caller=") {
		t.Fatalf("log should contain caller file and line: %s", line)
	}
	if !strings.Contains(line, "caller_func=") {
		t.Fatalf("log should contain caller function: %s", line)
	}
}

func newTestApp(t *testing.T, svc Service, mods ...Mod) *App {
	t.Helper()
	a := New("roost-test", "0.0.0")
	if len(mods) > 0 {
		a.Mods(mods...)
	}
	a.RegisterServer("game", svc)
	a.cfg.Set("log.file", false)
	a.cfg.Set("log.dir", t.TempDir())
	return a
}

// app.run 的最终错误进文件日志：run 在关闭文件日志之前写一行 Error，带上最终错误（含停机期间并入的 RuntimeFailure）。
// 历史：RR-20261006-07。

// Serve / Shutdown 的错误 run 自己已经记过日志；启动阶段（Mod Init / Provide / Start、单实例锁、业务时间检查）
// 失败时直接返回，之前文件里一行都没有。这里用 Mod Init 失败。
var errExitReasonForLog = errors.New("mod init failed: exit reason 7c1e")

type exitReasonMod struct{}

func (exitReasonMod) Name() ModName           { return "exit_reason" }
func (exitReasonMod) Init(*viper.Viper) error { return errExitReasonForLog }
func (exitReasonMod) Provide(*Registry) error { return nil }
func (exitReasonMod) Start() error            { return nil }
func (exitReasonMod) Stop()                   {}

func TestRunWritesTheExitReasonToTheFileLog(t *testing.T) {
	dir := t.TempDir()
	a := New("roost-test", "0.0.0")
	a.Mods(exitReasonMod{})
	a.RegisterServer("game", &errService{})
	a.cfg.Set("log.file", true)
	a.cfg.Set("log.stdout", false)
	a.cfg.Set("log.dir", dir)
	a.cfg.Set("log.rotate_interval", 0)
	a.RootCmd().SetArgs([]string{"game"})

	if err := a.Execute(); !errors.Is(err, errExitReasonForLog) {
		t.Fatalf("Execute err = %v, want the mod init error", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "game-1000.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(raw), errExitReasonForLog.Error()) {
		t.Fatalf("the exit reason %q is not in the file log:\n%s", errExitReasonForLog, raw)
	}
}

// 反向停机在上下文已结束时立刻带着 ctx 错误返回、不再去停任何 Mod；生命周期事件在没有 registry 或 lifecycle 能力类型不对时报错而
// 不是解引用；Mod 排序拒绝 nil 条目、空名字、重名；配置校验拒绝 nil viper；能力批量注册对空名字与批内重名整批拒绝、不发布任何一项。

// enteredStopMod reports the moment any stop entry point is entered; the
// stop itself runs on a goroutine, so a flag read right after the call
// could miss it — the test waits instead.
type enteredStopMod struct {
	entered chan struct{}
	once    sync.Once
}

func (m *enteredStopMod) Name() ModName           { return "entered_stop" }
func (m *enteredStopMod) Init(*viper.Viper) error { return nil }
func (m *enteredStopMod) Provide(*Registry) error { return nil }
func (m *enteredStopMod) Start() error            { return nil }
func (m *enteredStopMod) Stop()                   { m.once.Do(func() { close(m.entered) }) }
func (m *enteredStopMod) StopWithContext(context.Context) error {
	m.Stop()
	return nil
}

func TestStopModsReverseStopsNothingOnceTheContextIsDone(t *testing.T) {
	mod := &enteredStopMod{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := stopModsReverseWithContext(ctx, []Mod{mod}, "test stop")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a finished context = %v, want context.Canceled", err)
	}
	select {
	case <-mod.entered:
		t.Fatal("a mod was stopped after the stop context had already ended")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEmitLifecycleRequiresARegistryWithTheLifecycleCapability(t *testing.T) {
	ctx := context.Background()
	event := lifecycle.Event{Phase: lifecycle.PhaseServiceStarted, Service: "game"}
	var none *App
	if err := none.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "lifecycle registry unavailable") {
		t.Fatalf("emitLifecycle on a nil app = %v", err)
	}
	if err := (&App{}).emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "lifecycle registry unavailable") {
		t.Fatalf("emitLifecycle without a registry = %v", err)
	}
	wrongType := &App{registry: &Registry{store: map[ModName]any{ModLifecycle: "not a lifecycle registry"}}}
	if err := wrongType.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "not found or wrong type") {
		t.Fatalf("emitLifecycle with a wrong-typed capability = %v", err)
	}
	missing := &App{registry: &Registry{store: map[ModName]any{}}}
	if err := missing.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "not found or wrong type") {
		t.Fatalf("emitLifecycle without the capability = %v", err)
	}
	if err := (&App{registry: NewRegistry(viper.New())}).emitLifecycle(ctx, event); err != nil {
		t.Fatalf("emitLifecycle with a real registry = %v", err)
	}
}

func TestSortModsRefusesNilUnnamedAndDuplicateMods(t *testing.T) {
	a := &orderedTestMod{name: "a"}
	if _, err := sortMods([]Mod{a, nil}, nil); err == nil || !strings.Contains(err.Error(), "mod entry 1 is nil") {
		t.Fatalf("sortMods with a nil entry = %v", err)
	}
	if _, err := sortMods([]Mod{a, &orderedTestMod{}}, nil); err == nil || !strings.Contains(err.Error(), "mod entry 1 has empty name") {
		t.Fatalf("sortMods with an unnamed mod = %v", err)
	}
	if _, err := sortMods([]Mod{a, &orderedTestMod{name: "a"}}, nil); err == nil || !strings.Contains(err.Error(), `duplicate mod "a"`) {
		t.Fatalf("sortMods with a duplicate name = %v", err)
	}
	if got, err := sortMods([]Mod{a, &orderedTestMod{name: "b", hard: []ModName{"a"}}}, nil); err != nil || len(got) != 2 {
		t.Fatalf("sortMods with a valid pair = (%v, %v)", modNames(got), err)
	}
}

func TestValidateServiceConfigAndRegisterBatchRefuseMissingInputs(t *testing.T) {
	if err := ValidateServiceConfig(nil); err == nil || !strings.Contains(err.Error(), "viper is nil") {
		t.Fatalf("ValidateServiceConfig(nil) = %v", err)
	}
	registry := NewRegistry(viper.New())
	if err := registry.RegisterBatch(Capability{Name: "first", Value: 1}, Capability{Name: "", Value: 2}); err == nil || !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("RegisterBatch with an unnamed capability = %v", err)
	}
	if err := registry.RegisterBatch(Capability{Name: "first", Value: 1}, Capability{Name: "first", Value: 2}); err == nil || !strings.Contains(err.Error(), "appears more than once in batch") {
		t.Fatalf("RegisterBatch with a duplicate in the batch = %v", err)
	}
	if _, ok := registry.Get("first"); ok {
		t.Fatal("a refused batch published one of its capabilities")
	}
}

// 停机开始之后才发生的 RuntimeFailure（DataEngine / Remote fatal、续期判定失锁）也出现在 run 的返回值里：返回值是进程退出码与部署
// 脚本能看到的唯一信号。安全动作（OnFail 围栏 Nest、失锁不 Release）照旧由 RuntimeFailure 自己做。
// 历史：RR-20261005-NC-232。
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

// 停机阶段的生命周期 hook（service.stopping / service.stopped）也受 shutdown.total_timeout 约束：hook 不配合 ctx 时 run 仍在预算内
// 如实返回“不完整”；service.stopping 的 hook 还没返回时它可能仍在用 Service 与 Mod，所以不停 Service、不停 Mod、不释放单实例锁。
// 历史：RR-20261005-NC-231。

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
			// RR-20261006-25：错误（“app run failed” 那一行）要点出卡住的是哪个 hook，同一阶段常有多个 hook
			// （Ops 的就绪位、configdata、业务登记的），只报阶段运维无从下手。
			if stuck := "blocking-" + string(tc.phase); !strings.Contains(err.Error(), strconv.Quote(stuck)) {
				t.Fatalf("run error = %v, want it to name the hook that did not return (%q)", err, stuck)
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
