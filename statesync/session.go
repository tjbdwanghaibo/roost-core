package statesync

import (
	"bytes"
	"sync"
)

// sequenceNewer reports whether a is strictly newer than b under serial
// number arithmetic (RFC 1982). prepare already skips 0 when the counter
// wraps, so acceptance checks must tolerate wraparound too: a plain a <= b
// comparison would permanently reject every frame of a long-lived session
// once its sequence wraps.
func sequenceNewer(a, b uint32) bool {
	return a != b && int32(a-b) > 0
}

type SessionState struct {
	mu          sync.Mutex
	sendMu      sync.Mutex
	info        SessionInfo
	ackTick     uint32
	lastSent    uint32
	sequence    uint32
	forceFull   bool
	closed      bool
	qualityTier uint8
	sent        map[uint32]Snapshot
	sentOrder   []uint32
	maxHistory  int
	generation  uint64
	committed   uint32
	controlSeq  uint32
	// pinned* is the view this session's current tick was FIRST projected
	// into (U-0205, RR-20260915-02). Every later prepare of the same tick
	// reuses it, so two PreparedFrames of one tick can never carry
	// different views — whichever the transport delivers and whichever
	// commits, the client holds the same bytes the ACK will refer to.
	// U-0200 fixed the view at first commit; that left the window between
	// two prepares and the first commit, where a divergent second frame
	// could already be on the wire. Replaced on the next tick, cleared on
	// Close: one snapshot per session.
	pinnedTick uint32
	pinnedView Snapshot
	pinned     bool
}

func (s *SessionState) handleControl(message ControlMessage, latestPublished uint32) error {
	if s == nil {
		return ErrSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionNotFound
	}
	if !sequenceNewer(message.Sequence, s.controlSeq) {
		return ErrInvalidControl
	}
	s.controlSeq = message.Sequence
	switch message.Type {
	case ControlAck:
		if message.Tick == 0 || message.Tick > latestPublished || message.Tick > s.lastSent {
			return ErrInvalidAck
		}
		if _, ok := s.sent[message.Tick]; !ok {
			return ErrInvalidAck
		}
		if message.Tick > s.ackTick {
			s.ackTick = message.Tick
		}
	case ControlResync:
		s.forceFull = true
		s.generation++
	default:
		return ErrInvalidControl
	}
	return nil
}

func NewSessionState(info SessionInfo) (*SessionState, error) {
	if info.ID == 0 {
		return nil, ErrSessionNotFound
	}
	return &SessionState{info: info, forceFull: true, sent: make(map[uint32]Snapshot), maxHistory: 64}, nil
}

func (s *SessionState) Info() SessionInfo {
	if s == nil {
		return SessionInfo{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *SessionState) Acknowledge(tick, latestPublished uint32) error {
	if s == nil {
		return ErrSessionNotFound
	}
	if tick == 0 || tick > latestPublished {
		return ErrInvalidAck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionNotFound
	}
	if tick > s.lastSent {
		return ErrInvalidAck
	}
	if _, ok := s.sent[tick]; !ok {
		return ErrInvalidAck
	}
	// An ACK moves the watermark and nothing else. It used to clear
	// forceFull too, which let the ACK of a frame sent BEFORE ForceFull was
	// requested cancel the recovery (RR-20260914-11): the client had never
	// seen a full frame, and the next send was a delta again. The intent is
	// released in commitPrepared, by the full frame that belongs to the
	// current generation actually being committed (U-0201).
	if tick > s.ackTick {
		s.ackTick = tick
	}
	return nil
}

func (s *SessionState) ForceFull() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.forceFull = true
		s.generation++
	}
	s.mu.Unlock()
}

func (s *SessionState) SetQualityTier(tier uint8) error {
	if s == nil {
		return ErrSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionNotFound
	}
	if s.qualityTier != tier {
		s.qualityTier = tier
		s.generation++
	}
	return nil
}

func (s *SessionState) QualityTier() uint8 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qualityTier
}

// prepareResult is what one prepare hands to the projection and encoding
// steps. It is a struct rather than a tuple so the frozen-view rule below
// has somewhere to live without an eight-value return.
type prepareResult struct {
	info        SessionInfo
	qualityTier uint8
	base        *Snapshot // delta baseline (nil for a full frame)
	previous    *Snapshot // last committed projection, for rate-limited holds
	frozen      *Snapshot // this tick's already-committed view, if any (U-0200)
	sequence    uint32
	generation  uint64
	fullRefresh bool
}

