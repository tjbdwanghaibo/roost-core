// RR-20261005-NC-170：停止超时后，没有停完的 manager 与它的依赖都要留给重试；
// 重试在自己的 ctx 内继续等它，不能凭“列表已取走”报告成功。
package manager

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
)

// drainingManager 的 StopWithContext 发起关闭后等 drained 关闭（模拟在途工作排空），
// ctx 先结束就如实返回 ctx 错误；再次调用会再等同一个 drained。
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
