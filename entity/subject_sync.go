package entity

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	flog "github.com/tjbdwanghaibo/roost-core/log"
)

var (
	ErrSubjectSyncClosed       = errors.New("entity: subject sync state is closed")
	ErrSubjectSyncNotDirty     = errors.New("entity: subject sync state has no pending dirty data")
	ErrSubjectSyncInFlight     = errors.New("entity: subject sync prepare already in flight")
	ErrSubjectSyncPacker       = errors.New("entity: subject sync packer is required")
	ErrSubjectSyncStalePrepare = errors.New("entity: subject sync prepared update is stale")
	ErrSubjectSyncFinished     = errors.New("entity: subject sync prepared update is already finished")
	ErrSubjectSyncBatchInvalid = errors.New("entity: subject sync prepared batch is invalid")
)

const (
	preparedSubjectSyncOpen uint32 = iota
	preparedSubjectSyncFinished
	preparedSubjectSyncReserved
)

const SyncMaskFull uint64 = ^uint64(0)

// subjectSyncInFlightStaleAfter bounds how long an unfinished prepare may
// block new prepares. A PreparedSubjectSync whose owner panicked or discarded
// it without Commit/Abort would otherwise hold the in-flight token forever
// and permanently stall the subject's sync. Reclaiming is safe because Commit
// and Abort validate token and base version and turn into stale errors after
// the token is superseded.
const subjectSyncInFlightStaleAfter = 30 * time.Second

const (
	SyncFullReasonNone uint32 = iota
	SyncFullReasonDirty
	SyncFullReasonResync
	SyncFullReasonSchema
)

// SyncProfile identifies a finite state view such as an LOD, permission, or
// faction projection. It deliberately contains no subscriber identity.
type SyncProfile struct {
	Key           string
	LOD           uint8
	SchemaVersion uint32
}

func (p SyncProfile) Normalize() SyncProfile {
	if p.Key == "" {
		p.Key = "default"
	}
	return p
}

// FrozenSyncPayload owns immutable bytes. CopyFrozenSyncPayload copies caller
// memory. TakeFrozenSyncPayload transfers ownership: the caller must never
// mutate the supplied slice after the call.
type FrozenSyncPayload struct {
	codec uint16
	data  []byte
}

func CopyFrozenSyncPayload(codec uint16, data []byte) FrozenSyncPayload {
	return FrozenSyncPayload{codec: codec, data: bytes.Clone(data)}
}

func TakeFrozenSyncPayload(codec uint16, owned []byte) FrozenSyncPayload {
	return FrozenSyncPayload{codec: codec, data: owned}
}

func (p FrozenSyncPayload) Codec() uint16 { return p.codec }

func (p FrozenSyncPayload) Len() int { return len(p.data) }

func (p FrozenSyncPayload) Empty() bool { return len(p.data) == 0 }

func (p FrozenSyncPayload) BytesCopy() []byte { return bytes.Clone(p.data) }

func (p FrozenSyncPayload) AppendTo(dst []byte) []byte { return append(dst, p.data...) }

func (p FrozenSyncPayload) Equal(other FrozenSyncPayload) bool {
	return p.codec == other.codec && bytes.Equal(p.data, other.data)
}

// SubjectSyncPacker serializes entity state by profile. Implementations run
// while the owning Entity mutex is held. Returned payload ownership must have
// been transferred with TakeFrozenSyncPayload or copied explicitly.
type SubjectSyncPacker interface {
	PackSubjectSnapshot(SyncProfile) (FrozenSyncPayload, error)
	PackSubjectDelta(SyncProfile, uint64) (FrozenSyncPayload, error)
}

type SubjectSyncPackFunc struct {
	Snapshot func(SyncProfile) (FrozenSyncPayload, error)
	Delta    func(SyncProfile, uint64) (FrozenSyncPayload, error)
}

