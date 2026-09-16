package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
)

// RunStore holds runs. Versioned, so there is no unconditional write — the
// service this replaces incremented a version field and then wrote over
// whatever was there.
type RunStore = versionstore.Store[string, Run]

// ClaimStore holds the per-owner exclusive claim. Insert-only use; see Claim.
type ClaimStore = versionstore.Store[int64, Claim]

// RequestLedger maps an enter idempotency key to the run it produced, so a
// retried enter returns that run instead of allocating another.
type RequestLedger = versionstore.Store[string, LedgerEntry]

// LedgerEntry is what a request ledger entry holds.
type LedgerEntry struct {
	RequestID     string `json:"request_id"`
	OwnerID       int64  `json:"owner_id"`
	RunID         string `json:"run_id"`
	CreatedAtUnix int64  `json:"created_at_unix"`
}

// Releaser hands a run's external resources back.
//
// It is called at most once per resource, and the run records which resources
// have been released, so a retried release is a no-op rather than a second
// free. That record is what replaces the hand-written unwind path per failure
// branch — one of which the service this replaces simply omitted, leaking a
// scene and a replica together.
//
// A Releaser that fails is retried: the resource stays unreleased on the run,
// so a sweep or a later release picks it up. It must therefore tolerate being
// asked to release something it has already freed.
type Releaser interface {
	Release(ctx context.Context, run Run, resource Resource) error
}

// ReleaserFunc adapts a function to Releaser.
type ReleaserFunc func(context.Context, Run, Resource) error

// Release implements Releaser.
func (f ReleaserFunc) Release(ctx context.Context, run Run, resource Resource) error {
	return f(ctx, run, resource)
}

// Config wires a Service.
type Config struct {
	// Runs, Claims and Requests hold the durable state.
	Runs     RunStore
	Claims   ClaimStore
	Requests RequestLedger

	// Release hands resources back. Required: a run that allocates external
	// resources and cannot release them is the leak this package exists to
	// prevent, and a nil default would make that the out-of-the-box
	// behaviour.
	Release Releaser

	// TTL is how long a run may stay open. Zero selects DefaultTTL; it must
	// be positive, because a run with no deadline holds its resources
	// forever.
	TTL time.Duration

	// NewRunID mints run ids; nil means a 128-bit random id. They must not be
	// sequential or guessable: the service this replaces used a per-process
	// counter, so ids collided across replicas and a caller could name
	// another owner's run.
	NewRunID func() (string, error)

	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// The package this is extracted from had no logging and no metrics at
	// all, which is why an unbounded scene allocation and a permanently
	// leaked replica were both undiscoverable in production.
	Metrics servicemetrics.Reporter
}

// DefaultTTL is how long a run may stay open.
const DefaultTTL = 30 * time.Minute

// Service owns run lifecycles.
type Service struct {
	cfg    Config
	report servicemetrics.Sink
}

