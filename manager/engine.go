// Package manager is the lifecycle engine for a service's in-memory singleton
// managers: dependency-ordered start, reverse-order stop, rollback of only the
// managers that actually started, and a shutdown that can abort a start still
// in progress.
//
// A manager is process-wide singleton logic — a scene registry, a routing
// table, a mail cache — with a Start/Stop lifecycle and no persistent state of
// its own. app declares the contract (app.IManager,
// app.ManagerDependencyProvider, app.IManagerStopperWithContext); this package
// drives it. The app.Mod that owns the engine inside a service (name, config,
// capability registration under the kit's mod name) is roost-kit/manager's
// ManagerMod, which wraps an Engine (ARCH-02, M-09).
//
// Managers are per service, not per process: the same singleton may appear in
// several services' manager sets, and only the service actually running starts
// it. That is why an Engine is constructed per service rather than shared.
package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// ErrRegisterAfterStart is returned by Register once Start has run. A
// manager added after Start would never be started and never be stopped, and
// the only symptom would be a nil dependency somewhere later — so the engine
// refuses instead of silently dropping it.
var ErrRegisterAfterStart = errors.New("manager: Register after Start")

// ErrStartState is returned after an engine has claimed its startup attempt or
// received Stop. A new lifecycle requires a new Engine, including after failure.
var ErrStartState = errors.New("manager: lifecycle already started or stopped")

// Engine starts a service's managers in dependency order and stops them in
// reverse. Layers: Service -> Mod (kit ManagerMod) -> Engine -> IManager.
type Engine struct {
	mu       sync.Mutex
	managers []app.IManager
	started  []app.IManager
	registry *app.Registry
	// starting is set the moment Start takes its snapshot of managers and
	// never cleared: from then on the set is closed. It is a separate flag
	// because started stays nil until the FIRST manager finishes, and a
	// Register that slipped in during that first Start was accepted into a
	// list nobody would ever start or stop (RR-20260916-07).
	starting bool
	stopping bool
	// stopTurn 是停止 manager 的执行权（容量 1）：同一时刻只有一个调用方在停，其余调用方按自己的
	// ctx 等待，不会绕过正在停的那个报告成功（RR-20261005-NC-170）。
	stopTurn chan struct{}
}

// NewEngine builds an engine for the given managers, in registration order.
// Order among managers that declare no dependency on each other is preserved.
func NewEngine(managers ...app.IManager) *Engine {
	return &Engine{managers: append([]app.IManager(nil), managers...)}
}

// Register appends a manager. Assembly normally happens through NewEngine;
// Register exists for conditional wiring.
func (e *Engine) Register(manager app.IManager) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.starting || e.started != nil {
		return fmt.Errorf("%w: %q", ErrRegisterAfterStart, managerName(manager))
	}
	e.managers = append(e.managers, manager)
	return nil
}

// MustRegister is Register for assembly code that has no error path. It panics
// rather than continue with a manager that will never start.
func (e *Engine) MustRegister(manager app.IManager) {
	if err := e.Register(manager); err != nil {
		panic(err)
	}
}

// Managers returns the registered managers in registration order. The slice is
// a copy: the engine's own list drives the lifecycle and must not be editable
// from outside it.
func (e *Engine) Managers() []app.IManager {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]app.IManager(nil), e.managers...)
}

// Manager returns the registered manager with the given name.
func (e *Engine) Manager(name string) (app.IManager, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, manager := range e.managers {
		if manager != nil && manager.Name() == name {
			return manager, true
		}
	}
	return nil, false
}

// Provide binds the registry that every manager receives from Start. It is
// named after app.Mod.Provide because the owning Mod forwards to it verbatim;
// the engine itself registers nothing (which capability name to publish under
// is the Mod's business).
func (e *Engine) Provide(r *app.Registry) error {
	if r == nil {
		return fmt.Errorf("manager: registry is nil")
	}
	e.mu.Lock()
	e.registry = r
	e.mu.Unlock()
	return nil
}

// Start starts every registered manager in dependency order. A failure rolls
// back the managers that reported success (newest first) and leaves the one
// that failed alone; a shutdown requested while Start is still working aborts
// the remaining managers instead of racing the stop. Once Provide is available,
// an engine admits one startup attempt; subsequent calls return ErrStartState.
func (e *Engine) Start() error {
	e.mu.Lock()
	// RR-20261004-NC-02：在快照前取得唯一启动权，不能重复调用 singleton 的 Start。
	if e.starting || e.stopping {
		e.mu.Unlock()
		return ErrStartState
	}
	registry := e.registry
	// A manager's only handle on the rest of the service is the registry it
	// receives from Start. Starting without one hands every manager a nil
	// registry, which fails much later and far from the cause.
	if registry == nil {
		e.mu.Unlock()
		return fmt.Errorf("manager: Start before Provide, no registry available")
	}
	pending := append([]app.IManager(nil), e.managers...)
	e.starting = true
	e.mu.Unlock()

	ordered, err := Order(pending)
	if err != nil {
		return err
	}

	for _, manager := range ordered {
		// Shutdown can arrive while startup is still working through the
		// list — a SIGTERM during a slow start. Abort here instead of racing
		// it: continuing would start managers that Stop has already passed,
		// leaving them running after shutdown reported success.
		if e.stopRequested() {
			if stopErr := e.stopStarted(fctx.BaseContext()); stopErr != nil {
				return fmt.Errorf("manager: start aborted by shutdown, rollback: %w", stopErr)
			}
			return fmt.Errorf("manager: start aborted by shutdown")
		}
		begin := time.Now()
		slog.Info("manager start", "name", manager.Name())
		// A manager whose Start fails is deliberately NOT stopped: it never
		// completed, so Stop would have to cope with a half-built object.
		// Start owns cleanup of its own failure; the engine rolls back only
		// the managers that reported success.
		if err := manager.Start(registry); err != nil {
			startErr := fmt.Errorf("manager %s start: %w", manager.Name(), err)
			if stopErr := e.stopStarted(fctx.BaseContext()); stopErr != nil {
				return errors.Join(startErr, fmt.Errorf("rollback: %w", stopErr))
			}
			return startErr
		}
		elapsed := time.Since(begin)
		metrics.ObserveHistogram("manager.start.duration", metrics.Labels{"manager": manager.Name()}, elapsed)
		slog.Info("manager started", "name", manager.Name(), "elapsed", elapsed)

		// Hand-over happens under the state lock: either this manager joins
		// started (and Stop will find it there), or Stop has already drained
		// started and this path owns the cleanup. The loop-top check alone is
		// not enough — for the LAST manager there is no next iteration, so a
		// Stop that landed while it was starting used to leave it running
		// with Start reporting success (RR-20260916-06).
		e.mu.Lock()
		if e.stopping {
			e.mu.Unlock()
			if stopErr := stopOne(fctx.BaseContext(), manager); stopErr != nil {
				return fmt.Errorf("manager: start aborted by shutdown, rollback: %w", stopErr)
			}
			return fmt.Errorf("manager: start aborted by shutdown")
		}
		e.started = append(e.started, manager)
		count := len(e.started)
		e.mu.Unlock()
		metrics.SetGauge("manager.started", nil, int64(count))
	}
	return nil
}

