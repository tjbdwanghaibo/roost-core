package activity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/versionstore"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Config wires an Service.
//
// Six stores rather than one, because they are six key spaces with different
// lifetimes and different bounds: an activity outlives its notifications, the
// ledger is bounded by time, the audit log is bounded by count, and a dispatch
// is deleted by nobody. All six are versionstore.Store, so there is no write
// path in this service that skips the version comparison. That is not a
// convention here — the contract has no unconditional write, so the
// implementation that read-then-wrote in the code this replaces could not be
// written against it at all.
type Config struct {
	// Activities holds the aggregation records.
	Activities versionstore.Store[Key, Activity]
	// Participants holds per-participant progress.
	Participants versionstore.Store[ParticipantKey, Participant]
	// Ledger holds the insert-only progress reservations. Wire it with a TTL
	// matching ReservationTTL; see ProgressReservation for why the bound is
	// time and what that costs.
	Ledger versionstore.Store[RequestKey, ProgressReservation]
	// Audits holds the append-only refusal log, one record per activity.
	Audits versionstore.Store[Key, NotifyAuditLog]
	// Dispatches holds result deliveries, one per (activity, game).
	Dispatches versionstore.Store[DispatchKey, Dispatch]
	// Windows holds the per-group index of unfinished activities that
	// AdvanceExpired scans.
	Windows versionstore.Store[string, Window]

	// GraceWindow is how long after the FIRST notify the aggregation waits for
	// the rest of the expected games. Zero selects DefaultGraceWindow; it must
	// be positive, because a zero window would complete an aggregation on the
	// first notify and a negative one would complete it before it started.
	//
	// It is measured from the first notify and not from any schedule this
	// service holds, which is the whole of "global 不按自身 CloseAt 主动驱动
	// 活动时间线": the timeline belongs to the games.
	GraceWindow time.Duration
	// ReservationTTL is how long a ledger entry answers for its request id. It
	// must exceed the longest client retry horizon — past it, a replay is
	// indistinguishable from a new request. See ProgressReservation.
	ReservationTTL time.Duration
	// DispatchBackoff is the base delay between dispatch attempts; the delay
	// grows with the attempt count so a game that is down is not hammered.
	DispatchBackoff time.Duration
	// DispatchMaxAttempts bounds delivery attempts per dispatch. Zero selects
	// DefaultDispatchAttempts. It must be positive: an unbounded retry queue
	// is a queue that never drains and never reports that it is not draining.
	DispatchMaxAttempts int

	// OpeningGrace is how long a window entry may sit unconfirmed — admitted
	// by OpenActivity, Activities.Create not yet observed — before a sweep
	// reclaims its slot. It covers the opener crashing between the two
	// writes; a slow opener that confirms after it still lands in the window
	// (confirm re-adds). Zero selects DefaultOpeningGrace.
	OpeningGrace time.Duration

	// Now is the clock; nil means time.Now. Every deadline, grace window and
	// backoff in this file reads it, and none of them calls time.Now inline —
	// a service whose expiry cannot be moved by a test has no test for
	// expiry.
	Now func() time.Time
	// NewDispatchToken mints ACK tokens; nil means a 128-bit random token.
	// They must be unguessable because the token is what authorizes the ACK:
	// a guessable one lets any caller mark a delivery processed that never
	// arrived.
	NewDispatchToken func() (string, error)
	// SweepGroups names the groups whose expired activities THIS process
	// advances in the background (activity.sweep_groups). Enumerating groups
	// would be an unbounded keyspace scan, so a deployment supplies them; an
	// empty list means no back-stop runs here, and the Server says so once at
	// start. Until U-0119 there was no way to supply them at all, so the grace
	// window was never enforced by any process.
	SweepGroups []string

	// Metrics receives reports. A nil reporter means no reporting and never
	// fails an operation.
	//
	// The refusal audit answers "why was this game's notify rejected" for one
	// activity; these counters answer "how often, across all of them". Both
	// are needed: the audit is per-key and bounded, so it cannot show a rate,
	// and an exhausted dispatch — a result a game will now never receive —
	// leaves no audit at all.
	Metrics servicemetrics.Reporter
}

// Defaults.
const (
	DefaultGraceWindow      = 60 * time.Second
	DefaultOpeningGrace     = time.Minute
	DefaultReservationTTL   = 30 * time.Minute
	DefaultDispatchBackoff  = 5 * time.Second
	DefaultDispatchAttempts = 5

	// maxBackoffMultiplier caps the growth of the dispatch retry delay, so a
	// long-lived dispatch does not compute a delay measured in hours.
	maxBackoffMultiplier = 10
)

// Service coordinates cross-server activity aggregation: phase
// notifications from games, participant progress, refusal audit, and result
// dispatch.
//
// It is separate from Service because the two halves share no state — routing
// and leases answer "where does this game belong and is it alive", this
// answers "has every game reached the phase yet" — and joining them would give
// the activity half a reason to reach into the lease store, which is the kind
// of coupling that turns two bounded services into one unbounded one.
type Service struct {
	// deliveryMu / deliveryCursor rotate DeliveringActivities through a
	// group's list across sweeps so a long list is not always served from
	// the same head (U-0192).
	deliveryMu     sync.Mutex
	deliveryCursor map[string]Key

	cfg    Config
	report servicemetrics.Sink
}

