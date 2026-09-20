package global

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
)

// Config wires a Service.
type Config struct {
	// Routes and Leases hold the durable state. Both are versioned stores,
	// so there is no write path that skips the comparison — the property the
	// boundary document asked for and four hand-written stores did not keep.
	Routes versionstore.Store[int32, RouteBinding]
	Leases versionstore.Store[int32, GameLease]

	// LeaseTTL is how long a lease survives without a heartbeat; zero selects
	// DefaultLeaseTTL. It must be positive: a lease that never expires means
	// a dead game server stays "alive" in its coordination group forever.
	LeaseTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// NewIncarnation mints lease incarnation tokens; nil means a 128-bit
	// random token. They must be unguessable: a caller that can guess the
	// current token can renew — or effectively steal — another process's
	// lease.
	NewIncarnation func() (string, error)
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// A stale epoch and a wrong incarnation are the two refusals this package
	// exists to make. Both were unobservable in the implementation it
	// replaces — the epoch was guarded by an in-process lock, and the
	// incarnation check did not exist — so a late heartbeat from a dead
	// process overwriting a live lease produced no error, no log and no
	// counter. Counting them is how an operator sees it happening at all.
	Metrics servicemetrics.Reporter
}

// DefaultLeaseTTL is how long a lease lasts without a heartbeat.
const DefaultLeaseTTL = 30 * time.Second

// Service coordinates routing and liveness for a set of game servers.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