// New validates the configuration and returns a Service.
func New(cfg Config) (*Service, error) {
	missing := []string{}
	if cfg.Runs == nil {
		missing = append(missing, "run store")
	}
	if cfg.Claims == nil {
		missing = append(missing, "claim store")
	}
	if cfg.Requests == nil {
		missing = append(missing, "request ledger")
	}
	if cfg.Release == nil {
		missing = append(missing, "releaser")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("session: incomplete configuration: %s", strings.Join(missing, ", "))
	}
	if cfg.TTL < 0 {
		return nil, fmt.Errorf("session: ttl must not be negative")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.NewRunID == nil {
		cfg.NewRunID = randomID
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("session: mint run id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// EnterRequest describes one run to open.
//
// It carries no owner field: the owner is a parameter of Enter. The service
// this replaces read the owner out of a request struct on every entry point.
type EnterRequest struct {
	// Kind is what is being entered.
	Kind string
	// RequestID is the idempotency key. REQUIRED — this is the fix. The
	// service this replaces built its key from (owner, kind, requestID) and
	// returned an empty key when the request id was empty, and an empty key
	// made the lookup miss rather than fail. So idempotency failed OPEN: a
	// caller that omitted the id could enter repeatedly, and each entry
	// allocated a new run and new resources.
	RequestID string
	// Context is opaque per-run data, bounded by MaxContextEntries.
	Context map[string]string
}

func (r EnterRequest) validate() error {
	if strings.TrimSpace(r.Kind) == "" {
		return fmt.Errorf("%w: kind is empty", ErrRunInvalid)
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("%w: an idempotency key is required; without one a retry "+
			"allocates a second run", ErrRequestInvalid)
	}
	if len(r.Context) > MaxContextEntries {
		return fmt.Errorf("%w: context has %d entries, limit %d",
			ErrRunInvalid, len(r.Context), MaxContextEntries)
	}
	return nil
}

// Enter opens a run for an owner.
//
// The order is deliberate, and the reason for one step's position is a bug
// this function had until a concurrency test found it:
//
//  1. Consult the ledger. A retried enter returns the run it already produced,
//     without minting an id or taking a claim.
//  2. Create the run, insert-only. This comes BEFORE the claim. Taking the
//     claim first leaves a window in which the claim names a run that does not
//     exist yet — and a concurrent entrant that finds a claim pointing at a
//     missing run correctly concludes the claim is orphaned, clears it, and
//     proceeds. Twelve racing enters then produce twelve runs, which is
//     precisely the defect this package exists to remove.
//  3. Take the per-owner claim, insert-only. An owner that already holds a
//     live run loses here, atomically — including when it races itself, which
//     a same-owner-idempotent reservation would let through.
//  4. Record the ledger entry.
//
// A failure at step 3 deletes the run from step 2, which is safe because a run
// that has not been handed to the caller holds no resources yet.
//
// Because there is exactly one claim per owner, a run that leaks is
// discoverable: the owner cannot enter again and the claim names the run. In
// the service this replaces nothing indexed a run by its owner, so a leaked
// run was unreachable — it could never be left and its scene was never
// destroyed.
func (s *Service) Enter(ctx context.Context, ownerID int64, req EnterRequest) (Run, error) {
	if ownerID <= 0 {
		return Run{}, fmt.Errorf("%w: owner id must be positive", ErrRequestInvalid)
	}
	if err := req.validate(); err != nil {
		return Run{}, err
	}
	now := s.cfg.Now()
	nowUnix := now.Unix()

	if existing, found, err := s.cfg.Requests.Get(ctx, req.RequestID); err != nil {
		return Run{}, err
	} else if found {
		if existing.Value.OwnerID != ownerID {
			// One idempotency key, two owners. Answering either one is
			// answering the wrong caller.
			s.report.Refused("enter", "request_owner_mismatch")
			return Run{}, fmt.Errorf("%w: request %q belongs to owner %d",
				ErrRequestInvalid, req.RequestID, existing.Value.OwnerID)
		}
		run, ok, err := s.Get(ctx, ownerID, existing.Value.RunID)
		if err != nil {
			return Run{}, err
		}
		if !ok {
			return Run{}, fmt.Errorf("%w: request %q names missing run %s",
				ErrConflict, req.RequestID, existing.Value.RunID)
		}
		s.report.Replayed("enter")
		return run, nil
	}

	runID, err := s.cfg.NewRunID()
	if err != nil {
		return Run{}, err
	}
	run := Run{
		ID: runID, OwnerID: ownerID, Kind: req.Kind, RequestID: req.RequestID,
		State: StateOpen, Context: cloneContext(req.Context),
		StartedAtUnix: nowUnix,
		DeadlineUnix:  now.Add(s.cfg.TTL).Unix(),
		UpdatedAtUnix: nowUnix,
	}
	if err := run.Validate(); err != nil {
		return Run{}, err
	}
	stored, created, err := s.cfg.Runs.Create(ctx, runID, run)
	if err != nil {
		return Run{}, err
	}
	if !created {
		s.report.Conflict("enter")
		return Run{}, fmt.Errorf("%w: run id %s is already in use", ErrConflict, runID)
	}

	// Insert-only. This is what makes "one live run per owner" hold under an
	// owner racing itself.
	_, claimed, err := s.cfg.Claims.Create(ctx, ownerID, Claim{
		OwnerID: ownerID, RunID: runID, CreatedAtUnix: nowUnix,
	})
	if err != nil {
		return Run{}, errors.Join(err, s.discard(ctx, stored))
	}
	if !claimed {
		// Someone holds the claim. Read it — Create does NOT hand back the
		// value it collided with, and treating its zero value as the held
		// claim is a run id of "", which then looks like a claim pointing at
		// a missing run. That misreading made every racing entrant "resolve"
		// a perfectly live claim and take it, which is how this function
		// produced twelve runs for twelve racing enters.
		held, found, err := s.cfg.Claims.Get(ctx, ownerID)
		if err != nil {
			return Run{}, errors.Join(err, s.discard(ctx, stored))
		}
		if !found {
			// It was released between the Create and this read. Retaking is
			// handled by the retry below.
			held.Value = Claim{OwnerID: ownerID}
		}
		heldRun, resolved, err := s.resolveClaim(ctx, ownerID, held.Value, nowUnix)
		if err != nil {
			return Run{}, errors.Join(err, s.discard(ctx, stored))
		}
		if !resolved {
			s.report.Refused("enter", "already_running")
			return Run{}, errors.Join(
				fmt.Errorf("%w: owner %d holds run %s", ErrAlreadyRunning, ownerID, heldRun.ID),
				s.discard(ctx, stored))
		}
		// The stale claim is gone; retake it.
		_, claimed, err = s.cfg.Claims.Create(ctx, ownerID, Claim{
			OwnerID: ownerID, RunID: runID, CreatedAtUnix: nowUnix,
		})
		if err != nil {
			return Run{}, errors.Join(err, s.discard(ctx, stored))
		}
		if !claimed {
			// Another entrant took it in between. One live run per owner,
			// so this one loses.
			s.report.Refused("enter", "already_running")
			return Run{}, errors.Join(
				fmt.Errorf("%w: owner %d", ErrAlreadyRunning, ownerID),
				s.discard(ctx, stored))
		}
	}

	// The ledger write is the RequestID's only serialization point. The claim
	// serializes "one live run per owner"; it cannot see another owner using
	// the same RequestID, and two such entrants both read the ledger as absent
	// before either wrote it. Update's compare-and-set inserts when the key is
	// still absent and otherwise hands back the entry that won, so the loser
	// can undo the run and claim it built for a request that already has an
	// answer. (RR-20260908-01: Create's created=false used to be dropped, and
	// both owners were told they had succeeded.)
	var winner LedgerEntry
	collided := false
	if _, _, err := s.cfg.Requests.Update(ctx, req.RequestID, func(current LedgerEntry, found bool) (LedgerEntry, bool, error) {
		collided = found
		if found {
			winner = current
			return current, false, nil
		}
		return LedgerEntry{RequestID: req.RequestID, OwnerID: ownerID, RunID: runID, CreatedAtUnix: nowUnix}, true, nil
	}); err != nil {
		// The run exists and the claim names it, so it is reachable and
		// releasable; only the replay answer is missing. Reporting the error
		// lets the caller retry, which will find the claim and be refused —
		// which is correct, because the run really is open.
		return run, err
	}
	if collided {
		// This entrant's run was never handed out and holds no resources, so
		// both writes can be undone. Order matters: release the claim while the
		// run it names is still open. Nobody replaces a claim whose run is live
		// (resolveClaim only clears one whose run is gone or lapsed), so the
		// run-id and version checks in releaseClaim cannot hit a newcomer's
		// claim. Discarding the run first opened exactly that window: a
		// same-owner Enter saw the orphaned claim, took a fresh one, and — a
		// deleted-and-recreated key restarts at version 1 — the loser's
		// version-checked delete then removed the newcomer's claim, leaving
		// two open runs for one owner (RR-20260909-02).
		undo := errors.Join(s.releaseClaim(ctx, ownerID, runID), s.discard(ctx, stored))
		if winner.OwnerID != ownerID {
			s.report.Refused("enter", "request_owner_collision")
			return Run{}, errors.Join(fmt.Errorf("%w: request %q belongs to owner %d",
				ErrRequestInvalid, req.RequestID, winner.OwnerID), undo)
		}
		if undo != nil {
			return Run{}, undo
		}
		replay, ok, err := s.Get(ctx, ownerID, winner.RunID)
		if err != nil {
			return Run{}, err
		}
		if !ok {
			return Run{}, fmt.Errorf("%w: request %q names missing run %s", ErrConflict, req.RequestID, winner.RunID)
		}
		s.report.Replayed("enter")
		return replay, nil
	}
	s.report.Accepted("enter")
	return run, nil
}

// discard removes a run that was created but never handed to a caller.
//
// Version-checked, so it cannot remove a run that has since been modified. It
// is safe only for a run the caller never saw: such a run holds no resources,
// which is why step 2 of Enter can be undone at all.
func (s *Service) discard(ctx context.Context, stored versionstore.Versioned[Run]) error {
	if err := s.cfg.Runs.Delete(ctx, stored.Value.ID, stored); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			return nil
		}
		return fmt.Errorf("session: discard unclaimed run %s: %w", stored.Value.ID, err)
	}
	return nil
}

// resolveClaim decides whether a held claim is stale, and clears it if so.
//
// A claim is stale when the run it names is terminal, expired, or gone. This
// is what keeps a crashed run from burning an owner's ability to enter — the
// same reason a directory reservation carries a ttl.
func (s *Service) resolveClaim(ctx context.Context, ownerID int64, claim Claim, nowUnix int64) (Run, bool, error) {
	if strings.TrimSpace(claim.RunID) == "" {
		// A stored claim always names a run, so this is a corrupt record or a
		// misread — never an orphan. Refusing loudly matters because the
		// alternative reading, "orphaned, so clear it", clears a LIVE claim:
		// releaseClaim skips its ownership check when it is given no run id.
		// That is how this function once produced twelve runs for twelve
		// racing enters.
		return Run{}, false, fmt.Errorf("%w: owner %d holds a claim with no run id",
			ErrConflict, claim.OwnerID)
	}
	stored, found, err := s.cfg.Runs.Get(ctx, claim.RunID)
	if err != nil {
		return Run{}, false, err
	}
	if !found {
		// The claim names a run that is not there. Clearing it is safe and is
		// the only way the owner can ever enter again.
		s.report.Dropped("claim.orphaned", 1)
		return Run{}, true, s.releaseClaim(ctx, ownerID, claim.RunID)
	}
	run := stored.Value
	if run.Live(nowUnix) {
		return run, false, nil
	}
	// Terminal or lapsed. Resolve it properly — releasing its resources —
	// before freeing the claim, so the claim is never the thing that outlives
	// the cleanup.
	resolved, err := s.resolve(ctx, run, StateExpired, "deadline elapsed", nowUnix)
	if err != nil {
		return run, false, err
	}
	return resolved, true, s.releaseClaim(ctx, ownerID, claim.RunID)
}

// releaseClaim frees an owner's claim, version-checked so it cannot remove a
// claim another entrant has since taken.
func (s *Service) releaseClaim(ctx context.Context, ownerID int64, expectRunID string) error {
	stored, found, err := s.cfg.Claims.Get(ctx, ownerID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if expectRunID != "" && stored.Value.RunID != expectRunID {
		// Not ours any more. Deleting here would remove the new entrant's
		// claim — the release-races-a-re-acquire bug.
		s.report.Dropped("claim.release_not_ours", 1)
		return nil
	}
	if err := s.cfg.Claims.Delete(ctx, ownerID, stored); err != nil {
		if errors.Is(err, versionstore.ErrVersionMismatch) {
			// Someone else changed it, which means ours was already gone.
			return nil
		}
		return fmt.Errorf("session: release claim for owner %d: %w", ownerID, err)
	}
	return nil
}

// Attach records an external resource on a run.
//
// It is ONE compare-and-set on the run, which is the point. The service this
// replaces allocated a scene, then created a replica, then attached it, with a
// separate compensation path per failure — and the second one was missing, so
// a failed attach leaked both. Here a run that fails to attach is released by
// the same path that releases any other run, and there is no second branch to
// forget.
func (s *Service) Attach(ctx context.Context, ownerID int64, runID string, resource Resource) (Run, error) {
	if err := resource.Validate(); err != nil {
		return Run{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	var (
		result  Run
		refusal string
	)
	_, _, err := s.cfg.Runs.Update(ctx, runID, func(current Run, found bool) (Run, bool, error) {
		refusal = ""
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrRunMissing, runID)
		}
		if current.OwnerID != ownerID {
			refusal = "not_owner"
			return current, false, fmt.Errorf("%w: run %s", ErrNotOwner, runID)
		}
		if current.State.Terminal() {
			refusal = "terminal"
			return current, false, fmt.Errorf("%w: run %s is %s", ErrRunTerminal, runID, current.State)
		}
		if current.Expired(nowUnix) {
			// Attaching a resource to a run that has run out of time is how
			// a resource gets allocated with nothing left to release it.
			refusal = "expired"
			return current, false, fmt.Errorf("%w: run %s at %d", ErrRunExpired, runID, current.DeadlineUnix)
		}
		if existing, ok := current.Attached(resource.Kind); ok {
			if existing.ID == resource.ID {
				// Idempotent: a retried attach of the same resource is a
				// no-op, so a caller retry after a lost response does not
				// allocate a second one.
				result = current.clone()
				return current, false, nil
			}
			refusal = "already_attached"
			return current, false, fmt.Errorf("%w: run %s already holds %s %s",
				ErrAlreadyAttached, runID, existing.Kind, existing.ID)
		}
		if len(current.Resources) >= MaxResourceEntries {
			refusal = "resource_limit"
			return current, false, fmt.Errorf("%w: run %s holds %d resources, limit %d",
				ErrRunInvalid, runID, len(current.Resources), MaxResourceEntries)
		}
		next := current.clone()
		next.Resources = append(next.Resources, resource)
		next.UpdatedAtUnix = nowUnix
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		if refusal != "" {
			s.report.Refused("attach", refusal)
		}
		return Run{}, err
	}
	s.report.Accepted("attach")
	return result, nil
}

// Finish resolves a run to a terminal state chosen by the caller and releases
// its resources.
//
// The outcome is the caller's decision — this package does not evaluate
// success rules — but the state transition and the release are this package's,
// which is why they cannot be forgotten.
func (s *Service) Finish(ctx context.Context, ownerID int64, runID string, state State, outcome string) (Run, error) {
	switch state {
	case StateSucceeded, StateFailed, StateAbandoned:
	default:
		return Run{}, fmt.Errorf("%w: %q is not an outcome a caller may choose", ErrRunInvalid, state)
	}
	return s.finish(ctx, ownerID, runID, state, outcome)
}

// Leave abandons a run. It is Finish with the abandoned outcome, named
// separately because "the owner left" is the common case and deserves to read
// as one call.
func (s *Service) Leave(ctx context.Context, ownerID int64, runID string, reason string) (Run, error) {
	return s.finish(ctx, ownerID, runID, StateAbandoned, reason)
}

func (s *Service) finish(ctx context.Context, ownerID int64, runID string, state State, outcome string) (Run, error) {
	nowUnix := s.cfg.Now().Unix()
	stored, found, err := s.cfg.Runs.Get(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if !found {
		return Run{}, fmt.Errorf("%w: %s", ErrRunMissing, runID)
	}
	if stored.Value.OwnerID != ownerID {
		// Ownership is checked on EVERY operation. The service this replaces
		// checked it on one of four.
		s.report.Refused("finish", "not_owner")
		return Run{}, fmt.Errorf("%w: run %s", ErrNotOwner, runID)
	}
	if stored.Value.State.Terminal() {
		// Idempotent: a retried finish returns the run as it stands and does
		// not move its finish time, or "when did this run end" becomes
		// whenever it was last retried.
		s.report.Replayed("finish")
		return s.releasePending(ctx, stored.Value, nowUnix)
	}
	// An open run whose deadline has passed becomes expired, not whatever the
	// caller asked for: the run was over before the call arrived, and
	// recording the caller's outcome would credit a result it did not earn.
	target, resolvedOutcome := state, outcome
	if stored.Value.Expired(nowUnix) {
		target, resolvedOutcome = StateExpired, "deadline elapsed"
	}
	run, resolveErr := s.resolve(ctx, stored.Value, target, resolvedOutcome, nowUnix)
	if resolveErr != nil {
		// The run IS resolved; what failed is releasing a resource. Returning
		// the run alongside the error is what lets a caller see the terminal
		// state and the still-pending resources, rather than a zero value
		// that claims to be a finished run.
		return run, resolveErr
	}
	if err := s.releaseClaim(ctx, ownerID, runID); err != nil {
		return run, err
	}
	if target == StateExpired {
		s.report.Refused("finish", "expired")
		return run, fmt.Errorf("%w: run %s at %d", ErrRunExpired, runID, stored.Value.DeadlineUnix)
	}
	s.report.Accepted("finish")
	return run, nil
}

// resolve moves a run to a terminal state and releases its resources.
//
// The state moves first and the release follows, so a release that fails
// leaves a terminal run with pending resources — retryable, and visible — and
// never an open run whose resources are gone.
func (s *Service) resolve(ctx context.Context, run Run, state State, outcome string, nowUnix int64) (Run, error) {
	var result Run
	_, _, err := s.cfg.Runs.Update(ctx, run.ID, func(current Run, found bool) (Run, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrRunMissing, run.ID)
		}
		if current.State.Terminal() {
			result = current.clone()
			return current, false, nil
		}
		next := current.clone()
		next.State = state
		next.Outcome = outcome
		next.FinishedAtUnix = nowUnix
		next.UpdatedAtUnix = nowUnix
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		return Run{}, err
	}
	return s.releasePending(ctx, result, nowUnix)
}

// releasePending hands back every resource the run still holds, marking each
// one released as it goes.
//
// Each resource is marked released in its own compare-and-set, so a failure
// part-way through leaves the ones already freed marked and the rest pending.
// A retry then releases only what is left, which is what makes "release
// exactly once" hold across retries rather than only on the happy path.
func (s *Service) releasePending(ctx context.Context, run Run, nowUnix int64) (Run, error) {
	pending := run.Pending()
	if len(pending) == 0 {
		return run, nil
	}
	current := run
	var failures error
	for _, resource := range pending {
		if err := s.cfg.Release.Release(ctx, current, resource); err != nil {
			// Left pending on purpose: a resource this package could not free
			// has to stay on the run so a sweep or a later call retries it.
			// Dropping it here is how the service this replaces leaked a
			// replica permanently.
			s.report.Dropped("resource.release_failed", 1)
			failures = errors.Join(failures, fmt.Errorf("release %s %s: %w", resource.Kind, resource.ID, err))
			continue
		}
		marked, err := s.markReleased(ctx, run.ID, resource, nowUnix)
		if err != nil {
			return current, errors.Join(failures, err)
		}
		current = marked
		s.report.Accepted("release")
	}
	return current, failures
}

func (s *Service) markReleased(ctx context.Context, runID string, resource Resource, nowUnix int64) (Run, error) {
	var result Run
	_, _, err := s.cfg.Runs.Update(ctx, runID, func(current Run, found bool) (Run, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrRunMissing, runID)
		}
		next := current.clone()
		changed := false
		for index, held := range next.Resources {
			if held.Kind == resource.Kind && held.ID == resource.ID && !held.Released() {
				next.Resources[index].ReleasedAtUnix = nowUnix
				changed = true
				break
			}
		}
		if !changed {
			result = current.clone()
			return current, false, nil
		}
		next.UpdatedAtUnix = nowUnix
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		return Run{}, err
	}
	return result, nil
}

// Get reads a run, checking ownership.
//
// An elapsed deadline reads as expired even before anything sweeps, so a
// caller never sees a run that is only "open" because nothing has gotten
// round to resolving it. The service this replaces computed a deadline,
// stored it, sent it to clients, and never compared it to a clock anywhere.
//
// The returned run reflects the resolution; the stored one is resolved by
// Sweep. A reader being right before the sweep runs is the difference between
// a deadline that means something and a deadline that is a decoration.
func (s *Service) Get(ctx context.Context, ownerID int64, runID string) (Run, bool, error) {
	stored, found, err := s.cfg.Runs.Get(ctx, runID)
	if err != nil || !found {
		return Run{}, false, err
	}
	run := stored.Value
	if run.OwnerID != ownerID {
		// Reporting "not found" rather than "not yours" would be a small
		// kindness to an enumerator. Reporting the refusal is the honest
		// answer and the run ids are unguessable anyway.
		s.report.Refused("get", "not_owner")
		return Run{}, false, fmt.Errorf("%w: run %s", ErrNotOwner, runID)
	}
	if run.Expired(s.cfg.Now().Unix()) {
		run.State = StateExpired
		run.Outcome = "deadline elapsed"
	}
	return run.clone(), true, nil
}

// Current returns an owner's live run, if any.
//
// This is the index whose absence made a leaked run unreachable: Leave needs a
// run id, a client only holds the last one it was handed, and nothing in the
// service this replaces mapped an owner back to its runs.
func (s *Service) Current(ctx context.Context, ownerID int64) (Run, bool, error) {
	claim, found, err := s.cfg.Claims.Get(ctx, ownerID)
	if err != nil || !found {
		return Run{}, false, err
	}
	run, ok, err := s.Get(ctx, ownerID, claim.Value.RunID)
	if err != nil || !ok {
		return Run{}, false, err
	}
	return run, true, nil
}

// Sweep resolves expired runs, releasing their resources and freeing their
// owners' claims, up to limit.
//
// Something has to make the deadline durable rather than only computed on
// read. In the service this replaces there was no sweep, no ticker and no
// goroutine — the deadline was written and forwarded and never acted on.
func (s *Service) Sweep(ctx context.Context, ownerIDs []int64, limit int) ([]Run, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive, got %d", ErrRangeInvalid, limit)
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	nowUnix := s.cfg.Now().Unix()

	// Owners are swept in a deterministic order, so a sweep that is cut short
	// by the limit makes progress on the same prefix rather than a random one
	// — a random prefix means a run at the back may never be reached.
	owners := append([]int64(nil), ownerIDs...)
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	resolved := make([]Run, 0, limit)
	var failures error
	for _, ownerID := range owners {
		if len(resolved) >= limit {
			break
		}
		claim, found, err := s.cfg.Claims.Get(ctx, ownerID)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if !found {
			continue
		}
		stored, found, err := s.cfg.Runs.Get(ctx, claim.Value.RunID)
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if !found {
			s.report.Dropped("claim.orphaned", 1)
			if err := s.releaseClaim(ctx, ownerID, claim.Value.RunID); err != nil {
				failures = errors.Join(failures, err)
			}
			continue
		}
		run := stored.Value
		if run.Live(nowUnix) {
			continue
		}
		// A run that is already terminal needs its resources released and its
		// claim freed, not a state transition. Passing its own state back
		// through resolve would compute arguments for a branch that discards
		// them — resolve refuses to rewrite a terminal run — which reads as if
		// the outcome were being preserved here rather than there.
		var swept Run
		if run.State.Terminal() {
			swept, err = s.releasePending(ctx, run, nowUnix)
		} else {
			swept, err = s.resolve(ctx, run, StateExpired, "deadline elapsed", nowUnix)
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if err := s.releaseClaim(ctx, ownerID, claim.Value.RunID); err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if swept.State == StateExpired {
			s.report.Dropped("run.expired", 1)
		}
		resolved = append(resolved, swept)
	}
	s.report.Depth("session.swept", int64(len(resolved)))
	return resolved, failures
}

func cloneContext(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
