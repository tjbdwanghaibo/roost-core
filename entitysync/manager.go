package entitysync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// DefaultInterval is the tick: how often dirty subjects become frames. It is
// a batching window, not a latency budget — a change reaches every session
// within one interval.
const DefaultInterval = 50 * time.Millisecond

// ManagerConfig shapes the one Manager a process runs.
type ManagerConfig struct {
	// Transport receives one frame per session per tick. Required.
	Transport Transport
	// Interval is the tick period for Start. Zero takes DefaultInterval.
	Interval time.Duration
	// Limits bound a frame. Zero fields take statesync.DefaultLimits.
	// MaxObjects is also how many subjects one session may hold.
	Limits core.Limits
	// DurableWatermark, when set, is the pipelined-commit watermark
	// (typically the Data Engine's DurableLSN). A subject whose newest
	// commit lies above it is held back whole — snapshots and deltas — until
	// the watermark reaches it, so no client ever sees state the server can
	// still lose.
	DurableWatermark func() uint64

	// Capacity. Zero takes the defaults below.
	MaxSubjects              int
	MaxSessions              int
	MaxSubscribersPerSubject int

	// Wire constants stamped on every frame.
	FrameSchemaVersion     uint16
	Archetype              uint16
	ComponentTypeID        uint16
	ComponentSchemaVersion uint16

	// SessionLost is told when a session was closed by the manager because
	// its transport refused a frame (not on CloseSession). The policy that
	// subscribed the session uses it to forget the session on its side —
	// there is no reverse index here to do that for it (ARCH-10).
	SessionLost func(session SessionID, cause error)
}

const (
	DefaultMaxSubjects              = 65536
	DefaultMaxSessions              = 65536
	DefaultMaxSubscribersPerSubject = 1024
)

func (c ManagerConfig) normalized() ManagerConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	defaults := core.DefaultLimits()
	if c.Limits.MaxObjects <= 0 {
		c.Limits.MaxObjects = defaults.MaxObjects
	}
	if c.Limits.MaxComponentsPerObject <= 0 {
		c.Limits.MaxComponentsPerObject = defaults.MaxComponentsPerObject
	}
	if c.Limits.MaxComponentBytes <= 0 {
		c.Limits.MaxComponentBytes = defaults.MaxComponentBytes
	}
	if c.Limits.MaxFrameBytes <= 0 {
		c.Limits.MaxFrameBytes = defaults.MaxFrameBytes
	}
	if c.Limits.MaxDatagramBytes <= 0 {
		c.Limits.MaxDatagramBytes = defaults.MaxDatagramBytes
	}
	if c.Limits.MaxFragments <= 0 {
		c.Limits.MaxFragments = defaults.MaxFragments
	}
	if c.MaxSubjects <= 0 {
		c.MaxSubjects = DefaultMaxSubjects
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.MaxSubscribersPerSubject <= 0 {
		c.MaxSubscribersPerSubject = DefaultMaxSubscribersPerSubject
	}
	if c.FrameSchemaVersion == 0 {
		c.FrameSchemaVersion = 1
	}
	if c.Archetype == 0 {
		c.Archetype = 1
	}
	if c.ComponentTypeID == 0 {
		c.ComponentTypeID = 1
	}
	if c.ComponentSchemaVersion == 0 {
		c.ComponentSchemaVersion = 1
	}
	return c
}

