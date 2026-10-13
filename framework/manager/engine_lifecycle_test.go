// 引擎生命周期：并发启动只有一个所有者、失败或停止后不再启动、停止 panic 保留原因并继续清理、停止重试等未排空的管理器。

package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// 停止 panic 不跳过清理，单个 Engine 只取得一次启动权。历史：RR-20261004-NC-01/02。
type ownershipManager struct {
	name          string
	starts, stops atomic.Int32
	startErr      error
	stopPanic     bool
}

type ownershipGatedManager struct {
	*ownershipManager
	entered, release chan struct{}
	once             sync.Once
}

func (m *ownershipGatedManager) Start(registry *app.Registry) error {
	m.once.Do(func() { close(m.entered) })
	<-m.release
	return m.ownershipManager.Start(registry)
}

func TestConcurrentStartHasOneOwner(t *testing.T) {
	managed := &ownershipGatedManager{ownershipManager: &ownershipManager{name: "one"}, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(managed.release) })
	engine := NewEngine(managed)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- engine.Start() }()
	waitFor(t, managed.entered)
	const attempts = 16
	repeated := make(chan error, attempts)
	for range attempts {
		go func() { repeated <- engine.Start() }()
	}
	for range attempts {
		select {
		case err := <-repeated:
			if !errors.Is(err, ErrStartState) {
				t.Fatalf("concurrent Start=%v, want lifecycle state rejection", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("duplicate Start waited inside user callback")
		}
	}
	releaseOnce.Do(func() { close(managed.release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if managed.starts.Load() != 1 || managed.stops.Load() != 1 {
		t.Fatalf("starts=%d stops=%d", managed.starts.Load(), managed.stops.Load())
	}
}

func TestSingleAttemptAfterFailureOrStop(t *testing.T) {
	for _, mode := range []string{"order_failure", "start_failure", "stop_before_start", "stop_after_success"} {
		t.Run(mode, func(t *testing.T) {
			managed := &ownershipManager{name: "one"}
			engine := NewEngine(managed)
			if mode == "order_failure" {
				engine = NewEngine(managed, managed)
			}
			if mode == "start_failure" {
				managed.startErr = errors.New("failed")
			}
			if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
				t.Fatal(err)
			}
			if mode == "stop_before_start" {
				_ = engine.Stop(context.Background())
			} else {
				err := engine.Start()
				if (mode == "stop_after_success") != (err == nil) {
					t.Fatalf("initial Start=%v", err)
				}
				if mode == "stop_after_success" {
					_ = engine.Stop(context.Background())
				}
			}
			starts, stops := managed.starts.Load(), managed.stops.Load()
			if err := engine.Start(); !errors.Is(err, ErrStartState) {
				t.Fatalf("second Start=%v", err)
			}
			if managed.starts.Load() != starts || managed.stops.Load() != stops {
				t.Fatal("rejected lifecycle repeated callbacks")
			}
		})
	}
}

func TestProvidePreconditionDoesNotConsumeStartup(t *testing.T) {
	managed := &ownershipManager{name: "one"}
	engine := NewEngine()
	if err := engine.Start(); err == nil {
		t.Fatal("unprovided engine started")
	}
	if err := engine.Register(managed); err != nil {
		t.Fatal(err)
	}
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(context.Background()); err != nil || managed.starts.Load() != 1 || managed.stops.Load() != 1 {
		t.Fatalf("stop=%v starts=%d stops=%d", err, managed.starts.Load(), managed.stops.Load())
	}
}

func TestLastStartupHandoverAlsoContainsStopPanic(t *testing.T) {
	managed := &ownershipGatedManager{ownershipManager: &ownershipManager{name: "last", stopPanic: true}, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(managed.release) })
	engine := NewEngine(managed)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	panics := make(chan any, 1)
	go func() { err, recovered := ownershipCall(engine.Start); done <- err; panics <- recovered }()
	waitFor(t, managed.entered)
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(managed.release) })
	if err, recovered := <-done, <-panics; err == nil || recovered != nil || managed.stops.Load() != 1 {
		t.Fatalf("handover error=%v panic=%v stops=%d", err, recovered, managed.stops.Load())
	}
}

type ownershipErrorPanicManager struct {
	*ownershipManager
	cause error
}

func (m *ownershipErrorPanicManager) Stop() { m.stops.Add(1); panic(m.cause) }