// SubjectSyncDirtyNotifier is installed by a framework scheduler. It carries
// only content state and deliberately contains no observer/subscriber data.
type SubjectSyncDirtyNotifier func(*SubjectSyncState)

func (f SubjectSyncPackFunc) PackSubjectSnapshot(profile SyncProfile) (FrozenSyncPayload, error) {
	if f.Snapshot == nil {
		return FrozenSyncPayload{}, nil
	}
	return f.Snapshot(profile.Normalize())
}

func (f SubjectSyncPackFunc) PackSubjectDelta(profile SyncProfile, mask uint64) (FrozenSyncPayload, error) {
	if f.Delta == nil {
		return FrozenSyncPayload{}, nil
	}
	return f.Delta(profile.Normalize(), mask)
}

type SubjectSyncCreateParam struct {
	Enabled     bool
	SubjectID   int64
	Namespace   string
	SubjectKind uint32
	Packer      SubjectSyncPacker
}

// EntitySyncCreateParam is the single entity replication configuration. The
// state contains content only; subscriber membership and delivery live in the
// entitysync coordinator.
type EntitySyncCreateParam struct {
	Enabled    bool
	EntityID   int64
	Topic      string
	EntityKind uint32
	Packer     SubjectSyncPacker
}

type EntitySyncBuilderParam struct {
	Enabled       bool
	Topic         string
	PackerFactory func(IThreadSafeEntity) SubjectSyncPacker
}

func (p EntitySyncBuilderParam) toCreateParam(e IThreadSafeEntity) EntitySyncCreateParam {
	ret := EntitySyncCreateParam{Enabled: p.Enabled, Topic: p.Topic}
	if e != nil {
		ret.EntityID = e.ID()
		ret.EntityKind = uint32(e.GetEntityKind())
	}
	if p.PackerFactory != nil {
		ret.Packer = p.PackerFactory(e)
	}
	return ret
}

type SubjectSyncUpdate struct {
	SubjectID   int64
	Namespace   string
	SubjectKind uint32
	Profile     SyncProfile
	Version     uint64
	BaseVersion uint64
	Mask        uint64
	Full        bool
	Reason      uint32
	Payload     FrozenSyncPayload
}

type subjectSyncLockFunc func(func() error) error

// SubjectSyncState is the observer-free content state machine. Subscriber
// membership, session sequence, routing, and history live in entitysync/kit.
type SubjectSyncState struct {
	mu        sync.Mutex
	prepareMu sync.Mutex

	enabled         bool
	subjectID       int64
	namespace       string
	subjectKind     uint32
	version         uint64
	dirtyMask       uint64
	fullDirty       bool
	fullReason      uint32
	dirtyGeneration uint64
	packer          SubjectSyncPacker
	lockEntity      subjectSyncLockFunc
	dirtyNotifier   SubjectSyncDirtyNotifier
	nextToken       uint64
	inflightToken   uint64
	inflightSince   time.Time
	lastError       error
	// lastCommitLSN mirrors EntityBase.lastCommitLSN so the subscription
	// coordinator can gate distribution on the durable watermark without a
	// back-reference to the entity. Atomic: written under the entity lock,
	// read by flush timers.
	lastCommitLSN atomic.Uint64
}

func NewSubjectSyncState(param SubjectSyncCreateParam) *SubjectSyncState {
	if !param.Enabled {
		return nil
	}
	return &SubjectSyncState{
		enabled:     true,
		subjectID:   param.SubjectID,
		namespace:   param.Namespace,
		subjectKind: param.SubjectKind,
		packer:      param.Packer,
	}
}

func (s *SubjectSyncState) setEntityLock(lockEntity subjectSyncLockFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lockEntity = lockEntity
	s.mu.Unlock()
}

func (s *SubjectSyncState) withEntityLock(fn func() error) error {
	if fn == nil {
		return nil
	}
	s.mu.Lock()
	lockEntity := s.lockEntity
	s.mu.Unlock()
	if lockEntity == nil {
		return fn()
	}
	return lockEntity(fn)
}