// Manager owns every replicated subject and every receiving session in the
// process. It is the whole scheduler: policies tell it who subscribes to
// whom, entities tell it (through their dirty notifier) what changed, and on
// each tick it gives every affected session one frame.
type Manager struct {
	config ManagerConfig
	wire   wireConfig

	mu       sync.RWMutex
	subjects map[int64]*subject
	sessions map[SessionID]*session
	closed   bool

	pendingMu sync.Mutex
	pending   map[int64]struct{}

	// flushMu makes ticks sequential: Flush, Stop's final flush and Close
	// never overlap, so a subject's Prepare is never in flight twice.
	flushMu sync.Mutex

	runMu   sync.Mutex
	running bool
	stopCh  chan struct{}
	doneCh  chan struct{}

	framesAdmitted atomic.Uint64
	sessionsLost   atomic.Uint64
	deferred       atomic.Uint64
	flushFailures  atomic.Uint64
	errMu          sync.Mutex
	lastError      error
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if config.Transport == nil {
		return nil, ErrTransportRequired
	}
	config = config.normalized()
	return &Manager{
		config: config,
		wire: wireConfig{
			limits: config.Limits, schemaVersion: config.FrameSchemaVersion, archetype: config.Archetype,
			componentType: config.ComponentTypeID, componentSchema: config.ComponentSchemaVersion,
		},
		subjects: make(map[int64]*subject),
		sessions: make(map[SessionID]*session),
		pending:  make(map[int64]struct{}),
	}, nil
}

// ---- subjects ----

// Register makes an entity's sync state a subject. From now on its dirty
// notifications schedule it for the next tick; nobody receives it until a
// policy subscribes a session.
func (m *Manager) Register(state *entity.SubjectSyncState) error {
	if m == nil {
		return ErrManagerClosed
	}
	if state == nil || !state.Enabled() || state.SubjectID() == 0 {
		return ErrSubjectInvalid
	}
	id := state.SubjectID()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if existing, ok := m.subjects[id]; ok {
		m.mu.Unlock()
		existing.mu.Lock()
		retiring := existing.retiring
		existing.mu.Unlock()
		if retiring {
			return ErrSubjectRetiring
		}
		return ErrSubjectRegistered
	}
	if len(m.subjects) >= m.config.MaxSubjects {
		m.mu.Unlock()
		return ErrSubjectLimit
	}
	m.subjects[id] = newSubject(state)
	m.mu.Unlock()
	// Installing the notifier also fires it when the state is already dirty,
	// so a subject that changed before it was registered is not forgotten.
	state.SetDirtyNotifier(func(*entity.SubjectSyncState) { m.markPending(id) })
	return nil
}

// Unregister retires a subject: every subscriber is owed an ObjectRemove on
// its next frame, and once the last one has gone out the subject is
// forgotten. Subscribing to a retiring subject is refused.
func (m *Manager) Unregister(subjectID int64) error {
	subj := m.subject(subjectID)
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	subj.mu.Lock()
	subj.retiring = true
	for id, sub := range subj.subscribers {
		if sub.kind == kindSnapshot {
			// Never received anything; nothing to take back.
			delete(subj.subscribers, id)
			continue
		}
		sub.kind = kindLeaving
	}
	remaining := len(subj.subscribers)
	subj.mu.Unlock()
	if remaining == 0 {
		m.forget(subjectID)
		return nil
	}
	m.markPending(subjectID)
	return nil
}

// forget drops a subject whose last remove has gone out.
func (m *Manager) forget(subjectID int64) {
	m.mu.Lock()
	subj, ok := m.subjects[subjectID]
	if ok {
		delete(m.subjects, subjectID)
	}
	m.mu.Unlock()
	if ok {
		subj.state.SetDirtyNotifier(nil)
	}
}

func (m *Manager) subject(subjectID int64) *subject {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.subjects[subjectID]
}

// ---- sessions ----

// OpenSession admits a receiver. It is idempotent: an already open session
// stays as it is.
func (m *Manager) OpenSession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	if id == 0 {
		return ErrSessionInvalid
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if _, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return nil
	}
	if len(m.sessions) >= m.config.MaxSessions {
		m.mu.Unlock()
		return ErrSessionLimit
	}
	m.sessions[id] = newSession(id)
	m.mu.Unlock()
	if lifecycle, ok := m.config.Transport.(SessionLifecycle); ok {
		if err := lifecycle.SessionOpened(id); err != nil {
			m.mu.Lock()
			delete(m.sessions, id)
			m.mu.Unlock()
			return err
		}
	}
	return nil
}