func New(cfg Config) (*Service, error) {
	if cfg.Activities == nil {
		return nil, fmt.Errorf("activity: store is required")
	}
	if cfg.Participants == nil {
		return nil, fmt.Errorf("activity: participant store is required")
	}
	if cfg.Ledger == nil {
		return nil, fmt.Errorf("activity: progress ledger store is required")
	}
	if cfg.Audits == nil {
		// Refused notifications must be auditable, so a service configured
		// without the audit store is not a degraded service, it is the defect
		// this design exists to remove.
		return nil, fmt.Errorf("activity: notify audit store is required")
	}
	if cfg.Dispatches == nil {
		return nil, fmt.Errorf("activity: dispatch store is required")
	}
	if cfg.Windows == nil {
		return nil, fmt.Errorf("activity: window store is required")
	}
	if cfg.GraceWindow < 0 {
		return nil, fmt.Errorf("activity: grace window must not be negative")
	}
	if cfg.GraceWindow == 0 {
		cfg.GraceWindow = DefaultGraceWindow
	}
	if cfg.ReservationTTL < 0 {
		return nil, fmt.Errorf("activity: reservation ttl must not be negative")
	}
	if cfg.ReservationTTL == 0 {
		cfg.ReservationTTL = DefaultReservationTTL
	}
	if cfg.DispatchBackoff < 0 {
		return nil, fmt.Errorf("activity: dispatch backoff must not be negative")
	}
	if cfg.DispatchBackoff == 0 {
		cfg.DispatchBackoff = DefaultDispatchBackoff
	}
	if cfg.DispatchMaxAttempts < 0 {
		return nil, fmt.Errorf("activity: dispatch max attempts must not be negative")
	}
	if cfg.DispatchMaxAttempts == 0 {
		cfg.DispatchMaxAttempts = DefaultDispatchAttempts
	}
	if cfg.OpeningGrace < 0 {
		return nil, fmt.Errorf("activity: opening grace must not be negative")
	}
	if cfg.OpeningGrace == 0 {
		cfg.OpeningGrace = DefaultOpeningGrace
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewDispatchToken == nil {
		cfg.NewDispatchToken = randomDispatchToken
	}
	return &Service{cfg: cfg, report: servicemetrics.Wrap(cfg.Metrics)}, nil
}

func randomDispatchToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("activity: mint dispatch token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// --- opening an activity ---

// SweepGroups is the configured set of groups this process back-stops.
func (s *Service) SweepGroups() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.cfg.SweepGroups...)
}

// OpenActivity declares an aggregation: which games are expected to report the
// phase. It is insert-only, like Bind: an activity that is already open cannot
// be reopened with a different expected set, because widening the expected set
// of a collecting aggregation would un-collect it and narrowing it would
// complete it behind the games still working.
//
// The activity opens in StatusPending with no grace deadline. Nothing here
// schedules its advance — the first notify does that.
func (s *Service) OpenActivity(ctx context.Context, key Key, expectedGameSIDs []int32) (Activity, error) {
	if err := key.Validate(); err != nil {
		return Activity{}, err
	}
	if err := validateExpectedGames(expectedGameSIDs); err != nil {
		return Activity{}, err
	}

	// The window entry goes in first, as an opening entry. See Window: an
	// activity with no window entry is never swept at all, while an opening
	// entry with no activity is reclaimed only after OpeningGrace — a sweep
	// running between these two writes must not read "no activity yet" as
	// "dead entry" (RR-20260914-02).
	if err := s.admitToWindow(ctx, key); err != nil {
		return Activity{}, err
	}

	nowUnix := s.cfg.Now().Unix()
	activity := Activity{
		Key:              key,
		ExpectedGameSIDs: cloneSIDs(expectedGameSIDs),
		Status:           StatusPending,
		OpenedAtUnix:     nowUnix,
		UpdatedAtUnix:    nowUnix,
	}
	stored, created, err := s.cfg.Activities.Create(ctx, key, activity)
	if err != nil {
		// The create may or may not have landed; the opening entry stays so
		// the next sweep can tell (activity present → confirm, absent past
		// OpeningGrace → reclaim).
		return Activity{}, err
	}
	// Confirm regardless of who created it: the record exists, so the key
	// belongs in Keys, and a reclaim that raced ahead of us is undone here.
	if err := s.confirmWindow(ctx, key); err != nil {
		return Activity{}, err
	}
	if !created {
		return Activity{}, fmt.Errorf("%w: activity %s", ErrExists, key)
	}
	return stored.Value.clone(), nil
}

// confirmWindow moves a key from Opening to Keys under compare-and-set. It
// is idempotent and unconditional about presence: after Activities.Create
// has been observed the key must be in Keys whether or not a sweep already
// reclaimed its opening entry.
func (s *Service) confirmWindow(ctx context.Context, key Key) error {
	_, _, err := s.cfg.Windows.Update(ctx, key.GroupID, func(current Window, found bool) (Window, bool, error) {
		next := Window{GroupID: key.GroupID}
		if found {
			next = current.clone()
			next.GroupID = key.GroupID
		}
		changed := false
		if i := next.openingIndex(key); i >= 0 {
			next.Opening = append(next.Opening[:i], next.Opening[i+1:]...)
			changed = true
		}
		inKeys := false
		for _, existing := range next.Keys {
			if existing == key {
				inKeys = true
				break
			}
		}
		if !inKeys {
			next.Keys = append(next.Keys, key)
			changed = true
		}
		if !changed {
			return current, false, nil
		}
		return next, true, nil
	})
	return err
}

// admitToWindow adds a key to its group's pending window under
// compare-and-set. It is idempotent, so a retried open does not double-list.
func (s *Service) admitToWindow(ctx context.Context, key Key) error {
	var backlog bool
	_, _, err := s.cfg.Windows.Update(ctx, key.GroupID, func(current Window, found bool) (Window, bool, error) {
		backlog = false
		next := Window{GroupID: key.GroupID}
		if found {
			next = current.clone()
			next.GroupID = key.GroupID
		}
		if next.contains(key) {
			return current, false, nil
		}
		if next.pending() >= MaxPendingActivities {
			// Saved, not just returned: the refusal is counted in the record
			// so a backlog is visible to whoever looks at the window, instead
			// of being visible only to the caller that happened to lose.
			backlog = true
			next.RefusedOpens++
			return next, true, nil
		}
		next.Opening = append(next.Opening, OpeningEntry{Key: key, AdmittedAtUnix: s.cfg.Now().Unix()})
		return next, true, nil
	})
	if err != nil {
		return err
	}
	if backlog {
		return fmt.Errorf("%w: group %q holds %d unfinished activities",
			ErrBacklog, key.GroupID, MaxPendingActivities)
	}
	return nil
}

// LookupActivity reads an aggregation.
func (s *Service) LookupActivity(ctx context.Context, key Key) (Activity, bool, error) {
	if err := key.Validate(); err != nil {
		return Activity{}, false, err
	}
	current, found, err := s.cfg.Activities.Get(ctx, key)
	if err != nil || !found {
		return Activity{}, false, err
	}
	return current.Value.clone(), true, nil
}