func (s *SubjectSyncState) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func (s *SubjectSyncState) SubjectID() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subjectID
}

func (s *SubjectSyncState) Namespace() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namespace
}

func (s *SubjectSyncState) SubjectKind() uint32 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subjectKind
}

func (s *SubjectSyncState) Version() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *SubjectSyncState) PendingDirty() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirtyMask != 0 || s.fullDirty
}

func (s *SubjectSyncState) LastError() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastError
}

func (s *SubjectSyncState) SetPacker(packer SubjectSyncPacker) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.packer = packer
	s.mu.Unlock()
}

func (s *SubjectSyncState) SetDirtyNotifier(notifier SubjectSyncDirtyNotifier) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.enabled {
		s.dirtyNotifier = notifier
	}
	pending := s.enabled && (s.dirtyMask != 0 || s.fullDirty)
	s.mu.Unlock()
	if pending && notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func (s *SubjectSyncState) MarkDirty(mask uint64) {
	if s == nil || mask == 0 {
		return
	}
	s.mu.Lock()
	var notifier SubjectSyncDirtyNotifier
	if s.enabled {
		s.dirtyMask |= mask
		s.dirtyGeneration++
		notifier = s.dirtyNotifier
	}
	s.mu.Unlock()
	if notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func (s *SubjectSyncState) MarkFullDirty(reason uint32) {
	if s == nil {
		return
	}
	if reason == SyncFullReasonNone {
		reason = SyncFullReasonDirty
	}
	s.mu.Lock()
	var notifier SubjectSyncDirtyNotifier
	if s.enabled {
		s.fullDirty = true
		s.fullReason = reason
		s.dirtyMask = 0
		s.dirtyGeneration++
		notifier = s.dirtyNotifier
	}
	s.mu.Unlock()
	if notifier != nil {
		notifySubjectSyncDirty(notifier, s)
	}
}

func notifySubjectSyncDirty(notifier SubjectSyncDirtyNotifier, state *SubjectSyncState) {
	defer func() {
		_ = recover()
	}()
	notifier(state)
}

func entitySyncLockedInCurrentGuard(entityID int64) bool {
	if entityID == 0 {
		return false
	}
	scope := CurrentGuardScope()
	if scope == nil || scope.Guard() == nil {
		return false
	}
	ok := scope.Guard().Guarded(entityID)
	return ok
}

// Prepare captures one content version for every requested profile. It takes
// the Entity mutex before the prepare serialization lock, so calls made from an
// EntityGuard cannot deadlock with an asynchronous prepare waiting for Entity.
func (s *SubjectSyncState) Prepare(profiles []SyncProfile) (*PreparedSubjectSync, error) {
	if s == nil {
		return nil, ErrSubjectSyncClosed
	}
	profiles = normalizeSyncProfiles(profiles)
	var prepared *PreparedSubjectSync
	err := s.withEntityLock(func() error {
		s.prepareMu.Lock()
		defer s.prepareMu.Unlock()
		var err error
		prepared, err = s.prepareLocked(profiles)
		return err
	})
	return prepared, err
}

func (s *SubjectSyncState) prepareLocked(profiles []SyncProfile) (*PreparedSubjectSync, error) {
	s.mu.Lock()
	if !s.enabled {
		s.mu.Unlock()
		return nil, ErrSubjectSyncClosed
	}
	if s.inflightToken != 0 {
		if s.inflightSince.IsZero() || time.Since(s.inflightSince) < subjectSyncInFlightStaleAfter {
			s.mu.Unlock()
			return nil, ErrSubjectSyncInFlight
		}
		// Escape hatch: the previous prepare was abandoned without
		// Commit/Abort. Reclaim so the subject does not stall forever; a
		// late Commit of the abandoned token fails its token check.
		flog.Error("entity: reclaiming abandoned subject sync prepare",
			"subject", s.subjectID, "namespace", s.namespace,
			"token", s.inflightToken, "age", time.Since(s.inflightSince))
		s.inflightToken = 0
		s.inflightSince = time.Time{}
	}
	if s.dirtyMask == 0 && !s.fullDirty {
		s.mu.Unlock()
		return nil, ErrSubjectSyncNotDirty
	}
	if s.packer == nil {
		s.lastError = ErrSubjectSyncPacker
		s.mu.Unlock()
		return nil, ErrSubjectSyncPacker
	}
	s.nextToken++
	token := s.nextToken
	s.inflightToken = token
	s.inflightSince = time.Now()
	packer := s.packer
	subjectID := s.subjectID
	namespace := s.namespace
	subjectKind := s.subjectKind
	baseVersion := s.version
	version := baseVersion + 1
	mask := s.dirtyMask
	full := s.fullDirty
	reason := s.fullReason
	if full && reason == SyncFullReasonNone {
		reason = SyncFullReasonDirty
	}
	generation := s.dirtyGeneration
	s.mu.Unlock()

	updates := make([]SubjectSyncUpdate, 0, len(profiles))
	for _, profile := range profiles {
		var payload FrozenSyncPayload
		var err error
		if full {
			payload, err = packer.PackSubjectSnapshot(profile)
		} else {
			payload, err = packer.PackSubjectDelta(profile, mask)
		}
		if err != nil {
			s.failPrepare(token, err)
			return nil, fmt.Errorf("entity: pack subject %d profile %q: %w", subjectID, profile.Key, err)
		}
		updates = append(updates, SubjectSyncUpdate{
			SubjectID: subjectID, Namespace: namespace, SubjectKind: subjectKind,
			Profile: profile, Version: version,
			BaseVersion: baseVersion, Mask: mask, Full: full, Reason: reason,
			Payload: payload,
		})
	}

	return &PreparedSubjectSync{
		state: s, token: token, generation: generation, baseVersion: baseVersion,
		version: version, updates: updates,
	}, nil
}

// CaptureSnapshot creates subscriber-independent full payloads at the current
// content version. It never advances version and never consumes dirty state.
func (s *SubjectSyncState) CaptureSnapshot(profiles []SyncProfile, reason uint32) ([]SubjectSyncUpdate, error) {
	if s == nil {
		return nil, ErrSubjectSyncClosed
	}
	profiles = normalizeSyncProfiles(profiles)
	if reason == SyncFullReasonNone {
		reason = SyncFullReasonResync
	}
	var updates []SubjectSyncUpdate
	err := s.withEntityLock(func() error {
		s.prepareMu.Lock()
		defer s.prepareMu.Unlock()
		s.mu.Lock()
		if !s.enabled {
			s.mu.Unlock()
			return ErrSubjectSyncClosed
		}
		packer := s.packer
		subjectID := s.subjectID
		namespace := s.namespace
		subjectKind := s.subjectKind
		version := s.version
		s.mu.Unlock()
		if packer == nil {
			return ErrSubjectSyncPacker
		}
		updates = make([]SubjectSyncUpdate, 0, len(profiles))
		for _, profile := range profiles {
			payload, err := packer.PackSubjectSnapshot(profile)
			if err != nil {
				return fmt.Errorf("entity: snapshot subject %d profile %q: %w", subjectID, profile.Key, err)
			}
			updates = append(updates, SubjectSyncUpdate{
				SubjectID: subjectID, Namespace: namespace, SubjectKind: subjectKind,
				Profile: profile, Version: version,
				BaseVersion: version, Full: true, Reason: reason, Payload: payload,
			})
		}
		return nil
	})
	if err != nil {
		s.setSubjectSyncError(err)
		return nil, err
	}
	s.setSubjectSyncError(nil)
	return updates, nil
}

// SetLastCommitLSN mirrors EntityBase.SetLastCommitLSN for the sync state.
// Callers normally go through the entity; this is exported for hosts that
// manage subject sync states without an EntityBase.
func (s *SubjectSyncState) SetLastCommitLSN(lsn uint64) {
	if s == nil {
		return
	}
	s.lastCommitLSN.Store(lsn)
}

// LastCommitLSN returns the newest pipelined-commit LSN of the owning entity,
// or zero when no pipelined transaction touched it. Distribution must not run
// while this is above the committer's durable watermark.
func (s *SubjectSyncState) LastCommitLSN() uint64 {
	if s == nil {
		return 0
	}
	return s.lastCommitLSN.Load()
}

func (s *SubjectSyncState) failPrepare(token uint64, err error) {
	s.mu.Lock()
	if s.inflightToken == token {
		s.inflightToken = 0
		s.inflightSince = time.Time{}
	}
	s.lastError = err
	s.mu.Unlock()
}

func (s *SubjectSyncState) setSubjectSyncError(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastError = err
	s.mu.Unlock()
}

func (s *SubjectSyncState) Close() {
	if s == nil {
		return
	}
	s.prepareMu.Lock()
	s.mu.Lock()
	s.enabled = false
	s.dirtyMask = 0
	s.fullDirty = false
	s.fullReason = SyncFullReasonNone
	s.packer = nil
	s.dirtyNotifier = nil
	s.mu.Unlock()
	s.prepareMu.Unlock()
}

type PreparedSubjectSync struct {
	state       *SubjectSyncState
	token       uint64
	generation  uint64
	baseVersion uint64
	version     uint64
	updates     []SubjectSyncUpdate
	finished    atomic.Uint32
}

func (p *PreparedSubjectSync) Version() uint64 {
	if p == nil {
		return 0
	}
	return p.version
}

func (p *PreparedSubjectSync) Updates() []SubjectSyncUpdate {
	if p == nil {
		return nil
	}
	return append([]SubjectSyncUpdate(nil), p.updates...)
}

func (p *PreparedSubjectSync) Commit() error {
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{p})
	if err != nil {
		return err
	}
	return batch.Commit()
}