// OpenHeldSession admits a receiver that is not ready to receive yet: it can
// be subscribed, but no frame is encoded for it until ReadySession. A client
// that installs its decoder only after the login answer wants this — the
// first snapshot must not race the decoder (ARCH-10, "session ready").
func (m *Manager) OpenHeldSession(id SessionID) error {
	if err := m.OpenSession(id); err != nil {
		return err
	}
	return m.HoldSession(id)
}

// HoldSession stops frames to an open session and starts it over: every
// subscription it holds goes back to "needs a snapshot" and the next frame it
// gets (after ReadySession) opens a new epoch with a FrameFull. Use it when
// the receiver has lost its state — a reconnect, a client-side reset.
func (m *Manager) HoldSession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionUnknown
	}
	fresh := newSession(id)
	fresh.epoch = sess.epoch + 1
	fresh.framesSent = sess.framesSent
	fresh.held = true
	m.sessions[id] = fresh
	subjects := make([]*subject, 0, len(m.subjects))
	for _, subj := range m.subjects {
		subjects = append(subjects, subj)
	}
	m.mu.Unlock()
	for _, subj := range subjects {
		subj.mu.Lock()
		if sub, subscribed := subj.subscribers[id]; subscribed {
			switch sub.kind {
			case kindLeaving:
				// It was owed a remove; with its state gone there is nothing to remove.
				delete(subj.subscribers, id)
			default:
				sub.kind, sub.baseVersion = kindSnapshot, 0
			}
		}
		subj.mu.Unlock()
	}
	return nil
}

// ReadySession lets frames flow to a held session. Every subject it is
// subscribed to is scheduled so the snapshots go out on the next tick.
func (m *Manager) ReadySession(id SessionID) error {
	if m == nil {
		return ErrManagerClosed
	}
	m.mu.Lock()
	sess, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return ErrSessionUnknown
	}
	sess.held = false
	subjects := make([]*subject, 0, len(m.subjects))
	for _, subj := range m.subjects {
		subjects = append(subjects, subj)
	}
	m.mu.Unlock()
	for _, subj := range subjects {
		subj.mu.Lock()
		_, subscribed := subj.subscribers[id]
		subj.mu.Unlock()
		if subscribed {
			m.markPending(subj.id)
		}
	}
	return nil
}

// CloseSession forgets a receiver: its frame state goes, and every subject
// drops its subscription without owing it a remove — there is nobody to send
// one to. The policy that subscribed it is expected to have forgotten it
// already; this is the safety net for the entries it missed.
func (m *Manager) CloseSession(id SessionID) {
	m.dropSession(id, nil, false)
}

// loseSession is CloseSession for a session the transport refused: counted,
// and reported to the policy through SessionLost.
func (m *Manager) loseSession(id SessionID, cause error) {
	m.dropSession(id, cause, true)
}

func (m *Manager) dropSession(id SessionID, cause error, lost bool) {
	if m == nil || id == 0 {
		return
	}
	m.mu.Lock()
	_, existed := m.sessions[id]
	delete(m.sessions, id)
	subjects := make([]*subject, 0, len(m.subjects))
	for _, subj := range m.subjects {
		subjects = append(subjects, subj)
	}
	m.mu.Unlock()
	if !existed {
		return
	}
	if lifecycle, ok := m.config.Transport.(SessionLifecycle); ok {
		lifecycle.SessionClosed(id)
	}
	for _, subj := range subjects {
		subj.mu.Lock()
		if _, subscribed := subj.subscribers[id]; subscribed {
			delete(subj.subscribers, id)
			if subj.retiring && len(subj.subscribers) == 0 {
				// Its last subscriber left before the remove could go out.
				defer m.forget(subj.id)
			}
		}
		subj.mu.Unlock()
	}
	if lost {
		m.sessionsLost.Add(1)
		metrics.IncCounter("entitysync_sessions_lost_total", nil, 1)
		if m.config.SessionLost != nil {
			m.config.SessionLost(id, cause)
		}
	}
}