func New(cfg Config) (*Service, error) {
	if cfg.Routes == nil {
		return nil, fmt.Errorf("global: route store is required")
	}
	if cfg.Leases == nil {
		return nil, fmt.Errorf("global: lease store is required")
	}
	if cfg.LeaseTTL < 0 {
		return nil, fmt.Errorf("global: lease ttl must not be negative")
	}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewIncarnation == nil {
		cfg.NewIncarnation = randomIncarnation
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

func randomIncarnation() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("global: mint incarnation: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// --- route binding ---

// Bind creates the first binding for a game server. It is insert-only: a
// game server that already has a binding must be rebound through Rebind,
// which requires the current epoch.
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

// --- game lease ---

// AcquireLease takes the lease for a game server and returns the incarnation
// token that authorizes renewals.
//
// It succeeds when there is no lease or when the existing lease has lapsed
// or was released. There is no "re-take by the same incarnation": the caller
// presents no token here, so a live lease is refused whoever asks — a holder
// that wants to keep its lease renews it. It refuses to displace a live lease: two processes claiming to be the
// same game server is a deployment fault, and silently letting the second one
// win is how a lease's start time and load become fiction.
func (s *Service) AcquireLease(ctx context.Context, gameSID int32) (GameLease, error) {
	binding, err := s.Resolve(ctx, gameSID)
	if err != nil {
		return GameLease{}, err
	}
	now := s.cfg.Now()

	// The token is minted only once the lease is known to be takeable, and
	// only once across retries. Minting before the check spends a token on
	// every refused acquire, which is free for crypto/rand and a hot path for
	// a rate-limited or remote minter; minting inside the callback would spend
	// one per compare-and-set retry, because Update may call it more than
	// once.
	incarnation := ""
	mintIncarnation := func() (string, error) {
		if incarnation != "" {
			return incarnation, nil
		}
		minted, err := s.cfg.NewIncarnation()
		if err != nil {
			return "", err
		}
		incarnation = minted
		return minted, nil
	}

	var result GameLease
	_, _, err = s.cfg.Leases.Update(ctx, gameSID, func(current GameLease, found bool) (GameLease, bool, error) {
		if found && current.State == LeaseActive && !current.Expired(now.Unix()) {
			s.report.Refused("acquire_lease", "held")
			return current, false, fmt.Errorf("%w: game %d lease is held until %d",
				ErrConflict, gameSID, current.ExpiresAtUnix)
		}
		incarnation, err := mintIncarnation()
		if err != nil {
			return current, false, err
		}
		next := GameLease{
			GameSID: gameSID, Incarnation: incarnation,
			GlobalGroupID: binding.GlobalGroupID, GlobalSID: binding.GlobalSID,
			RouteEpoch: binding.Epoch, State: LeaseActive,
			StartedAtUnix: now.Unix(), LastHeartbeatAtUnix: now.Unix(),
			ExpiresAtUnix: now.Add(s.cfg.LeaseTTL).Unix(),
		}
		result = next
		return next, true, nil
	})
	if err != nil {
		return GameLease{}, err
	}
	s.report.Accepted("acquire_lease")
	return result, nil
}

// RenewLease extends a lease the caller holds and records its load snapshot.
//
// The incarnation token is required, and this is the whole point: a heartbeat
// from a process that is no longer the holder is refused. The implementation
// this replaces read the record, computed a next version from it and wrote
// unconditionally, so a late heartbeat from a previous incarnation overwrote
// the live lease with its own start time and load — and the version field it
// maintained never gated anything.
func (s *Service) RenewLease(ctx context.Context, gameSID int32, incarnation string, load map[string]string) (GameLease, error) {
	if incarnation == "" {
		return GameLease{}, fmt.Errorf("%w: incarnation is required", ErrLeaseInvalid)
	}
	if err := validateLoad(load); err != nil {
		return GameLease{}, err
	}
	now := s.cfg.Now()

	var result GameLease
	_, _, err := s.cfg.Leases.Update(ctx, gameSID, func(current GameLease, found bool) (GameLease, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: game %d", ErrLeaseMissing, gameSID)
		}
		if current.Incarnation != incarnation {
			// Deliberately does not reveal the current incarnation: a caller
			// that does not hold the lease must not learn the token that
			// would let it renew.
			s.report.Refused("renew_lease", "not_holder")
			return current, false, fmt.Errorf("%w: game %d", ErrLeaseNotHolder, gameSID)
		}
		if current.State != LeaseActive {
			s.report.Refused("renew_lease", "released")
			return current, false, fmt.Errorf("%w: game %d lease was released", ErrLeaseExpired, gameSID)
		}
		if current.Expired(now.Unix()) {
			// An expired lease is not renewable: the holder must re-acquire,
			// which mints a new incarnation and a new start time. Extending
			// it instead would hide that the game server was gone.
			s.report.Refused("renew_lease", "lapsed")
			return current, false, fmt.Errorf("%w: game %d lapsed at %d", ErrLeaseExpired, gameSID, current.ExpiresAtUnix)
		}
		next := current
		next.LastHeartbeatAtUnix = now.Unix()
		next.ExpiresAtUnix = now.Add(s.cfg.LeaseTTL).Unix()
		next.Load = cloneLoad(load)
		result = next
		return next, true, nil
	})
	if err != nil {
		return GameLease{}, err
	}
	s.report.Accepted("renew_lease")
	return result, nil
}

// ReleaseLease gives up a lease the caller holds.
func (s *Service) ReleaseLease(ctx context.Context, gameSID int32, incarnation string) error {
	if incarnation == "" {
		return fmt.Errorf("%w: incarnation is required", ErrLeaseInvalid)
	}
	now := s.cfg.Now()
	var replayed, notOurs bool
	_, _, err := s.cfg.Leases.Update(ctx, gameSID, func(current GameLease, found bool) (GameLease, bool, error) {
		if !found {
			// Idempotent: a retried release must not fail.
			replayed = true
			return current, false, nil
		}
		if current.Incarnation != incarnation {
			// Not ours. Returning nil here would be the release-races-a-
			// re-acquire bug: dropping the new holder's lease. It is still a
			// release that did nothing while answering success, so it is
			// counted rather than merely commented.
			notOurs = true
			return current, false, nil
		}
		if current.State == LeaseReleased {
			replayed = true
			return current, false, nil
		}
		next := current
		next.State = LeaseReleased
		next.ExpiresAtUnix = 0
		next.LastHeartbeatAtUnix = now.Unix()
		return next, true, nil
	})
	if err != nil {
		return err
	}
	switch {
	case notOurs:
		s.report.Dropped("release_lease.not_ours", 1)
	case replayed:
		s.report.Replayed("release_lease")
	default:
		s.report.Accepted("release_lease")
	}
	return nil
}

// Lease reads a lease. An elapsed deadline reads as expired even before
// anything sweeps, so a caller never sees a lease that is only "active"
// because nothing has gotten round to resolving it.
func (s *Service) Lease(ctx context.Context, gameSID int32) (GameLease, bool, error) {
	current, found, err := s.cfg.Leases.Get(ctx, gameSID)
	if err != nil || !found {
		return GameLease{}, false, err
	}
	lease := current.Value
	if lease.Expired(s.cfg.Now().Unix()) {
		// Reported as lapsed rather than released: the deadline it missed is
		// left in place, so a caller can see when it stopped answering.
		lease.State = LeaseLapsed
	}
	return lease, true, nil
}

// LiveGames returns the game servers in a group whose leases are live, up to
// limit. Bounded, and the bound cannot be bypassed with a zero limit.
func (s *Service) LiveGames(ctx context.Context, groupID string, candidates []int32, limit int) ([]GameLease, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive, got %d", ErrRangeInvalid, limit)
	}
	if limit > MaxPageSize {
		return nil, fmt.Errorf("%w: limit %d exceeds %d", ErrRangeInvalid, limit, MaxPageSize)
	}
	if len(candidates) > MaxPageSize {
		return nil, fmt.Errorf("%w: %d candidates exceeds %d", ErrRangeInvalid, len(candidates), MaxPageSize)
	}
	nowUnix := s.cfg.Now().Unix()
	out := make([]GameLease, 0, limit)
	for _, gameSID := range candidates {
		current, found, err := s.cfg.Leases.Get(ctx, gameSID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		lease := current.Value
		if lease.GlobalGroupID != groupID || lease.State != LeaseActive || lease.Expired(nowUnix) {
			continue
		}
		out = append(out, lease)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}