func (p *PreparedSubjectSync) Abort() error { return p.AbortWithError(nil) }

func (p *PreparedSubjectSync) AbortWithError(cause error) error {
	batch, err := ReservePreparedSubjectSyncBatch([]*PreparedSubjectSync{p})
	if err != nil {
		return err
	}
	return batch.AbortWithError(cause)
}

// PreparedSubjectSyncBatch reserves a set of prepared subjects before a
// downstream admission. Reservation prevents any caller from independently
// committing or aborting one member while the batch is in flight. Commit and
// AbortWithError update every SubjectSyncState while holding all state locks,
// so observers cannot observe or create a partially committed batch.
type PreparedSubjectSyncBatch struct {
	items    []*PreparedSubjectSync
	finished atomic.Uint32
}

// ReservePreparedSubjectSyncBatch transfers completion ownership for every
// prepared item to one batch. Callers must invoke Commit after successful
// durable admission or AbortWithError after failed admission.
func ReservePreparedSubjectSyncBatch(items []*PreparedSubjectSync) (*PreparedSubjectSyncBatch, error) {
	if len(items) == 0 {
		return nil, ErrSubjectSyncBatchInvalid
	}
	ordered := append([]*PreparedSubjectSync(nil), items...)
	seen := make(map[*SubjectSyncState]struct{}, len(ordered))
	for _, item := range ordered {
		if item == nil || item.state == nil {
			return nil, ErrSubjectSyncBatchInvalid
		}
		if _, duplicate := seen[item.state]; duplicate {
			return nil, ErrSubjectSyncBatchInvalid
		}
		seen[item.state] = struct{}{}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.state.subjectID != right.state.subjectID {
			return left.state.subjectID < right.state.subjectID
		}
		return left.token < right.token
	})
	reserved := 0
	for _, item := range ordered {
		if !item.finished.CompareAndSwap(preparedSubjectSyncOpen, preparedSubjectSyncReserved) {
			for i := 0; i < reserved; i++ {
				ordered[i].finished.CompareAndSwap(preparedSubjectSyncReserved, preparedSubjectSyncOpen)
			}
			return nil, ErrSubjectSyncFinished
		}
		reserved++
	}
	for _, item := range ordered {
		state := item.state
		state.mu.Lock()
		valid := state.enabled && state.inflightToken == item.token && state.version == item.baseVersion
		state.mu.Unlock()
		if !valid {
			for _, rollback := range ordered {
				rollback.finished.CompareAndSwap(preparedSubjectSyncReserved, preparedSubjectSyncOpen)
			}
			return nil, ErrSubjectSyncStalePrepare
		}
	}
	return &PreparedSubjectSyncBatch{items: ordered}, nil
}

