// RR-20261004-NC-01/02：停止 panic 不能跳过清理，单个 Engine 只取得一次启动权。
package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

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
