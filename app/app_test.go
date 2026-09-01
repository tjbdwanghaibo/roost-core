package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
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
	a := New("cube-test", "0.0.0")
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
	a := New("cube-test", "0.0.0")
	if len(mods) > 0 {
		a.Mods(mods...)
	}
	a.RegisterServer("game", svc)
	a.cfg.Set("log.file", false)
	a.cfg.Set("log.dir", t.TempDir())
	return a
}