// Commit atomically advances all reserved content versions.
func (b *PreparedSubjectSyncBatch) Commit() error {
	if b == nil || len(b.items) == 0 {
		return ErrSubjectSyncBatchInvalid
	}
	if !b.finished.CompareAndSwap(0, 1) {
		return ErrSubjectSyncFinished
	}
	for _, item := range b.items {
		item.state.mu.Lock()
	}
	defer func() {
		for i := len(b.items) - 1; i >= 0; i-- {
			b.items[i].state.mu.Unlock()
		}
	}()
	for _, item := range b.items {
		state := item.state
		if item.finished.Load() != preparedSubjectSyncReserved || state.inflightToken != item.token || state.version != item.baseVersion {
			b.abortLocked(ErrSubjectSyncStalePrepare)
			return ErrSubjectSyncStalePrepare
		}
	}
	for _, item := range b.items {
		state := item.state
		state.version = item.version
		if state.dirtyGeneration == item.generation {
			state.dirtyMask = 0
			state.fullDirty = false
			state.fullReason = SyncFullReasonNone
		}
		state.inflightToken = 0
		state.inflightSince = time.Time{}
		state.lastError = nil
		item.finished.Store(preparedSubjectSyncFinished)
	}
	return nil
}

func (b *PreparedSubjectSyncBatch) Abort() error { return b.AbortWithError(nil) }