func TestStopPanicPreservesCauseAndContinues(t *testing.T) {
	cause := errors.New("stop panic cause")
	older := &ownershipManager{name: "older"}
	broken := &ownershipErrorPanicManager{ownershipManager: &ownershipManager{name: "broken"}, cause: cause}
	engine := NewEngine(older, broken)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(context.Background()); !errors.Is(err, cause) || older.stops.Load() != 1 || broken.stops.Load() != 1 {
		t.Fatalf("error=%v older=%d broken=%d", err, older.stops.Load(), broken.stops.Load())
	}
}

func (m *ownershipManager) Name() string              { return m.name }
func (m *ownershipManager) Start(*app.Registry) error { m.starts.Add(1); return m.startErr }
func (m *ownershipManager) Stop() {
	m.stops.Add(1)
	if m.stopPanic {
		panic("stop boom")
	}
}

type ownershipBoundedManager struct {
	*ownershipManager
	stopErr error
}

func (m *ownershipBoundedManager) StopWithContext(context.Context) error { m.Stop(); return m.stopErr }
func ownershipCall(fn func() error) (err error, recovered any) {
	defer func() { recovered = recover() }()
	err = fn()
	return
}

func TestStopPanicKeepsCleanupChain(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		name := "plain"
		if bounded {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			older := &ownershipManager{name: "older"}
			broken := &ownershipManager{name: "broken", stopPanic: true}
			var last app.IManager = broken
			if bounded {
				last = &ownershipBoundedManager{ownershipManager: broken}
			}
			engine := NewEngine(older, last)
			if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
				t.Fatal(err)
			}
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			err, panicked := ownershipCall(func() error { return engine.Stop(context.Background()) })
			retry, retryPanic := ownershipCall(func() error { return engine.Stop(context.Background()) })
			t.Logf("panic=%v error=%v older_stops=%d retry=%v retry_panic=%v", panicked, err, older.stops.Load(), retry, retryPanic)
			if panicked != nil || err == nil || older.stops.Load() != 1 {
				t.Fatalf("stop failure skipped cleanup: panic=%v error=%v older_stops=%d", panicked, err, older.stops.Load())
			}
		})
	}
}

func TestRollbackPanicKeepsStartFailureAndCleanup(t *testing.T) {
	startFailure := errors.New("start failed")
	older := &ownershipManager{name: "older"}
	broken := &ownershipManager{name: "broken", stopPanic: true}
	failed := &ownershipManager{name: "failed", startErr: startFailure}
	engine := NewEngine(older, broken, failed)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	err, panicked := ownershipCall(engine.Start)
	t.Logf("panic=%v error=%v older_stops=%d failed_stops=%d", panicked, err, older.stops.Load(), failed.stops.Load())
	if panicked != nil || !errors.Is(err, startFailure) || older.stops.Load() != 1 || failed.stops.Load() != 0 {
		t.Fatalf("rollback lost cause/cleanup: panic=%v error=%v older_stops=%d failed_stops=%d", panicked, err, older.stops.Load(), failed.stops.Load())
	}
}

func TestReturnedStopErrorContinuesCleanup(t *testing.T) {
	stopFailure := errors.New("stop failed")
	older := &ownershipManager{name: "older"}
	broken := &ownershipBoundedManager{ownershipManager: &ownershipManager{name: "broken"}, stopErr: stopFailure}
	engine := NewEngine(older, broken)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Stop(context.Background()); !errors.Is(err, stopFailure) || older.stops.Load() != 1 {
		t.Fatalf("error=%v older_stops=%d", err, older.stops.Load())
	}
}

func TestRepeatedStartDoesNotDuplicateManagers(t *testing.T) {
	manager := &ownershipManager{name: "once"}
	engine := NewEngine(manager)
	if err := engine.Provide(app.NewRegistry(viper.New())); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	second := engine.Start() // either explicit rejection or idempotence is acceptable
	stop := engine.Stop(context.Background())
	t.Logf("second_start=%v stop=%v starts=%d stops=%d", second, stop, manager.starts.Load(), manager.stops.Load())
	if manager.starts.Load() != 1 || manager.stops.Load() != 1 {
		t.Fatalf("one lifecycle executed twice: starts=%d stops=%d", manager.starts.Load(), manager.stops.Load())
	}
}

// 停止超时后，没有停完的 manager 与它的依赖都留给重试；重试在自己的 ctx 内继续等它，不能凭“列表已取走”报告成功。
// drainingManager 的 StopWithContext 发起关闭后等 drained 关闭（模拟在途工作排空），ctx 先结束就如实返回 ctx 错误；再次调用会再等同一个 drained。
type drainingManager struct {
	name      string
	dependsOn []string
	initiated chan struct{}
	drained   chan struct{}
	once      sync.Once
	calls     atomic.Int32
	running   atomic.Bool
}