// Stop stops the managers that actually started, newest first, preferring
// app.IManagerStopperWithContext when a manager implements it. A nil ctx
// means the process base context. A manager whose stop fails with an ordinary
// error counts as stopped and the rest still get their chance; every failure is
// reported (errors.Join). A stop cut short by ctx (Canceled / DeadlineExceeded)
// ends the call at once and keeps that manager and the older ones — its
// dependencies — registered, so a later Stop with a fresh ctx continues from it
// (RR-20261005-NC-170). Once everything has stopped, Stop returns nil.
func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	return e.stopStarted(ctx)
}

// stopStarted 按三步停机（RR-20261005-NC-170，roost-coding 生命周期复审要点）：
//
//  1. 发起：置 stopping（幂等），Start 据此中止、不再把 manager 交给 started。
//  2. 等待：取得停止执行权后逆序在 ctx 内停止仍登记的 manager；某个 manager 停完（含普通错误）
//     才从 started 移除。它因 ctx 取消 / 超时没有停完时立即返回 ctx 错误。
//  3. 释放：没有停完的 manager 与更早启动的（它的依赖）都留在 started 里，不在过期的 ctx 下继续
//     停依赖；用新 ctx 重试从它继续。
//
// 修前先把 started 换成空切片再逐个停：第一次超时后，第二次 Stop 看到空列表返回 nil，manager 实际
// 仍在运行；同一次调用还会继续停它的依赖。
func (e *Engine) stopStarted(ctx context.Context) error {
	e.mu.Lock()
	e.stopping = true
	// A non-nil empty slice, not nil: it marks the lifecycle as over, so a
	// later Register is still refused even when nothing had started yet.
	if e.started == nil {
		e.started = []app.IManager{}
	}
	if e.stopTurn == nil {
		e.stopTurn = make(chan struct{}, 1)
	}
	turn := e.stopTurn
	e.mu.Unlock()

	select {
	case turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-turn }()

	var joined error
	for {
		e.mu.Lock()
		n := len(e.started)
		if n == 0 {
			e.mu.Unlock()
			break
		}
		manager := e.started[n-1]
		e.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		err := stopOne(ctx, manager)
		if stopIncomplete(err) {
			return errors.Join(joined, err)
		}
		joined = errors.Join(joined, err)
		e.mu.Lock()
		// stopping 已置位，Start 不会再追加；持有执行权时只有这里缩短 started。
		e.started = e.started[:n-1]
		count := len(e.started)
		e.mu.Unlock()
		metrics.SetGauge("manager.started", nil, int64(count))
	}
	return joined
}

// stopIncomplete 与 app 的 Mod 停机同一判定：ctx 取消 / 超时表示没有停完，其他错误算已停完。
func stopIncomplete(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// stopOne stops a single manager, preferring the bounded hook. It is shared
// by the reverse-order drain and by a Start that finished after Stop had
// already drained (RR-20260916-06), so both paths stop a manager the same way.
func stopOne(ctx context.Context, manager app.IManager) (err error) {
	name := "<unknown>"
	// RR-20261004-NC-01：逐 manager 隔离 panic，回滚和逆序停机都必须继续清理其余对象。
	defer func() {
		if recovered := recover(); recovered != nil {
			if cause, ok := recovered.(error); ok {
				err = fmt.Errorf("manager %s stop panic: %w", name, cause)
			} else {
				err = fmt.Errorf("manager %s stop panic: %v", name, recovered)
			}
		}
	}()
	name = manager.Name()
	slog.Info("manager stop", "name", name)
	if stopper, ok := manager.(app.IManagerStopperWithContext); ok {
		if err := stopper.StopWithContext(ctx); err != nil {
			return fmt.Errorf("manager %s stop: %w", name, err)
		}
		return nil
	}
	manager.Stop()
	return nil
}

func (e *Engine) stopRequested() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopping
}

func managerName(manager app.IManager) string {
	if manager == nil {
		return "<nil>"
	}
	return manager.Name()
}