// AbortWithError releases every reservation without consuming dirty state.
func (b *PreparedSubjectSyncBatch) AbortWithError(cause error) error {
	if b == nil || len(b.items) == 0 {
		return ErrSubjectSyncBatchInvalid
	}
	if !b.finished.CompareAndSwap(0, 1) {
		return ErrSubjectSyncFinished
	}
	for _, item := range b.items {
		item.state.mu.Lock()
	}
	defer func() {
		for i := len(b.items) - 1; i >= 0; i-- {
			b.items[i].state.mu.Unlock()
		}
	}()
	b.abortLocked(cause)
	return nil
}

func (b *PreparedSubjectSyncBatch) abortLocked(cause error) {
	for _, item := range b.items {
		state := item.state
		if state.inflightToken == item.token {
			state.inflightToken = 0
			state.inflightSince = time.Time{}
			state.lastError = cause
		}
		item.finished.Store(preparedSubjectSyncFinished)
	}
}

func normalizeSyncProfiles(profiles []SyncProfile) []SyncProfile {
	if len(profiles) == 0 {
		profiles = []SyncProfile{{Key: "default"}}
	}
	unique := make(map[SyncProfile]struct{}, len(profiles))
	for _, profile := range profiles {
		unique[profile.Normalize()] = struct{}{}
	}
	out := make([]SyncProfile, 0, len(unique))
	for profile := range unique {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		if out[i].LOD != out[j].LOD {
			return out[i].LOD < out[j].LOD
		}
		return out[i].SchemaVersion < out[j].SchemaVersion
	})
	return out
}