// adoptSession installs the state a tick encoded against, unless the session
// was closed meanwhile.
func (m *Manager) adoptSession(next *session) {
	m.mu.Lock()
	if _, open := m.sessions[next.id]; open {
		m.sessions[next.id] = next
	}
	m.mu.Unlock()
}

func (m *Manager) session(id SessionID) *session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ---- subscriptions ----

// Subscribe makes a session a receiver of a subject under a profile. The
// session gets a full snapshot on the next tick; changing the profile of an
// existing subscription does the same.
func (m *Manager) Subscribe(session SessionID, subjectID int64, profile entity.SyncProfile) error {
	if m == nil {
		return ErrManagerClosed
	}
	if session == 0 {
		return ErrSessionInvalid
	}
	if m.session(session) == nil {
		return ErrSessionUnknown
	}
	subj := m.subject(subjectID)
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	profile = profile.Normalize()
	subj.mu.Lock()
	defer subj.mu.Unlock()
	if subj.retiring {
		return ErrSubjectRetiring
	}
	if existing, ok := subj.subscribers[session]; ok {
		switch {
		case existing.kind == kindLeaving:
			// Back before the remove went out. It may have missed deltas
			// while leaving, so it starts over with a snapshot.
			existing.profile, existing.kind, existing.baseVersion = profile, kindSnapshot, 0
		case existing.profile != profile:
			existing.profile, existing.kind, existing.baseVersion = profile, kindSnapshot, 0
		default:
			return nil
		}
		m.markPending(subjectID)
		return nil
	}
	if len(subj.subscribers) >= m.config.MaxSubscribersPerSubject {
		return ErrSubscriberLimit
	}
	subj.subscribers[session] = &subscription{profile: profile, kind: kindSnapshot}
	m.markPending(subjectID)
	return nil
}

// Unsubscribe ends a subscription. A session that already holds the object is
// owed an ObjectRemove on its next frame; one that never received anything is
// simply forgotten.
func (m *Manager) Unsubscribe(session SessionID, subjectID int64) error {
	if m == nil {
		return ErrManagerClosed
	}
	subj := m.subject(subjectID)
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	subj.mu.Lock()
	defer subj.mu.Unlock()
	existing, ok := subj.subscribers[session]
	if !ok {
		return ErrSubscriptionNotFound
	}
	if sess := m.session(session); sess == nil || sess.held || existing.kind == kindSnapshot {
		// Nothing was ever delivered to this session for this subject (or
		// its state is gone): there is no object to take back.
		delete(subj.subscribers, session)
		if subj.retiring && len(subj.subscribers) == 0 {
			defer m.forget(subjectID)
		}
		return nil
	}
	existing.kind = kindLeaving
	m.markPending(subjectID)
	return nil
}

// Subscribers reports the sessions subscribed to a subject, in ascending
// order, whatever their standing.
func (m *Manager) Subscribers(subjectID int64) []SessionID {
	subj := m.subject(subjectID)
	if subj == nil {
		return nil
	}
	subj.mu.Lock()
	defer subj.mu.Unlock()
	return subj.sessionsLocked()
}

func (m *Manager) markPending(subjectID int64) {
	m.pendingMu.Lock()
	m.pending[subjectID] = struct{}{}
	m.pendingMu.Unlock()
}

