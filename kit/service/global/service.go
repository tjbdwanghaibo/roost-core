package global

import (
	"context"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Config wires a Service.
type Config struct {
	// Routes holds the durable state. It is a versioned store, so there is no
	// write path that skips the comparison — the property the boundary
	// document asked for and four hand-written stores did not keep.
	Routes versionstore.Store[int32, RouteBinding]

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// A stale epoch is the refusal this package exists to make. It was
	// unobservable in the implementation it replaces — the epoch was guarded
	// by an in-process lock — so a migration that lost a race produced no
	// error, no log and no counter. Counting it is how an operator sees it
	// happening at all.
	Metrics servicemetrics.Reporter
}

// Service coordinates routing for a set of game servers: which global group
// and instance serve each one.
//
// It used to hold each game server's liveness lease too. That moved to the
// App: a process's liveness is its singleton lock, which every service type
// gets from app.Singleton and any process reads with app.SingletonLiveness.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

func New(cfg Config) (*Service, error) {
	if cfg.Routes == nil {
		return nil, fmt.Errorf("global: route store is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

// --- route binding ---

// Bind creates the first binding for a game server. It is insert-only: a
// game server that already has a binding is refused with ErrConflict, and
// moving it goes through BeginMigration / CompleteMigration, which require
// the current epoch. (There is no Rebind.)
func (s *Service) Bind(ctx context.Context, gameSID int32, groupID string, globalSID int32) (RouteBinding, error) {
	binding := RouteBinding{
		GameSID: gameSID, GlobalGroupID: groupID, GlobalSID: globalSID,
		Epoch: 1, State: RouteActive, UpdatedAtUnix: s.cfg.Now().Unix(),
	}
	if err := binding.Validate(); err != nil {
		return RouteBinding{}, err
	}
	stored, created, err := s.cfg.Routes.Create(ctx, gameSID, binding)
	if err != nil {
		return RouteBinding{}, err
	}
	if !created {
		s.report.Conflict("bind")
		return RouteBinding{}, fmt.Errorf("%w: game %d is already bound", ErrConflict, gameSID)
	}
	s.report.Accepted("bind")
	return stored.Value, nil
}

// Resolve returns the current binding.
func (s *Service) Resolve(ctx context.Context, gameSID int32) (RouteBinding, error) {
	current, found, err := s.cfg.Routes.Get(ctx, gameSID)
	if err != nil {
		return RouteBinding{}, err
	}
	if !found {
		return RouteBinding{}, fmt.Errorf("%w: game %d", ErrRouteMissing, gameSID)
	}
	return current.Value, nil
}

// BeginMigration marks a binding as moving to targetGlobalSID.
//
// expectEpoch is the epoch the caller read. Presenting it is what makes
// concurrent migrations safe: the second caller's epoch is stale and it is
// refused, without any process holding a lock. The implementation this
// replaces used an in-process mutex, which is no guarantee at all once there
// is more than one instance — and the boundary document said so explicitly.
func (s *Service) BeginMigration(ctx context.Context, gameSID int32, targetGlobalSID int32, expectEpoch uint64) (RouteBinding, error) {
	if targetGlobalSID <= 0 {
		return RouteBinding{}, fmt.Errorf("%w: target global sid must be positive", ErrRouteInvalid)
	}
	now := s.cfg.Now()
	var result RouteBinding
	_, _, err := s.cfg.Routes.Update(ctx, gameSID, func(current RouteBinding, found bool) (RouteBinding, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: game %d", ErrRouteMissing, gameSID)
		}
		if current.Epoch != expectEpoch {
			s.report.Refused("begin_migration", "stale_epoch")
			return current, false, fmt.Errorf("%w: game %d is at epoch %d, caller presented %d",
				ErrRouteStale, gameSID, current.Epoch, expectEpoch)
		}
		if current.State == RouteMigrating {
			return current, false, fmt.Errorf("%w: game %d is already moving to %d",
				ErrRouteMigrating, gameSID, current.TargetGlobalSID)
		}
		if targetGlobalSID == current.GlobalSID {
			return current, false, fmt.Errorf("%w: game %d is already served by %d",
				ErrRouteInvalid, gameSID, targetGlobalSID)
		}
		next := current
		next.State = RouteMigrating
		next.TargetGlobalSID = targetGlobalSID
		next.Epoch = current.Epoch + 1
		next.UpdatedAtUnix = now.Unix()
		result = next
		return next, true, nil
	})
	if err != nil {
		return RouteBinding{}, err
	}
	s.report.Accepted("begin_migration")
	return result, nil
}

// CompleteMigration moves a migrating binding to its target.
func (s *Service) CompleteMigration(ctx context.Context, gameSID int32, expectEpoch uint64) (RouteBinding, error) {
	now := s.cfg.Now()
	var result RouteBinding
	var replayed bool
	_, _, err := s.cfg.Routes.Update(ctx, gameSID, func(current RouteBinding, found bool) (RouteBinding, bool, error) {
		replayed = false
		if !found {
			return current, false, fmt.Errorf("%w: game %d", ErrRouteMissing, gameSID)
		}
		if current.Epoch != expectEpoch {
			s.report.Refused("complete_migration", "stale_epoch")
			return current, false, fmt.Errorf("%w: game %d is at epoch %d, caller presented %d",
				ErrRouteStale, gameSID, current.Epoch, expectEpoch)
		}
		if current.State != RouteMigrating {
			// Idempotent when already complete at this epoch: a retried
			// completion must not fail, and it cannot be confused with a new
			// migration because the epoch would have moved.
			result, replayed = current, true
			return current, false, nil
		}
		next := current
		next.GlobalSID = current.TargetGlobalSID
		next.TargetGlobalSID = 0
		next.State = RouteActive
		next.Epoch = current.Epoch + 1
		next.UpdatedAtUnix = now.Unix()
		result = next
		return next, true, nil
	})
	if err != nil {
		return RouteBinding{}, err
	}
	if replayed {
		s.report.Replayed("complete_migration")
	} else {
		s.report.Accepted("complete_migration")
	}
	return result, nil
}

// AbortMigration returns a migrating binding to its current instance.
func (s *Service) AbortMigration(ctx context.Context, gameSID int32, expectEpoch uint64) (RouteBinding, error) {
	now := s.cfg.Now()
	var result RouteBinding
	_, _, err := s.cfg.Routes.Update(ctx, gameSID, func(current RouteBinding, found bool) (RouteBinding, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: game %d", ErrRouteMissing, gameSID)
		}
		if current.Epoch != expectEpoch {
			s.report.Refused("abort_migration", "stale_epoch")
			return current, false, fmt.Errorf("%w: game %d is at epoch %d, caller presented %d",
				ErrRouteStale, gameSID, current.Epoch, expectEpoch)
		}
		if current.State != RouteMigrating {
			result = current
			return current, false, nil
		}
		next := current
		next.TargetGlobalSID = 0
		next.State = RouteActive
		next.Epoch = current.Epoch + 1
		next.UpdatedAtUnix = now.Unix()
		result = next
		return next, true, nil
	})
	if err != nil {
		return RouteBinding{}, err
	}
	s.report.Accepted("abort_migration")
	return result, nil
}