func (s *SessionState) prepare(targetTick uint32) (prepareResult, error) {
	if s == nil {
		return prepareResult{}, ErrSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return prepareResult{}, ErrSessionNotFound
	}
	s.sequence++
	if s.sequence == 0 {
		s.sequence++
	}
	result := prepareResult{info: s.info, qualityTier: s.qualityTier, sequence: s.sequence, generation: s.generation}
	if latest, ok := s.sent[s.lastSent]; ok {
		clone := latest.Clone()
		result.previous = &clone
	}
	// One tick, one view (U-0200, RR-20260914-10). `sent` is keyed by tick
	// and an ACK names only a tick, so if this tick were projected again
	// (an interest change between two sends of the same tick) the ACK
	// could not say which view the client holds. Re-preparing a committed
	// tick therefore reuses the committed view; whatever changed in the
	// projection lands with the next tick.
	if committed, ok := s.sent[targetTick]; ok {
		clone := committed.Clone()
		result.frozen = &clone
	} else if s.pinned && s.pinnedTick == targetTick {
		clone := s.pinnedView.Clone()
		result.frozen = &clone
	}
	if !s.forceFull && s.ackTick != 0 && s.ackTick < targetTick {
		if base, ok := s.sent[s.ackTick]; ok {
			base = base.Clone()
			result.base = &base
			return result, nil
		}
		s.forceFull = true
	}
	result.fullRefresh = true
	return result, nil
}

func (s *SessionState) commitPrepared(snapshot Snapshot, sequence uint32, generation uint64, full bool) error {
	if s == nil {
		return ErrSessionNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSessionNotFound
	}
	if generation != s.generation || !sequenceNewer(sequence, s.committed) {
		return ErrPreparedFrameStale
	}
	// A tick's view is fixed by its first commit. Two prepares of the same
	// tick that both ran before either committed can carry different views;
	// the later one must not overwrite what the ACK will refer to (U-0200).
	// Same view twice is idempotent.
	if existing, ok := s.sent[snapshot.Tick]; ok && !snapshotEqual(existing, snapshot) {
		// Unreachable through PrepareLatest since U-0205 pins the view at
		// first projection; kept for frames built some other way. If it
		// ever fires, the divergent frame may already have been delivered,
		// so this tick is no longer a baseline anyone can trust: recover
		// with a full frame of a new generation instead of letting a later
		// zero-change delta paper over the split (RR-20260915-02).
		s.forceFull = true
		s.generation++
		return ErrPreparedFrameStale
	}
	s.committed = sequence
	if snapshot.Tick > s.lastSent {
		s.lastSent = snapshot.Tick
	}
	if _, exists := s.sent[snapshot.Tick]; !exists {
		s.sentOrder = append(s.sentOrder, snapshot.Tick)
	}
	s.sent[snapshot.Tick] = snapshot.Clone()
	for len(s.sentOrder) > s.maxHistory {
		oldest := s.sentOrder[0]
		s.sentOrder = s.sentOrder[1:]
		delete(s.sent, oldest)
	}
	if full {
		s.forceFull = false
	}
	return nil
}

// pinView records the first projection of a tick so later prepares of the
// same tick reuse it (see pinned*). Called by prepareLatest under the
// session's sendMu, which serializes prepares per session.
func (s *SessionState) pinView(snapshot Snapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed && (!s.pinned || s.pinnedTick != snapshot.Tick) {
		s.pinnedTick, s.pinnedView, s.pinned = snapshot.Tick, snapshot.Clone(), true
	}
	s.mu.Unlock()
}

func (s *SessionState) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	clear(s.sent)
	s.sentOrder = nil
	s.pinned, s.pinnedView = false, Snapshot{}
	s.mu.Unlock()
}

type SessionSnapshot struct {
	Info        SessionInfo
	AckTick     uint32
	LastSent    uint32
	Sequence    uint32
	ForceFull   bool
	Closed      bool
	QualityTier uint8
}

func (s *SessionState) Snapshot() SessionSnapshot {
	if s == nil {
		return SessionSnapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionSnapshot{
		Info: s.info, AckTick: s.ackTick, LastSent: s.lastSent, Sequence: s.sequence,
		ForceFull: s.forceFull, Closed: s.closed, QualityTier: s.qualityTier,
	}
}

// snapshotEqual reports whether two normalized snapshots describe the same
// view: same meta, same objects (by ref), same archetypes and component
// bytes. Order-independent so it does not depend on how either was built.
func snapshotEqual(a, b Snapshot) bool {
	if a.SnapshotMeta != b.SnapshotMeta || len(a.Objects) != len(b.Objects) {
		return false
	}
	byRef := make(map[ObjectRef]ObjectState, len(b.Objects))
	for _, object := range b.Objects {
		byRef[object.Ref] = object
	}
	for _, object := range a.Objects {
		other, ok := byRef[object.Ref]
		if !ok || other.Archetype != object.Archetype || len(other.Components) != len(object.Components) {
			return false
		}
		byType := make(map[uint16]ComponentState, len(other.Components))
		for _, component := range other.Components {
			byType[component.TypeID] = component
		}
		for _, component := range object.Components {
			match, ok := byType[component.TypeID]
			if !ok || match.SchemaVersion != component.SchemaVersion || !bytes.Equal(match.Data, component.Data) {
				return false
			}
		}
	}
	return true
}