func (m *Manager) takePending() []int64 {
	m.pendingMu.Lock()
	ids := make([]int64, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	clear(m.pending)
	m.pendingMu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ---- the tick ----

// settlement is what becomes true for one (session, subject) once the
// session's frame has been admitted.
type settlement struct {
	subj    *subject
	version uint64
	remove  bool
}

// Flush is one tick. Every pending subject is captured once (one packer call
// per distinct profile, under its entity lock), the captures are dealt into
// per-session frames, each frame is pushed, and only then are the captures
// committed. A session whose push fails is closed; a transport that cannot
// take anything (ErrRetryLater) abandons the tick with every subject still
// dirty. Subjects behind the durable watermark are skipped whole and retried.
func (m *Manager) Flush(ctx context.Context) error {
	if m == nil {
		return ErrManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if m.isClosed() {
		return ErrManagerClosed
	}
	ids := m.takePending()
	if len(ids) == 0 {
		return nil
	}
	var watermark uint64
	gated := m.config.DurableWatermark != nil
	if gated {
		watermark = m.config.DurableWatermark()
	}

	var prepared []*entity.PreparedSubjectSync
	frames := make(map[SessionID][]frameEntry)
	settlements := make(map[SessionID][]settlement)
	var retry []int64
	var failures []error

	for _, id := range ids {
		subj := m.subject(id)
		if subj == nil {
			continue
		}
		subj.mu.Lock()
		// Sessions that closed since are pruned here: their entries owe
		// nothing. Held sessions are skipped for this tick — ReadySession
		// schedules the subject again when they can receive.
		held := make(map[SessionID]bool)
		for sid := range subj.subscribers {
			sess := m.session(sid)
			switch {
			case sess == nil:
				delete(subj.subscribers, sid)
			case sess.held:
				held[sid] = true
			}
		}
		snapshotProfiles := subj.profilesLocked(kindSnapshot)
		deltaProfiles := subj.profilesLocked(kindLive)
		dirty := subj.state.PendingDirty()
		wantsCapture := len(snapshotProfiles) > 0 || dirty
		if wantsCapture {
			// A dirty subject nobody live is watching still gets captured
			// (against the default profile) so Commit clears its dirty state
			// instead of leaving it pending forever.
			item, err := subj.state.PrepareTick(deltaProfiles, snapshotProfiles)
			switch {
			case errors.Is(err, entity.ErrSubjectSyncNotDirty):
			case err != nil:
				failures = append(failures, fmt.Errorf("subject %d: %w", id, err))
				retry = append(retry, id)
			case gated && capturedAbove(item, watermark):
				_ = item.AbortWithError(ErrDurabilityDeferred)
				m.deferred.Add(1)
				metrics.IncCounter("entitysync_durability_gate_deferred_total", nil, 1)
				retry = append(retry, id)
			default:
				prepared = append(prepared, item)
				snapshots, deltas := item.Snapshots(), item.Updates()
				for sid, sub := range subj.subscribers {
					if held[sid] {
						continue
					}
					switch sub.kind {
					case kindSnapshot:
						if update, ok := updateFor(snapshots, sub.profile); ok {
							frames[sid] = append(frames[sid], frameEntry{subjectID: id, kind: entryCreate, update: update})
							settlements[sid] = append(settlements[sid], settlement{subj: subj, version: item.Version()})
						}
					case kindLive:
						if !dirty {
							continue
						}
						if update, ok := updateFor(deltas, sub.profile); ok {
							frames[sid] = append(frames[sid], frameEntry{subjectID: id, kind: entryUpdate, update: update})
							settlements[sid] = append(settlements[sid], settlement{subj: subj, version: item.Version()})
						}
					}
				}
			}
		}
		for sid, sub := range subj.subscribers {
			if sub.kind == kindLeaving && !held[sid] {
				frames[sid] = append(frames[sid], frameEntry{subjectID: id, kind: entryRemove})
				settlements[sid] = append(settlements[sid], settlement{subj: subj, remove: true})
			}
		}
		subj.mu.Unlock()
	}

	sessionIDs := make([]SessionID, 0, len(frames))
	for sid := range frames {
		sessionIDs = append(sessionIDs, sid)
	}
	sort.Slice(sessionIDs, func(i, j int) bool { return sessionIDs[i] < sessionIDs[j] })

	for _, sid := range sessionIDs {
		sess := m.session(sid)
		if sess == nil {
			continue
		}
		// Encode against a scratch copy: the clock and the reference table
		// only move once the frames were admitted, so a retried tick sends
		// the same frames again instead of deltas against ticks nobody saw.
		next := sess.clone()
		payloads, err := next.encode(frames[sid], m.wire)
		if err != nil {
			m.loseSession(sid, err)
			continue
		}
		var pushErr error
		for _, payload := range payloads {
			if pushErr = m.config.Transport.Push(ctx, sid, payload); pushErr != nil {
				break
			}
		}
		if errors.Is(pushErr, ErrRetryLater) {
			// Nobody's fault: put everything back and try the whole tick again.
			abortAll(prepared, pushErr)
			for _, id := range ids {
				m.markPending(id)
			}
			m.flushFailures.Add(1)
			m.setLastError(pushErr)
			return pushErr
		}
		if pushErr != nil {
			m.loseSession(sid, pushErr)
			continue
		}
		m.adoptSession(next)
		m.framesAdmitted.Add(uint64(len(payloads)))
		metrics.IncCounter("entitysync_frames_admitted_total", nil, int64(len(payloads)))
		for _, settle := range settlements[sid] {
			settle.subj.mu.Lock()
			if sub, ok := settle.subj.subscribers[sid]; ok {
				if settle.remove {
					delete(settle.subj.subscribers, sid)
				} else {
					sub.kind, sub.baseVersion = kindLive, settle.version
				}
			}
			settle.subj.mu.Unlock()
		}
	}

	if len(prepared) > 0 {
		batch, err := entity.ReservePreparedSubjectSyncBatch(prepared)
		if err != nil {
			abortAll(prepared, err)
			failures = append(failures, err)
		} else if err := batch.Commit(); err != nil {
			failures = append(failures, err)
		}
	}
	for _, id := range ids {
		subj := m.subject(id)
		if subj == nil {
			continue
		}
		subj.mu.Lock()
		done := subj.retiring && len(subj.subscribers) == 0
		subj.mu.Unlock()
		if done {
			m.forget(id)
		}
	}
	for _, id := range retry {
		m.markPending(id)
	}
	err := errors.Join(failures...)
	if err != nil {
		m.flushFailures.Add(1)
		m.setLastError(err)
	}
	return err
}

// capturedAbove reports whether any part of the capture came from a commit
// the durable watermark has not reached.
func capturedAbove(item *entity.PreparedSubjectSync, watermark uint64) bool {
	for _, update := range item.Updates() {
		if update.CommitLSN > watermark {
			return true
		}
	}
	for _, update := range item.Snapshots() {
		if update.CommitLSN > watermark {
			return true
		}
	}
	return false
}

func abortAll(prepared []*entity.PreparedSubjectSync, cause error) {
	for _, item := range prepared {
		_ = item.AbortWithError(cause)
	}
}

// ---- lifecycle ----

// Start runs Flush every Interval until Stop.
func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return ErrManagerClosed
	}
	if m.isClosed() {
		return ErrManagerClosed
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.running {
		return nil
	}
	m.running = true
	m.stopCh = make(chan struct{})
	m.doneCh = make(chan struct{})
	go m.run(m.stopCh, m.doneCh)
	return nil
}

func (m *Manager) run(stopCh, doneCh chan struct{}) {
	defer close(doneCh)
	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			_ = m.Flush(context.Background())
		}
	}
}