func (m *drainingManager) Name() string              { return m.name }
func (m *drainingManager) DependsOn() []string       { return m.dependsOn }
func (m *drainingManager) Start(*app.Registry) error { m.running.Store(true); return nil }
func (m *drainingManager) Stop()                     { _ = m.StopWithContext(context.Background()) }

func (m *drainingManager) StopWithContext(ctx context.Context) error {
	m.calls.Add(1)
	m.once.Do(func() { close(m.initiated) })
	select {
	case <-m.drained:
		m.running.Store(false)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dependencyManager 只有无期限的 Stop；它被停时记录依赖它的 manager 是否仍在运行。
type dependencyManager struct {
	name              string
	dependent         *drainingManager
	stops             atomic.Int32
	stoppedUnderUsage atomic.Bool
}

func (m *dependencyManager) Name() string              { return m.name }
func (m *dependencyManager) Start(*app.Registry) error { return nil }
func (m *dependencyManager) Stop() {
	m.stops.Add(1)
	if m.dependent.running.Load() {
		m.stoppedUnderUsage.Store(true)
	}
}

func TestEngineStopRetryWaitsForTheManagerTheFirstStopCouldNotDrain(t *testing.T) {
	dependent := &drainingManager{name: "dependent", dependsOn: []string{"dependency"}, initiated: make(chan struct{}), drained: make(chan struct{})}
	dependency := &dependencyManager{name: "dependency", dependent: dependent}
	var drainOnce sync.Once
	defer drainOnce.Do(func() { close(dependent.drained) })
	engine, _ := newStartedEngine(t, dependency, dependent)

	// 第一次停止：dependent 发起关闭后调用方预算耗尽。
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { first <- engine.Stop(ctx) }()
	waitFor(t, dependent.initiated)
	cancel()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first Stop = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first Stop ignored its context")
	}
	if dependency.stops.Load() != 0 {
		t.Errorf("dependency stopped %d time(s) while its dependent was still draining (stopped under usage=%v)", dependency.stops.Load(), dependency.stoppedUnderUsage.Load())
	}

	// 重试且预算已用尽：dependent 仍在运行，不能报告成功。
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	if err := engine.Stop(expired); err == nil {
		t.Errorf("retry Stop reported success while manager %q was still running (calls=%d)", dependent.name, dependent.calls.Load())
	}

	// 排空后用新 ctx 重试：等到 dependent 停完，再停依赖，各停一次。
	drainOnce.Do(func() { close(dependent.drained) })
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop after drain = %v", err)
	}
	if dependent.running.Load() {
		t.Fatal("dependent still running after a successful Stop")
	}
	if dependency.stops.Load() != 1 || dependency.stoppedUnderUsage.Load() {
		t.Fatalf("dependency stops=%d stoppedUnderUsage=%v, want exactly one stop after the dependent drained", dependency.stops.Load(), dependency.stoppedUnderUsage.Load())
	}
	if err := engine.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after full drain = %v", err)
	}
	if dependency.stops.Load() != 1 {
		t.Fatalf("dependency stopped %d times, want 1", dependency.stops.Load())
	}
}

// 并发的第二个 Stop 不能在第一个仍在停 manager 时绕过它报告成功，也不能无视自己的 ctx 等下去。
func TestEngineConcurrentStopWaitsWithinItsOwnContext(t *testing.T) {
	dependent := &drainingManager{name: "dependent", initiated: make(chan struct{}), drained: make(chan struct{})}
	var drainOnce sync.Once
	defer drainOnce.Do(func() { close(dependent.drained) })
	engine, _ := newStartedEngine(t, dependent)

	first := make(chan error, 1)
	go func() { first <- engine.Stop(context.Background()) }()
	waitFor(t, dependent.initiated)

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { second <- engine.Stop(ctx) }()
	cancel()
	select {
	case err := <-second:
		if err == nil {
			t.Fatal("concurrent Stop reported success while the first Stop was still draining")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent Stop ignored its context")
	}
	drainOnce.Do(func() { close(dependent.drained) })
	if err := <-first; err != nil {
		t.Fatalf("first Stop = %v", err)
	}
	if got := dependent.calls.Load(); got != 1 {
		t.Fatalf("StopWithContext calls = %d, want 1 (the second caller must not stop it concurrently)", got)
	}
}

// Start 途中收到 Stop，最后一个（含唯一一个）管理器成功启动后恰好由一方清理，不在正常关闭后继续运行。
// 历史：RR-20260916-06。
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