// PendingActivities lists a group's unfinished activities, bounded.
func (s *Service) PendingActivities(ctx context.Context, groupID string, limit int) ([]Key, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	window, found, err := s.cfg.Windows.Get(ctx, groupID)
	if err != nil || !found {
		return nil, err
	}
	keys := window.Value.clone().Keys
	for _, entry := range window.Value.Opening {
		keys = append(keys, entry.Key)
	}
	sortKeys(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

// DeliveringActivities lists complete activities whose dispatches are still
// being delivered (U-0192), up to limit, rotating through the group's list
// across calls so a long list is served fairly rather than from its head.
func (s *Service) DeliveringActivities(ctx context.Context, groupID string, limit int) ([]Key, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	window, found, err := s.cfg.Windows.Get(ctx, groupID)
	if err != nil || !found {
		return nil, err
	}
	keys := window.Value.clone().Delivering
	sortKeys(keys)
	if len(keys) <= limit {
		return keys, nil
	}
	s.deliveryMu.Lock()
	cursor, seen := s.deliveryCursor[groupID]
	start := 0
	if seen {
		for i, key := range keys {
			if key.String() > cursor.String() {
				start = i
				break
			}
		}
	}
	out := make([]Key, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, keys[(start+i)%len(keys)])
	}
	if s.deliveryCursor == nil {
		s.deliveryCursor = make(map[string]Key)
	}
	s.deliveryCursor[groupID] = out[len(out)-1]
	s.deliveryMu.Unlock()
	return out, nil
}

// RetireDelivered heals and, once every dispatch of a complete activity is
// terminal (acked or exhausted), drops it from the group's Delivering list.
// It reports whether the key was retired. Healing is ensureDispatches: a
// dispatch whose Create failed after the activity left the aggregation
// window has no other way back.
func (s *Service) RetireDelivered(ctx context.Context, key Key) (bool, error) {
	if err := key.Validate(); err != nil {
		return false, err
	}
	activity, found, err := s.LookupActivity(ctx, key)
	if err != nil {
		return false, err
	}
	if found {
		if activity.Status != StatusComplete {
			return false, nil
		}
		if err := s.ensureDispatches(ctx, activity); err != nil {
			return false, err
		}
		for _, gameSID := range activity.ExpectedGameSIDs {
			current, exists, err := s.cfg.Dispatches.Get(ctx, DispatchKey{Activity: key, GameSID: gameSID})
			if err != nil {
				return false, err
			}
			if !exists || current.Value.State == DispatchPending {
				return false, nil
			}
		}
	}
	_, _, err = s.cfg.Windows.Update(ctx, key.GroupID, func(current Window, found bool) (Window, bool, error) {
		if !found || !current.delivering(key) {
			return current, false, nil
		}
		next := current.clone()
		kept := next.Delivering[:0]
		for _, existing := range next.Delivering {
			if existing != key {
				kept = append(kept, existing)
			}
		}
		next.Delivering = cloneActivityKeys(kept)
		return next, true, nil
	})
	return err == nil, err
}

// --- notification and aggregation advance ---

// NotifyPhase records that one game reached the activity's phase.
//
// This is the only thing that advances an aggregation forward: the first
// notify turns the pending declaration into a collecting snapshot and starts
// the grace window, and the notify that completes the expected set finishes
// the aggregation immediately — "第一个 game 通知后创建 collecting snapshot,
// 收齐 expected games 立即完成". The advance and the collecting snapshot are
// one compare-and-set, so two games notifying at once cannot both read
// "one left" and both complete the activity.
//
// A redelivered notify from a game already counted is a no-op returning the
// current activity, and is NOT audited: it is a healthy client retrying, and
// filling the audit log with it would bury the refusals that matter. The three
// refusals the boundary document requires to be audited — late, stale status,
// notification from a game outside the expected set — go through refuseNotify,
// which cannot return without having written the record.
func (s *Service) NotifyPhase(ctx context.Context, key Key, gameSID int32) (Activity, error) {
	if err := key.Validate(); err != nil {
		return Activity{}, err
	}
	if gameSID <= 0 {
		return Activity{}, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	now := s.cfg.Now()
	nowUnix := now.Unix()

	var (
		result        Activity
		refusal       NotifyRefusal
		refusedStatus Status
		detail        string
		completed     bool
	)
	_, _, err := s.cfg.Activities.Update(ctx, key, func(current Activity, found bool) (Activity, bool, error) {
		// mutate may run again on a lost compare-and-set, so every decision
		// it records is reset here rather than accumulated.
		refusal, refusedStatus, detail, completed = "", "", "", false

		if !found {
			refusal = RefusalUnknownActivity
			return current, false, nil
		}
		if current.Notified(gameSID) {
			// Duplicate delivery of an accepted notify: idempotent, and not a
			// refusal. Checked before the status and deadline tests on
			// purpose — a retry of a notify that already counted is not late,
			// and reporting it as late would make a healthy retry look like a
			// broken clock.
			result = current.clone()
			return current, false, nil
		}
		if !current.Expects(gameSID) {
			refusal, refusedStatus = RefusalUnexpectedGame, current.Status
			detail = fmt.Sprintf("expected %v", current.ExpectedGameSIDs)
			return current, false, nil
		}
		if current.Status == StatusComplete {
			refusal, refusedStatus = RefusalStaleStatus, current.Status
			detail = fmt.Sprintf("completed at %d as %s", current.CompletedAtUnix, current.CompletionReason)
			return current, false, nil
		}
		if current.GraceExpired(nowUnix) {
			// The window closed and the sweep has not gotten to it yet.
			// Accepting this would make the result depend on the sweep's
			// timing, so the same sequence of notifies would aggregate
			// differently on a busy day.
			refusal, refusedStatus = RefusalLate, current.Status
			detail = fmt.Sprintf("grace deadline %d, now %d", current.GraceDeadlineUnix, nowUnix)
			return current, false, nil
		}

		next := current.clone()
		if next.Status == StatusPending {
			next.Status = StatusCollecting
			next.FirstNotifyAtUnix = nowUnix
			next.GraceDeadlineUnix = now.Add(s.cfg.GraceWindow).Unix()
		}
		next.NotifiedGameSIDs = append(next.NotifiedGameSIDs, gameSID)
		next.UpdatedAtUnix = nowUnix
		if next.Collected() {
			next.Status = StatusComplete
			next.CompletedAtUnix = nowUnix
			next.CompletionReason = CompletedCollected
			completed = true
		}
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		return Activity{}, err
	}
	if refusal != "" {
		return Activity{}, s.refuseNotify(ctx, key, gameSID, refusedStatus, refusal, detail)
	}
	s.report.Accepted("notify_phase")
	if completed {
		if err := s.settleCompletion(ctx, result); err != nil {
			// The aggregation IS complete; only the delivery records are
			// missing. Reporting the error lets the caller retry, and the
			// sweep heals it either way, because dispatch creation is
			// insert-only and re-runnable.
			return result, err
		}
	}
	return result, nil
}

// refuseNotify is the ONLY expression in this package that produces a notify
// refusal, and it writes the audit before it builds the error.
//
// The confirmed defect it answers: audit was documented but written on only
// some refusal paths, because refusing was a `return err` and auditing was a
// separate call next to it, so a new refusal branch simply forgot one. Here a
// branch cannot refuse without the audit — there is no error value to return
// except the one this function computes, and it is unreachable without the
// write. If the audit cannot be written the call fails as a store error rather
// than as a clean refusal: a refusal that left no trace is the thing being
// prevented, so it must not be the thing that gets returned.
func (s *Service) refuseNotify(
	ctx context.Context,
	key Key,
	gameSID int32,
	status Status,
	refusal NotifyRefusal,
	detail string,
) error {
	audit := NotifyAudit{
		Key:     key,
		GameSID: gameSID,
		Refusal: refusal,
		Status:  status,
		AtUnix:  s.cfg.Now().Unix(),
		Detail:  detail,
	}
	if err := s.appendAudit(ctx, key, audit); err != nil {
		return fmt.Errorf("activity: audit %s refusal for activity %s game %d: %w", refusal, key, gameSID, err)
	}
	// One report for every refusal, for the same reason the audit is written
	// here: a per-branch call is a call a new branch forgets.
	s.report.Refused("notify_phase", string(refusal))
	switch refusal {
	case RefusalUnknownActivity:
		return fmt.Errorf("%w: activity %s", ErrMissing, key)
	case RefusalUnexpectedGame:
		return fmt.Errorf("%w: game %d, activity %s", ErrNotifyUnexpected, gameSID, key)
	case RefusalStaleStatus:
		return fmt.Errorf("%w: activity %s is %s", ErrStatus, key, status)
	case RefusalLate:
		return fmt.Errorf("%w: activity %s, game %d", ErrNotifyLate, key, gameSID)
	default:
		// A refusal reason with no error would read as success to the caller.
		// Fail closed instead: an unknown reason is a bug in this package, and
		// the audit for it has already been written.
		return fmt.Errorf("%w: activity %s refusal %q has no error mapping", ErrInvalid, key, refusal)
	}
}

// appendAudit appends to the per-activity refusal log under compare-and-set.
// Entries are never rewritten, and a full log counts what it could not store
// instead of dropping it silently.
func (s *Service) appendAudit(ctx context.Context, key Key, audit NotifyAudit) error {
	_, _, err := s.cfg.Audits.Update(ctx, key, func(current NotifyAuditLog, found bool) (NotifyAuditLog, bool, error) {
		next := NotifyAuditLog{Key: key, NextSeq: 1}
		if found {
			next = current.clone()
			next.Key = key
			if next.NextSeq == 0 {
				next.NextSeq = 1
			}
		}
		if len(next.Entries) >= MaxNotifyAudits {
			next.Overflowed++
			return next, true, nil
		}
		entry := audit
		entry.Seq = next.NextSeq
		next.NextSeq++
		next.Entries = append(next.Entries, entry)
		return next, true, nil
	})
	return err
}

// NotifyAudits reads an activity's refusal log, newest first, bounded.
func (s *Service) NotifyAudits(ctx context.Context, key Key, limit int) ([]NotifyAudit, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	current, found, err := s.cfg.Audits.Get(ctx, key)
	if err != nil || !found {
		return nil, err
	}
	entries := cloneAudits(current.Value.Entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Seq > entries[j].Seq })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// AuditOverflow reports how many refusals were counted rather than stored,
// because the log was full. Exposed so the drop is observable: design
// constraint 6 exists because silent paths survive precisely when nothing
// reports them.
func (s *Service) AuditOverflow(ctx context.Context, key Key) (uint64, error) {
	if err := key.Validate(); err != nil {
		return 0, err
	}
	current, found, err := s.cfg.Audits.Get(ctx, key)
	if err != nil || !found {
		return 0, err
	}
	return current.Value.Overflowed, nil
}

// AdvanceExpired is the back-stop: it completes collecting aggregations whose
// grace window closed with games still missing — "未收齐则由 aggregation runner
// 扫描 pending 窗口兜底完成" — and prunes finished or vanished entries from the
// group's window.
//
// Two bounds, neither bypassable: it completes at most limit activities (and a
// non-positive limit is an error, never unlimited), and it reads at most
// MaxPendingActivities activity records, because the window it scans is itself
// bounded by count. Which activities a bounded call picks is deterministic —
// earliest deadline first — so a sweep that can only take three does not
// starve the fourth on the next call.
//
// It cannot touch a pending activity. A pending activity has no grace
// deadline, and the deadline is set by a game's first notify, so this sweep
// can only finish a window a game already opened. That is the difference
// between a back-stop and global driving the activity timeline itself.
func (s *Service) AdvanceExpired(ctx context.Context, groupID string, limit int) ([]Activity, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	window, found, err := s.cfg.Windows.Get(ctx, groupID)
	if err != nil || !found {
		return nil, err
	}

	type candidate struct {
		key      Key
		deadline int64
	}
	var (
		due         []candidate
		prune       []Key
		heal        []Activity
		confirm     []Key
		dropOpening []Key
	)
	nowUnix := s.cfg.Now().Unix()
	snapshot := window.Value.clone()
	keys := snapshot.Keys
	sortKeys(keys)
	classify := func(key Key, activity Activity) {
		switch {
		case activity.Status == StatusComplete:
			// Complete but still listed: either the completing call died
			// before it pruned, or its dispatch creation failed. Both heal
			// here, because dispatch creation is insert-only and re-running it
			// cannot duplicate a delivery.
			heal = append(heal, activity.clone())
			prune = append(prune, key)
		case activity.Status == StatusCollecting && activity.GraceExpired(nowUnix):
			due = append(due, candidate{key: key, deadline: activity.GraceDeadlineUnix})
		}
	}
	for index, key := range keys {
		if index >= MaxPendingActivities {
			// The bound is enforced on write, but a record written by an older
			// build could exceed it. Stop rather than trust stored data to
			// respect a bound this code is responsible for.
			break
		}
		current, exists, err := s.cfg.Activities.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if !exists {
			// Confirmed entries were added after Create was observed, so a
			// missing record here is genuinely gone.
			prune = append(prune, key)
			continue
		}
		classify(key, current.Value)
	}
	// Opening entries (U-0191): the opener may have died between admitting
	// and creating, or between creating and confirming. Present → confirm on
	// its behalf and treat it like any confirmed entry. Absent → nothing to
	// conclude until OpeningGrace has passed; then reclaim, and only if the
	// entry is still opening at reclaim time (see dropOpening in
	// retireFromWindow).
	openingGraceUnix := int64(s.cfg.OpeningGrace / time.Second)
	for _, entry := range snapshot.Opening {
		current, exists, err := s.cfg.Activities.Get(ctx, entry.Key)
		if err != nil {
			return nil, err
		}
		if !exists {
			if nowUnix-entry.AdmittedAtUnix >= openingGraceUnix {
				dropOpening = append(dropOpening, entry.Key)
			}
			continue
		}
		confirm = append(confirm, entry.Key)
		classify(entry.Key, current.Value)
	}
	for _, key := range confirm {
		if err := s.confirmWindow(ctx, key); err != nil {
			return nil, err
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].deadline != due[j].deadline {
			return due[i].deadline < due[j].deadline
		}
		return due[i].key.String() < due[j].key.String()
	})

	completed := make([]Activity, 0, limit)
	for _, entry := range due {
		if len(completed) >= limit {
			break
		}
		activity, advanced, err := s.completeExpired(ctx, entry.key, nowUnix)
		if err != nil {
			return completed, err
		}
		if !advanced {
			// Another sweep or a final notify got there first. Not an error:
			// the compare-and-set is what makes concurrent runners safe, and
			// losing it means the work is done.
			continue
		}
		completed = append(completed, activity)
		heal = append(heal, activity)
		prune = append(prune, entry.key)
	}

	delivering := make([]Key, 0, len(heal))
	for _, activity := range heal {
		if err := s.ensureDispatches(ctx, activity); err != nil {
			return completed, err
		}
		delivering = append(delivering, activity.Key)
	}
	if len(prune)+len(dropOpening) > 0 {
		if err := s.retireFromWindow(ctx, groupID, prune, delivering, dropOpening); err != nil {
			return completed, err
		}
	}
	return completed, nil
}

// completeExpired finishes one lapsed aggregation. advanced reports whether
// this call is the one that wrote the completion, which is how concurrent
// sweeps end up with exactly one winner per activity without a lock.
func (s *Service) completeExpired(ctx context.Context, key Key, nowUnix int64) (Activity, bool, error) {
	var (
		result   Activity
		advanced bool
	)
	_, _, err := s.cfg.Activities.Update(ctx, key, func(current Activity, found bool) (Activity, bool, error) {
		advanced = false
		if !found {
			return current, false, nil
		}
		if current.Status != StatusCollecting {
			return current, false, nil
		}
		if !current.GraceExpired(nowUnix) {
			return current, false, nil
		}
		next := current.clone()
		next.Status = StatusComplete
		next.CompletedAtUnix = nowUnix
		next.UpdatedAtUnix = nowUnix
		next.CompletionReason = CompletedGraceExpired
		result = next.clone()
		advanced = true
		return next, true, nil
	})
	if err != nil {
		return Activity{}, false, err
	}
	return result, advanced, nil
}

// settleCompletion is what follows a completion: create the deliveries, then
// drop the activity from the sweep's window. In that order, since a window
// entry for a complete activity is only a wasted read while a missing
// dispatch is a game that never learns the result.
func (s *Service) settleCompletion(ctx context.Context, activity Activity) error {
	if err := s.ensureDispatches(ctx, activity); err != nil {
		return err
	}
	return s.retireFromWindow(ctx, activity.Key.GroupID, []Key{activity.Key}, []Key{activity.Key}, nil)
}

// retireFromWindow is the one compare-and-set that moves keys out of the
// aggregation window:
//
//   - prune leaves Keys (and Opening, should a confirm have been skipped);
//   - delivering (a subset of prune: the complete ones) joins Delivering so
//     the sweep keeps retrying their dispatches (U-0192);
//   - dropOpening leaves Opening only, and only if still there — the
//     opener's confirm may have moved it to Keys since the sweep looked, in
//     which case the reclaim observed a generation that no longer exists and
//     must do nothing (U-0191).
func (s *Service) retireFromWindow(ctx context.Context, groupID string, prune, delivering, dropOpening []Key) error {
	if len(prune)+len(dropOpening) == 0 {
		return nil
	}
	remove := make(map[Key]struct{}, len(prune))
	for _, key := range prune {
		remove[key] = struct{}{}
	}
	reclaim := make(map[Key]struct{}, len(dropOpening))
	for _, key := range dropOpening {
		reclaim[key] = struct{}{}
	}
	_, _, err := s.cfg.Windows.Update(ctx, groupID, func(current Window, found bool) (Window, bool, error) {
		if !found {
			return current, false, nil
		}
		next := current.clone()
		changed := false
		kept := next.Keys[:0]
		for _, key := range next.Keys {
			if _, drop := remove[key]; drop {
				changed = true
				continue
			}
			kept = append(kept, key)
		}
		next.Keys = cloneActivityKeys(kept)
		opening := next.Opening[:0]
		for _, entry := range next.Opening {
			_, drop := remove[entry.Key]
			_, reclaimed := reclaim[entry.Key]
			if drop || reclaimed {
				changed = true
				continue
			}
			opening = append(opening, entry)
		}
		next.Opening = append([]OpeningEntry(nil), opening...)
		for _, key := range delivering {
			if !next.delivering(key) {
				next.Delivering = append(next.Delivering, key)
				changed = true
			}
		}
		if !changed {
			return current, false, nil
		}
		return next, true, nil
	})
	return err
}

// --- participant progress ---

// ApplyProgress adds a delta to one participant's standing, exactly once per
// request id.
//
// The confirmed defect: the notify path had no idempotency at all, so a
// redelivered notification double-counted a participant's progress. The
// boundary document already said what the shape had to be — "participant
// progress 必须先通过 request ledger CAS reserve 再更新参与者积分/进度" — so
// this is two steps that cannot be collapsed:
//
//  1. Reserve. An insert-only Create on the ledger claims the right to apply.
//     Insert-only, not "read then write if absent": a read-then-write claim
//     hands the right to both of two concurrent callers, which is the double
//     count with extra steps.
//  2. Apply. One compare-and-set moves the score and records the request id in
//     the participant's bounded ring together, so the record of having applied
//     cannot exist without the apply, and vice versa.
//
// A replayed request id is a no-op that returns the participant's current
// state: an applied reservation short-circuits before the participant store is
// touched, and if the reservation is still merely reserved — the caller that
// claimed it lost the race to get here, or died between the two steps — the
// apply is re-driven and the ring makes it a no-op. There is no interleaving
// of a redelivery that counts twice.
//
// The request id is not trusted as an identity, only as an idempotency key,
// and it is scoped by activity and participant so it cannot address another
// participant's progress. It is client-chosen, which design constraint 4
// allows exactly because it is reserved through this ledger rather than
// believed.
func (s *Service) ApplyProgress(
	ctx context.Context,
	key Key,
	participantID string,
	requestID string,
	delta ProgressDelta,
) (Participant, error) {
	requestKey := RequestKey{Activity: key, ParticipantID: participantID, RequestID: requestID}
	if err := requestKey.Validate(); err != nil {
		return Participant{}, err
	}
	if err := delta.Validate(); err != nil {
		return Participant{}, err
	}

	// Progress is refused once the aggregation completed: its result has been
	// dispatched, so an apply after that would be invisible to every game that
	// already settled on it. Refusing loudly beats a score that only some
	// servers know about.
	activity, found, err := s.LookupActivity(ctx, key)
	if err != nil {
		return Participant{}, err
	}
	if !found {
		return Participant{}, fmt.Errorf("%w: activity %s", ErrMissing, key)
	}
	if activity.Status == StatusComplete {
		return Participant{}, fmt.Errorf("%w: activity %s completed at %d",
			ErrStatus, key, activity.CompletedAtUnix)
	}

	now := s.cfg.Now()
	participantKey := ParticipantKey{Activity: key, ParticipantID: participantID}

	reservation := ProgressReservation{
		Key:           requestKey,
		Delta:         delta,
		State:         ReservationReserved,
		CreatedAtUnix: now.Unix(),
		ExpiresAtUnix: now.Add(s.cfg.ReservationTTL).Unix(),
	}
	_, created, err := s.cfg.Ledger.Create(ctx, requestKey, reservation)
	if err != nil {
		return Participant{}, err
	}
	if !created {
		existing, ok, err := s.cfg.Ledger.Get(ctx, requestKey)
		if err != nil {
			return Participant{}, err
		}
		if !ok {
			// The entry existed at Create and was gone at Get: its ttl elapsed
			// in between. Applying now would be applying without a claim,
			// which is the one path that could count twice, so refuse and let
			// the caller decide.
			return Participant{}, fmt.Errorf("%w: reservation for request %q vanished", ErrRequestInvalid, requestID)
		}
		if existing.Value.Delta != delta {
			// One idempotency key, two different requests. Neither answer is
			// right and applying both is the double count.
			return Participant{}, fmt.Errorf("%w: request %q was reserved for a different delta", ErrRequestInvalid, requestID)
		}
		if existing.Value.State == ReservationApplied {
			current, _, err := s.lookupParticipant(ctx, participantKey)
			if err != nil {
				return Participant{}, err
			}
			s.report.Replayed("apply_progress")
			return current, nil
		}
		// Reserved but not yet marked applied. Fall through: the ring inside
		// the apply below makes re-driving it a no-op.
	}

	var result Participant
	_, _, err = s.cfg.Participants.Update(ctx, participantKey, func(current Participant, found bool) (Participant, bool, error) {
		if found && current.Applied(requestID) {
			// The apply already landed. Returning the current state without a
			// write is what "a replay is a no-op" means concretely: Applies
			// does not move, so the no-op is observable.
			result = current.clone()
			return current, false, nil
		}
		next := Participant{Key: key, ParticipantID: participantID}
		if found {
			next = current.clone()
			next.Key, next.ParticipantID = key, participantID
		}
		next.Score += delta.Score
		next.Progress += delta.Progress
		next.AppliedRequestIDs = appendBounded(next.AppliedRequestIDs, requestID, MaxProgressWindow)
		next.Applies++
		next.UpdatedAtUnix = now.Unix()
		result = next.clone()
		return next, true, nil
	})
	if err != nil {
		return Participant{}, err
	}

	// Mark the claim applied, so a replay is answered by the ledger and never
	// reaches the participant record — the ring is bounded and cannot answer
	// for a replay that arrives after MaxProgressWindow other requests.
	if err := s.markReservationApplied(ctx, requestKey, now.Unix()); err != nil {
		return result, err
	}
	s.report.Accepted("apply_progress")
	return result, nil
}

func (s *Service) markReservationApplied(ctx context.Context, key RequestKey, nowUnix int64) error {
	_, _, err := s.cfg.Ledger.Update(ctx, key, func(current ProgressReservation, found bool) (ProgressReservation, bool, error) {
		if !found {
			// The entry expired under us. Recreating it here would resurrect a
			// claim the store deliberately reaped, so leave it gone.
			return current, false, nil
		}
		if current.State == ReservationApplied {
			return current, false, nil
		}
		next := current
		next.State = ReservationApplied
		next.AppliedAtUnix = nowUnix
		return next, true, nil
	})
	return err
}

// LookupParticipant reads one participant's standing.
func (s *Service) LookupParticipant(ctx context.Context, key Key, participantID string) (Participant, bool, error) {
	participantKey := ParticipantKey{Activity: key, ParticipantID: participantID}
	if err := participantKey.Validate(); err != nil {
		return Participant{}, false, err
	}
	current, found, err := s.cfg.Participants.Get(ctx, participantKey)
	if err != nil || !found {
		return Participant{}, false, err
	}
	return current.Value.clone(), true, nil
}

func (s *Service) lookupParticipant(ctx context.Context, key ParticipantKey) (Participant, bool, error) {
	current, found, err := s.cfg.Participants.Get(ctx, key)
	if err != nil || !found {
		return Participant{}, false, err
	}
	return current.Value.clone(), true, nil
}

// Reservation reads one ledger entry. Exposed for operators answering "did
// request X apply", which is the question a double-count investigation starts
// from and which the implementation this replaces could not answer at all.
func (s *Service) Reservation(ctx context.Context, key Key, participantID, requestID string) (ProgressReservation, bool, error) {
	requestKey := RequestKey{Activity: key, ParticipantID: participantID, RequestID: requestID}
	if err := requestKey.Validate(); err != nil {
		return ProgressReservation{}, false, err
	}
	current, found, err := s.cfg.Ledger.Get(ctx, requestKey)
	if err != nil || !found {
		return ProgressReservation{}, false, err
	}
	return current.Value, true, nil
}

// --- dispatch ---

// ensureDispatches creates the missing deliveries for a completed
// aggregation, one per expected game. Insert-only per game, so re-running it
// after a partial failure cannot duplicate a delivery or, worse, mint a second
// token and invalidate an ACK that is already in flight.
func (s *Service) ensureDispatches(ctx context.Context, activity Activity) error {
	if activity.Status != StatusComplete {
		return fmt.Errorf("%w: activity %s is %s, dispatch needs a result",
			ErrStatus, activity.Key, activity.Status)
	}
	result := activity.result()
	nowUnix := s.cfg.Now().Unix()
	for _, gameSID := range activity.ExpectedGameSIDs {
		key := DispatchKey{Activity: activity.Key, GameSID: gameSID}
		_, found, err := s.cfg.Dispatches.Get(ctx, key)
		if err != nil {
			return err
		}
		if found {
			continue
		}
		token, err := s.cfg.NewDispatchToken()
		if err != nil {
			return err
		}
		if token == "" {
			// An empty token would authorize every ACK.
			return fmt.Errorf("activity: dispatch token generator returned an empty token")
		}
		dispatch := Dispatch{
			Key:               activity.Key,
			GameSID:           gameSID,
			Token:             token,
			Result:            result.clone(),
			State:             DispatchPending,
			MaxAttempts:       s.cfg.DispatchMaxAttempts,
			NextAttemptAtUnix: nowUnix,
			CreatedAtUnix:     nowUnix,
		}
		if _, _, err := s.cfg.Dispatches.Create(ctx, key, dispatch); err != nil {
			return err
		}
	}
	return nil
}

// LookupDispatch reads one delivery record.
func (s *Service) LookupDispatch(ctx context.Context, key Key, gameSID int32) (Dispatch, bool, error) {
	if err := key.Validate(); err != nil {
		return Dispatch{}, false, err
	}
	if gameSID <= 0 {
		return Dispatch{}, false, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	current, found, err := s.cfg.Dispatches.Get(ctx, DispatchKey{Activity: key, GameSID: gameSID})
	if err != nil || !found {
		return Dispatch{}, false, err
	}
	return current.Value.clone(), true, nil
}

// DueDispatches lists the deliveries for one activity that may be attempted
// now, bounded. The scan is over the activity's expected set, which is itself
// bounded by MaxExpectedGames, so this needs no index of its own.
// OwedDispatchIndex is implemented by a dispatch store that can answer "what
// is owed to this game server", cheaply and bounded.
//
// It is an optional capability rather than part of the Store contract because
// only a backend with a secondary index can do it — and the alternative, which
// is what the game had to do before, is to GUESS activity ids from its own
// clock and look each one up. That works until a game is down longer than a
// window, and then the obligation is still recorded and permanently
// unreachable (RR-20260919-10).
type OwedDispatchIndex interface {
	// OwedDispatches returns up to limit dispatch keys owed to gameSID in
	// groupID that are pending and due at nowUnix, oldest first.
	OwedDispatches(ctx context.Context, groupID string, gameSID int32, nowUnix int64, limit int) ([]DispatchKey, error)
}

// OwedDispatches lists the activities whose result this game server still owes
// an ack for, oldest first.
//
// It is how a game finds work it did not know about: no activity id, no clock
// arithmetic, no assumption about how long it was away. The keys come back;
// taking the payload is AttemptDispatch, which is also where an attempt is
// spent — so a result is only ever charged an attempt when somebody actually
// took it.
func (s *Service) OwedDispatches(ctx context.Context, groupID string, gameSID int32, limit int) ([]Key, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, fmt.Errorf("%w: group id is empty", ErrInvalid)
	}
	if gameSID <= 0 {
		return nil, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	index, ok := s.cfg.Dispatches.(OwedDispatchIndex)
	if !ok {
		return nil, fmt.Errorf("%w: this deployment's dispatch store keeps no per-game index, "+
			"so what a game server is owed cannot be enumerated; use NewRedisStores", ErrUnsupported)
	}
	keys, err := index.OwedDispatches(ctx, groupID, gameSID, s.cfg.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]Key, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.Activity)
	}
	return out, nil
}

func (s *Service) DueDispatches(ctx context.Context, key Key, limit int) ([]Dispatch, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := validateLimit(limit); err != nil {
		return nil, err
	}
	activity, found, err := s.LookupActivity(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: activity %s", ErrMissing, key)
	}
	nowUnix := s.cfg.Now().Unix()
	out := make([]Dispatch, 0, limit)
	for _, gameSID := range activity.ExpectedGameSIDs {
		current, exists, err := s.cfg.Dispatches.Get(ctx, DispatchKey{Activity: key, GameSID: gameSID})
		if err != nil {
			return nil, err
		}
		if !exists || !current.Value.Due(nowUnix) {
			continue
		}
		out = append(out, current.Value.clone())
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// AttemptDispatch takes one delivery attempt from a dispatch's budget and
// returns the record to deliver, token included.
//
// It is the retry runner's only way to get a payload, which is what keeps the
// attempt count honest: the count is incremented in the same compare-and-set
// that hands out the payload, so two runners cannot both deliver on one
// attempt, and a delivery cannot happen without being counted. When the budget
// is spent the dispatch moves to DispatchExhausted, an explicit terminal state
// rather than a record that simply stops being picked up — a queue that
// silently stops draining is exactly the failure the design constraints say
// survives because nothing reports it.
func (s *Service) AttemptDispatch(ctx context.Context, key Key, gameSID int32) (Dispatch, error) {
	if err := key.Validate(); err != nil {
		return Dispatch{}, err
	}
	if gameSID <= 0 {
		return Dispatch{}, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	now := s.cfg.Now()
	nowUnix := now.Unix()

	var (
		result    Dispatch
		refusal   error
		exhausted bool
	)
	_, _, err := s.cfg.Dispatches.Update(ctx, DispatchKey{Activity: key, GameSID: gameSID},
		func(current Dispatch, found bool) (Dispatch, bool, error) {
			refusal, exhausted = nil, false
			if !found {
				refusal = fmt.Errorf("%w: activity %s game %d", ErrDispatchMissing, key, gameSID)
				return current, false, nil
			}
			switch current.State {
			case DispatchAcked:
				// Already processed by the game: nothing to deliver, and not
				// an error — a runner that raced an ACK should stop, not
				// retry.
				result = current.clone()
				return current, false, nil
			case DispatchExhausted:
				refusal = fmt.Errorf("%w: activity %s game %d after %d attempts",
					ErrDispatchExhausted, key, gameSID, current.Attempts)
				return current, false, nil
			}
			if current.Attempts >= current.MaxAttempts {
				// Budget spent and still unacknowledged. Record the terminal
				// state; the refusal is reported after the write so the state
				// is durable before the caller hears about it.
				next := current.clone()
				next.State = DispatchExhausted
				next.ExhaustedAtUnix = nowUnix
				result = next.clone()
				exhausted = true
				return next, true, nil
			}
			if !current.Due(nowUnix) {
				refusal = fmt.Errorf("%w: activity %s game %d is due at %d, now %d",
					ErrDispatchNotDue, key, gameSID, current.NextAttemptAtUnix, nowUnix)
				return current, false, nil
			}
			next := current.clone()
			next.Attempts++
			next.LastAttemptAtUnix = nowUnix
			next.NextAttemptAtUnix = now.Add(s.dispatchBackoff(next.Attempts)).Unix()
			result = next.clone()
			return next, true, nil
		})
	if err != nil {
		return Dispatch{}, err
	}
	if exhausted {
		// The one genuinely lost thing in this file: the activity completed
		// and this game will not learn the result. It leaves no audit — the
		// audit log covers notify refusals — so the counter is the only trace.
		s.report.Dropped("dispatch.exhausted", 1)
		return result, fmt.Errorf("%w: activity %s game %d after %d attempts",
			ErrDispatchExhausted, key, gameSID, result.Attempts)
	}
	if refusal != nil {
		return Dispatch{}, refusal
	}
	return result, nil
}

// dispatchBackoff grows the delay with the attempt count, capped, so a game
// that is down is retried less often rather than at a fixed rate that turns a
// single outage into a hot loop.
func (s *Service) dispatchBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > maxBackoffMultiplier {
		attempt = maxBackoffMultiplier
	}
	return s.cfg.DispatchBackoff * time.Duration(attempt)
}

// AckDispatch records that a game processed a delivered result.
//
// The token is required and compared in constant time. It is the whole
// authorization: the dispatch key is derivable by anyone who knows the
// activity and a game sid, so without a token any caller could mark another
// game's settlement processed and the retry would stop before the game ever
// saw it.
func (s *Service) AckDispatch(ctx context.Context, key Key, gameSID int32, token string) (Dispatch, error) {
	if err := key.Validate(); err != nil {
		return Dispatch{}, err
	}
	if gameSID <= 0 {
		return Dispatch{}, fmt.Errorf("%w: game sid must be positive, got %d", ErrInvalid, gameSID)
	}
	if token == "" {
		return Dispatch{}, fmt.Errorf("%w: activity %s game %d", ErrDispatchToken, key, gameSID)
	}
	nowUnix := s.cfg.Now().Unix()

	var (
		result   Dispatch
		refusal  error
		accepted bool
	)
	_, _, err := s.cfg.Dispatches.Update(ctx, DispatchKey{Activity: key, GameSID: gameSID},
		func(current Dispatch, found bool) (Dispatch, bool, error) {
			refusal, accepted = nil, false
			if !found {
				refusal = fmt.Errorf("%w: activity %s game %d", ErrDispatchMissing, key, gameSID)
				return current, false, nil
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(current.Token)) != 1 {
				// Deliberately says nothing about the expected token.
				s.report.Refused("ack_dispatch", "bad_token")
				refusal = fmt.Errorf("%w: activity %s game %d", ErrDispatchToken, key, gameSID)
				return current, false, nil
			}
			if current.State == DispatchExhausted {
				refusal = fmt.Errorf("%w: activity %s game %d, ack arrived after the budget was spent",
					ErrDispatchExhausted, key, gameSID)
				return current, false, nil
			}
			if current.State == DispatchAcked {
				// Idempotent: a redelivered ACK must not fail, and must not
				// move the ack time either, or "when did game 7 process this"
				// becomes whenever it last retried.
				result = current.clone()
				return current, false, nil
			}
			next := current.clone()
			next.State = DispatchAcked
			next.AckedAtUnix = nowUnix
			result = next.clone()
			accepted = true
			return next, true, nil
		})
	if err != nil {
		return Dispatch{}, err
	}
	if refusal != nil {
		return Dispatch{}, refusal
	}
	if accepted {
		s.report.Accepted("ack_dispatch")
	} else {
		s.report.Replayed("ack_dispatch")
	}
	return result, nil
}

func cloneActivityKeys(in []Key) []Key {
	if len(in) == 0 {
		return nil
	}
	out := make([]Key, len(in))
	copy(out, in)
	return out
}