// Stop ends the tick loop and flushes once more so nothing admitted before
// the stop is left owed.
func (m *Manager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.runMu.Lock()
	if m.running {
		m.running = false
		close(m.stopCh)
		doneCh := m.doneCh
		m.runMu.Unlock()
		select {
		case <-doneCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		m.runMu.Unlock()
	}
	if m.isClosed() {
		return nil
	}
	err := m.Flush(ctx)
	if errors.Is(err, ErrManagerClosed) {
		return nil
	}
	return err
}

// Close stops the manager and lets go of every subject and session.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	err := m.Stop(ctx)
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return err
	}
	m.closed = true
	subjects := m.subjects
	sessions := m.sessions
	m.subjects = make(map[int64]*subject)
	m.sessions = make(map[SessionID]*session)
	m.mu.Unlock()
	for _, subj := range subjects {
		subj.state.SetDirtyNotifier(nil)
	}
	if lifecycle, ok := m.config.Transport.(SessionLifecycle); ok {
		for id := range sessions {
			lifecycle.SessionClosed(id)
		}
	}
	return err
}

func (m *Manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *Manager) setLastError(err error) {
	m.errMu.Lock()
	m.lastError = err
	m.errMu.Unlock()
}

// LastError is the most recent tick failure.
func (m *Manager) LastError() error {
	if m == nil {
		return nil
	}
	m.errMu.Lock()
	defer m.errMu.Unlock()
	return m.lastError
}

