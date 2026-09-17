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
// the remaining managers instead of racing the stop.
func (e *Engine) Start() error {
	e.mu.Lock()
	registry := e.registry
	pending := append([]app.IManager(nil), e.managers...)
	e.starting = true
	e.mu.Unlock()

	// A manager's only handle on the rest of the service is the registry it
	// receives from Start. Starting without one hands every manager a nil
	// registry, which fails much later and far from the cause.
	if registry == nil {
		return fmt.Errorf("manager: Start before Provide, no registry available")
	}

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
// means the process base context. Every manager gets its chance to stop and
// every failure is reported (errors.Join). Stop is idempotent.
func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	return e.stopStarted(ctx)
}

// stopStarted stops the managers that actually started, newest first, and is
// idempotent: the started list is taken under the lock so a second Stop (or a
// Stop racing the rollback inside Start) finds nothing left to do rather than
// stopping a manager twice.
func (e *Engine) stopStarted(ctx context.Context) error {
	e.mu.Lock()
	started := e.started
	// A non-nil empty slice, not nil: it marks the lifecycle as over, so a
	// later Register is still refused even when nothing had started yet.
	e.started = []app.IManager{}
	e.stopping = true
	e.mu.Unlock()

	var joined error
	for i := len(started) - 1; i >= 0; i-- {
		joined = errors.Join(joined, stopOne(ctx, started[i]))
	}
	metrics.SetGauge("manager.started", nil, 0)
	return joined
}

// stopOne stops a single manager, preferring the bounded hook. It is shared
// by the reverse-order drain and by a Start that finished after Stop had
// already drained (RR-20260916-06), so both paths stop a manager the same way.
func stopOne(ctx context.Context, manager app.IManager) error {
	slog.Info("manager stop", "name", manager.Name())
	if stopper, ok := manager.(app.IManagerStopperWithContext); ok {
		if err := stopper.StopWithContext(ctx); err != nil {
			return fmt.Errorf("manager %s stop: %w", manager.Name(), err)
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