// ---- observability ----

type ManagerStats struct {
	Subjects           int
	Sessions           int
	HeldSessions       int
	Subscriptions      int
	Pending            int
	FramesAdmitted     uint64
	SessionsLost       uint64
	DurabilityDeferred uint64
	FlushFailures      uint64
	MaxSubjects        int
	MaxSessions        int
}

func (m *Manager) Stats() ManagerStats {
	if m == nil {
		return ManagerStats{}
	}
	m.mu.RLock()
	subjects := make([]*subject, 0, len(m.subjects))
	for _, subj := range m.subjects {
		subjects = append(subjects, subj)
	}
	sessions, heldSessions := len(m.sessions), 0
	for _, sess := range m.sessions {
		if sess.held {
			heldSessions++
		}
	}
	m.mu.RUnlock()
	subscriptions := 0
	for _, subj := range subjects {
		subj.mu.Lock()
		subscriptions += len(subj.subscribers)
		subj.mu.Unlock()
	}
	m.pendingMu.Lock()
	pending := len(m.pending)
	m.pendingMu.Unlock()
	return ManagerStats{
		Subjects: len(subjects), Sessions: sessions, HeldSessions: heldSessions, Subscriptions: subscriptions, Pending: pending,
		FramesAdmitted: m.framesAdmitted.Load(), SessionsLost: m.sessionsLost.Load(),
		DurabilityDeferred: m.deferred.Load(), FlushFailures: m.flushFailures.Load(),
		MaxSubjects: m.config.MaxSubjects, MaxSessions: m.config.MaxSessions,
	}
}

// CheckHealth degrades at 80% of either capacity and fails when full or
// closed; the message carries the counts an operator would ask for next.
func (m *Manager) CheckHealth(context.Context) health.Result {
	if m == nil {
		return health.Result{Status: health.StatusFail, Message: "entitysync manager is nil"}
	}
	stats := m.Stats()
	message := fmt.Sprintf("subjects=%d/%d sessions=%d/%d subscriptions=%d pending=%d frames=%d sessions_lost=%d deferred=%d flush_failures=%d",
		stats.Subjects, stats.MaxSubjects, stats.Sessions, stats.MaxSessions, stats.Subscriptions, stats.Pending,
		stats.FramesAdmitted, stats.SessionsLost, stats.DurabilityDeferred, stats.FlushFailures)
	if m.isClosed() {
		return health.Result{Status: health.StatusFail, Message: "closed: " + message}
	}
	if stats.Subjects >= stats.MaxSubjects || stats.Sessions >= stats.MaxSessions {
		return health.Result{Status: health.StatusFail, Message: message}
	}
	if stats.Subjects*10 >= stats.MaxSubjects*8 || stats.Sessions*10 >= stats.MaxSessions*8 {
		return health.Result{Status: health.StatusDegraded, Message: message}
	}
	return health.Result{Status: health.StatusOK, Message: message}
}

var _ health.Checker = (*Manager)(nil)
