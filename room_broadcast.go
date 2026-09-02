package room

import (
	"context"
	"errors"
	"fmt"
	"sort"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/cube-core/entitysync"
)

var (
	ErrRoomIDInvalid             = errors.New("room: room id is invalid")
	ErrRoomFrameSinkRequired     = errors.New("room: reliable room frame sink is required")
	ErrRoomSubjectInvalid        = errors.New("room: room subject is invalid")
	ErrRoomSubjectNotRegistered  = errors.New("room: room subject is not registered")
	ErrRoomSubjectAlreadyExists  = errors.New("room: room subject is already registered")
	ErrRoomSubjectHasSubscribers = errors.New("room: room subject still has subscribers")
	ErrRoomEnvelopeInvalid       = errors.New("room: room delivery envelope is invalid")
	ErrRoomFrameAdmission        = errors.New("room: room frame admission failed")
	ErrRoomBroadcasterStopped    = errors.New("room: broadcaster is stopped")
	ErrRoomSubjectRetiring       = errors.New("room: room subject is retiring")
	ErrRoomSubjectLimit          = errors.New("room: room subject limit exceeded")
	ErrRoomSubscriberLimit       = errors.New("room: room subscriber limit exceeded")
	ErrRoomGlobalSubjectLimit    = errors.New("room: global room subject limit exceeded")
	ErrRoomGlobalSubscriberLimit = errors.New("room: global room subscriber limit exceeded")
)

const DefaultRoomBroadcastInterval = 50 * time.Millisecond

// RoomFrameEntry is one subject update in a receiver-specific room frame.
// Payload bytes are immutable and may be shared by every receiver using the
// same profile.
type RoomFrameEntry struct {
	Kind   coreentitysync.EnvelopeKind
	Update entity.SubjectSyncUpdate
}

// RoomFrame is the transport-neutral unit admitted by the room replication
// layer. Frame is monotonic within a room. SessionSequence is monotonic for a
// (room, subscriber) stream and is suitable for loss detection and replay.
type RoomFrame struct {
	RoomID          int64
	Frame           uint64
	Subscriber      coreentitysync.SubscriberRef
	SessionSequence uint64
	Entries         []RoomFrameEntry
}

// ReliableRoomFrameSink must atomically admit the complete slice. Returning
// nil transfers responsibility for buffering, persistence/history and network
// delivery to the sink. On error it must retain none of the frames.
type ReliableRoomFrameSink interface {
	AdmitRoomFrames(context.Context, []RoomFrame) error
}

// RoomSubjectLifecycle lets a transport release compact per-room object IDs
// only after the subject has no subscribers and has been unregistered.
type RoomSubjectLifecycle interface {
	ReleaseRoomSubject(roomID, subjectID int64)
}

type RoomSubscriberLifecycle interface {
	ReleaseRoomSubscriber(context.Context, int64, coreentitysync.SubscriberRef)
}

type RoomLifecycle interface {
	ResetRoom(int64)
}

type ReliableRoomFrameSinkFunc func(context.Context, []RoomFrame) error

func (f ReliableRoomFrameSinkFunc) AdmitRoomFrames(ctx context.Context, frames []RoomFrame) error {
	if f == nil {
		return ErrRoomFrameSinkRequired
	}
	return f(ctx, frames)
}

type roomSubscriberKey struct {
	roomID     int64
	subscriber coreentitysync.SubscriberRef
}

type roomFrameGroupKey struct {
	roomID     int64
	subscriber coreentitysync.SubscriberRef
}

// RoomEnvelopeSink adapts core subscription envelopes into room frames. An
// admission is serialized so frame/session counters can be committed only
// after the downstream sink has accepted the complete batch.
type RoomEnvelopeSink struct {
	admitMu stdsync.Mutex
	mu      stdsync.RWMutex

	downstream       ReliableRoomFrameSink
	subjectRooms     map[int64]int64
	roomFrames       map[int64]uint64
	sessionSequences map[roomSubscriberKey]uint64

	admittedFrames  atomic.Uint64
	admittedEntries atomic.Uint64
	failedBatches   atomic.Uint64
}

type RoomBroadcasterStats struct {
	AdmittedFrames     uint64
	AdmittedEntries    uint64
	FailedBatches      uint64
	FlushFailures      uint64
	PendingSubjects    int
	PendingRetirements int
	LastError          string
	ActiveSubjects     int
	ActiveSubscribers  int
	SessionSequences   int
}

func NewRoomEnvelopeSink(downstream ReliableRoomFrameSink) *RoomEnvelopeSink {
	return &RoomEnvelopeSink{
		downstream:       downstream,
		subjectRooms:     make(map[int64]int64),
		roomFrames:       make(map[int64]uint64),
		sessionSequences: make(map[roomSubscriberKey]uint64),
	}
}

func (s *RoomEnvelopeSink) SetDownstream(downstream ReliableRoomFrameSink) {
	if s == nil {
		return
	}
	s.admitMu.Lock()
	s.mu.Lock()
	s.downstream = downstream
	s.mu.Unlock()
	s.admitMu.Unlock()
}

func (s *RoomEnvelopeSink) RegisterSubject(roomID, subjectID int64) error {
	if s == nil || roomID == 0 {
		return ErrRoomIDInvalid
	}
	if subjectID == 0 {
		return ErrRoomSubjectInvalid
	}
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.subjectRooms[subjectID]; ok {
		if existing == roomID {
			return nil
		}
		return fmt.Errorf("%w: subject %d belongs to room %d", ErrRoomSubjectAlreadyExists, subjectID, existing)
	}
	s.subjectRooms[subjectID] = roomID
	return nil
}

func (s *RoomEnvelopeSink) UnregisterSubject(roomID, subjectID int64) error {
	if s == nil || roomID == 0 {
		return ErrRoomIDInvalid
	}
	if subjectID == 0 {
		return ErrRoomSubjectInvalid
	}
	s.admitMu.Lock()
	defer s.admitMu.Unlock()
	s.mu.Lock()
	existing, ok := s.subjectRooms[subjectID]
	if !ok || existing != roomID {
		s.mu.Unlock()
		return ErrRoomSubjectNotRegistered
	}
	delete(s.subjectRooms, subjectID)
	downstream := s.downstream
	s.mu.Unlock()
	if lifecycle, ok := downstream.(RoomSubjectLifecycle); ok {
		lifecycle.ReleaseRoomSubject(roomID, subjectID)
	}
	return nil
}

func (s *RoomEnvelopeSink) Stats() RoomBroadcasterStats {
	if s == nil {
		return RoomBroadcasterStats{}
	}
	s.mu.RLock()
	sequences := len(s.sessionSequences)
	s.mu.RUnlock()
	return RoomBroadcasterStats{
		AdmittedFrames:   s.admittedFrames.Load(),
		AdmittedEntries:  s.admittedEntries.Load(),
		FailedBatches:    s.failedBatches.Load(),
		SessionSequences: sequences,
	}
}

func (s *RoomEnvelopeSink) ReleaseSubscriber(ctx context.Context, roomID int64, subscriber coreentitysync.SubscriberRef) {
	if s == nil || roomID == 0 || subscriber.Normalize().Empty() {
		return
	}
	subscriber = subscriber.Normalize()
	s.admitMu.Lock()
	s.mu.Lock()
	delete(s.sessionSequences, roomSubscriberKey{roomID: roomID, subscriber: subscriber})
	downstream := s.downstream
	s.mu.Unlock()
	s.admitMu.Unlock()
	if lifecycle, ok := downstream.(RoomSubscriberLifecycle); ok {
		lifecycle.ReleaseRoomSubscriber(ctx, roomID, subscriber)
	}
}

func (s *RoomEnvelopeSink) ResetRoom(roomID int64) {
	if s == nil || roomID == 0 {
		return
	}
	s.admitMu.Lock()
	s.mu.Lock()
	delete(s.roomFrames, roomID)
	for key := range s.sessionSequences {
		if key.roomID == roomID {
			delete(s.sessionSequences, key)
		}
	}
	downstream := s.downstream
	s.mu.Unlock()
	s.admitMu.Unlock()
	if lifecycle, ok := downstream.(RoomLifecycle); ok {
		lifecycle.ResetRoom(roomID)
	}
}

func (s *RoomEnvelopeSink) AdmitEnvelopes(ctx context.Context, envelopes []coreentitysync.DeliveryEnvelope) error {
	if s == nil {
		return ErrRoomFrameSinkRequired
	}
	if len(envelopes) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.admitMu.Lock()
	defer s.admitMu.Unlock()

	s.mu.RLock()
	downstream := s.downstream
	groups := make(map[roomFrameGroupKey][]RoomFrameEntry)
	for _, envelope := range envelopes {
		subscriber := envelope.Subscriber.Normalize()
		roomID, registered := s.subjectRooms[envelope.Update.SubjectID]
		if subscriber.Empty() || envelope.Update.SubjectID == 0 || !registered {
			s.mu.RUnlock()
			return fmt.Errorf("%w: subject=%d subscriber=%+v", ErrRoomEnvelopeInvalid, envelope.Update.SubjectID, subscriber)
		}
		key := roomFrameGroupKey{roomID: roomID, subscriber: subscriber}
		groups[key] = append(groups[key], RoomFrameEntry{Kind: envelope.Kind, Update: envelope.Update})
	}
	if downstream == nil {
		s.mu.RUnlock()
		return ErrRoomFrameSinkRequired
	}

	keys := make([]roomFrameGroupKey, 0, len(groups))
	roomNext := make(map[int64]uint64)
	sessionNext := make(map[roomSubscriberKey]uint64, len(groups))
	for key := range groups {
		keys = append(keys, key)
		if _, ok := roomNext[key.roomID]; !ok {
			roomNext[key.roomID] = s.roomFrames[key.roomID] + 1
		}
		sessionKey := roomSubscriberKey{roomID: key.roomID, subscriber: key.subscriber}
		sessionNext[sessionKey] = s.sessionSequences[sessionKey] + 1
	}
	s.mu.RUnlock()

	sort.Slice(keys, func(i, j int) bool { return lessRoomFrameGroupKey(keys[i], keys[j]) })
	frames := make([]RoomFrame, 0, len(keys))
	entryCount := 0
	for _, key := range keys {
		entries := groups[key]
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].Update.SubjectID != entries[j].Update.SubjectID {
				return entries[i].Update.SubjectID < entries[j].Update.SubjectID
			}
			return entries[i].Kind < entries[j].Kind
		})
		sessionKey := roomSubscriberKey{roomID: key.roomID, subscriber: key.subscriber}
		frames = append(frames, RoomFrame{
			RoomID: key.roomID, Frame: roomNext[key.roomID], Subscriber: key.subscriber,
			SessionSequence: sessionNext[sessionKey], Entries: entries,
		})
		entryCount += len(entries)
	}

	if err := admitRoomFrames(ctx, downstream, frames); err != nil {
		s.failedBatches.Add(1)
		return errors.Join(ErrRoomFrameAdmission, err)
	}

	s.mu.Lock()
	for roomID, frame := range roomNext {
		s.roomFrames[roomID] = frame
	}
	for key, sequence := range sessionNext {
		s.sessionSequences[key] = sequence
	}
	s.mu.Unlock()
	s.admittedFrames.Add(uint64(len(frames)))
	s.admittedEntries.Add(uint64(entryCount))
	return nil
}

func lessRoomFrameGroupKey(a, b roomFrameGroupKey) bool {
	if a.roomID != b.roomID {
		return a.roomID < b.roomID
	}
	left := a.subscriber.Normalize()
	right := b.subscriber.Normalize()
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.ID != right.ID {
		return left.ID < right.ID
	}
	if left.Sid != right.Sid {
		return left.Sid < right.Sid
	}
	return left.Key < right.Key
}

func admitRoomFrames(ctx context.Context, sink ReliableRoomFrameSink, frames []RoomFrame) (err error) {
	if sink == nil {
		return ErrRoomFrameSinkRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: sink panic: %v", ErrRoomFrameAdmission, recovered)
		}
	}()
	return sink.AdmitRoomFrames(ctx, frames)
}

// RoomBroadcaster is the application-facing facade for one room. Application
// code registers entity content state, then only subscribes, unsubscribes and
// flushes; it never owns entity locks or observer collections.
type RoomBroadcaster struct {
	roomID       int64
	envelopeSink *RoomEnvelopeSink
	coordinator  *coreentitysync.SubscriptionCoordinator

	mu             stdsync.RWMutex
	lifecycleMu    stdsync.RWMutex
	subjects       map[int64]*entity.SubjectSyncState
	subscribers    map[coreentitysync.SubscriberRef]int
	maxSubjects    int
	maxSubscribers int
	started        bool
	stopped        bool
	stopCh         chan struct{}
	doneCh         chan struct{}
	lastError      error

	dirtyMu  stdsync.Mutex
	dirty    map[int64]struct{}
	retiring map[int64]struct{}

	flushOps               [64]stdsync.Mutex
	flushFailures          atomic.Uint64
	unregisterSlowConsumer func()
	budget                 *roomResourceBudget
	onActivity             func()
	closeMu                stdsync.Mutex
	closed                 bool
}

type RoomBroadcasterConfig struct {
	MaxSubjects    int
	MaxSubscribers int
	budget         *roomResourceBudget
	onActivity     func()
}

func NewRoomBroadcaster(roomID int64, downstream ReliableRoomFrameSink, configs ...RoomBroadcasterConfig) (*RoomBroadcaster, error) {
	if roomID == 0 {
		return nil, ErrRoomIDInvalid
	}
	if downstream == nil {
		return nil, ErrRoomFrameSinkRequired
	}
	envelopeSink := NewRoomEnvelopeSink(downstream)
	config := RoomBroadcasterConfig{MaxSubjects: 100, MaxSubscribers: 100}
	if len(configs) > 0 {
		config.budget = configs[0].budget
		config.onActivity = configs[0].onActivity
		if configs[0].MaxSubjects > 0 {
			config.MaxSubjects = configs[0].MaxSubjects
		}
		if configs[0].MaxSubscribers > 0 {
			config.MaxSubscribers = configs[0].MaxSubscribers
		}
	}
	replication := &RoomBroadcaster{
		roomID: roomID, envelopeSink: envelopeSink,
		coordinator: coreentitysync.NewSubscriptionCoordinator(envelopeSink),
		subjects:    make(map[int64]*entity.SubjectSyncState),
		subscribers: make(map[coreentitysync.SubscriberRef]int),
		maxSubjects: config.MaxSubjects, maxSubscribers: config.MaxSubscribers,
		dirty:    make(map[int64]struct{}),
		retiring: make(map[int64]struct{}),
		budget:   config.budget, onActivity: config.onActivity,
	}
	if lifecycle, ok := downstream.(interface {
		RegisterRoomSlowConsumerHandler(int64, func(context.Context, RoomSlowConsumer)) (func(), error)
	}); ok {
		unregister, err := lifecycle.RegisterRoomSlowConsumerHandler(roomID, replication.handleSlowConsumer)
		if err != nil {
			return nil, err
		}
		replication.unregisterSlowConsumer = unregister
	}
	return replication, nil
}

func (r *RoomBroadcaster) handleSlowConsumer(ctx context.Context, event RoomSlowConsumer) {
	if r == nil || event.RoomID != r.roomID || event.Subscriber.Normalize().Empty() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	subscriber := event.Subscriber.Normalize()
	r.mu.RLock()
	subjectIDs := make([]int64, 0, len(r.subjects))
	for subjectID := range r.subjects {
		subjectIDs = append(subjectIDs, subjectID)
	}
	r.mu.RUnlock()
	for _, subjectID := range subjectIDs {
		if !containsRoomSubscriber(r.coordinator.Subscribers(subjectID), subscriber) {
			continue
		}
		err := r.Unsubscribe(ctx, subscriber, subjectID)
		if err != nil && !errors.Is(err, coreentitysync.ErrSubscriptionNotFound) && !errors.Is(err, ErrRoomSubjectNotRegistered) {
			r.setLastError(errors.Join(r.LastError(), fmt.Errorf("room: evict slow subscriber: %w", err)))
		}
	}
}

// Close stops the room, releases transport baselines and callbacks, and drops
// all entity references. A closed room is intentionally not restartable.
func (r *RoomBroadcaster) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return nil
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	err := r.stop(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if r.unregisterSlowConsumer != nil {
		r.unregisterSlowConsumer()
		r.unregisterSlowConsumer = nil
	}
	r.envelopeSink.ResetRoom(r.roomID)
	r.coordinator.Close()
	r.mu.Lock()
	if r.budget != nil {
		r.budget.releaseSubjects(len(r.subjects))
		r.budget.releaseSubscribers(len(r.subscribers))
	}
	for _, state := range r.subjects {
		state.SetDirtyNotifier(nil)
	}
	clear(r.subjects)
	clear(r.subscribers)
	r.closed = true
	r.mu.Unlock()
	r.dirtyMu.Lock()
	clear(r.dirty)
	clear(r.retiring)
	r.dirtyMu.Unlock()
	return err
}

func (r *RoomBroadcaster) RoomID() int64 {
	if r == nil {
		return 0
	}
	return r.roomID
}

func (r *RoomBroadcaster) SetDownstream(downstream ReliableRoomFrameSink) error {
	if r == nil {
		return ErrRoomIDInvalid
	}
	if downstream == nil {
		return ErrRoomFrameSinkRequired
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	r.envelopeSink.SetDownstream(downstream)
	return nil
}

func (r *RoomBroadcaster) RegisterSubject(state *entity.SubjectSyncState) error {
	if r == nil || state == nil || !state.Enabled() || state.SubjectID() == 0 {
		return ErrRoomSubjectInvalid
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	subjectID := state.SubjectID()
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return ErrRoomBroadcasterStopped
	}
	if existing, ok := r.subjects[subjectID]; ok {
		r.mu.Unlock()
		if existing == state {
			return nil
		}
		return fmt.Errorf("%w: subject %d", ErrRoomSubjectAlreadyExists, subjectID)
	}
	if len(r.subjects) >= r.maxSubjects {
		r.mu.Unlock()
		return ErrRoomSubjectLimit
	}
	if r.budget != nil && !r.budget.reserveSubject() {
		r.mu.Unlock()
		return ErrRoomGlobalSubjectLimit
	}
	if err := r.envelopeSink.RegisterSubject(r.roomID, subjectID); err != nil {
		if r.budget != nil {
			r.budget.releaseSubject()
		}
		r.mu.Unlock()
		return err
	}
	r.subjects[subjectID] = state
	r.mu.Unlock()
	state.SetDirtyNotifier(r.markDirty)
	r.touchActivity()
	return nil
}

func (r *RoomBroadcaster) UnregisterSubject(subjectID int64) error {
	if r == nil || subjectID == 0 {
		return ErrRoomSubjectInvalid
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	op := r.subjectOp(subjectID)
	op.Lock()
	defer op.Unlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	return r.unregisterSubject(subjectID)
}

func (r *RoomBroadcaster) unregisterSubject(subjectID int64) error {
	if r == nil || subjectID == 0 {
		return ErrRoomSubjectInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subjects[subjectID]; !ok {
		return ErrRoomSubjectNotRegistered
	}
	if len(r.coordinator.Subscribers(subjectID)) != 0 {
		return ErrRoomSubjectHasSubscribers
	}
	if err := r.envelopeSink.UnregisterSubject(r.roomID, subjectID); err != nil {
		return err
	}
	r.subjects[subjectID].SetDirtyNotifier(nil)
	delete(r.subjects, subjectID)
	if r.budget != nil {
		r.budget.releaseSubject()
	}
	r.clearDirty(subjectID)
	r.clearRetiring(subjectID)
	return nil
}

func (r *RoomBroadcaster) Subscribe(ctx context.Context, subscriber coreentitysync.SubscriberRef, subjectID int64, profile entity.SyncProfile) (coreentitysync.Subscription, error) {
	if r == nil || subjectID == 0 {
		return coreentitysync.Subscription{}, ErrRoomSubjectInvalid
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	op := r.subjectOp(subjectID)
	op.Lock()
	defer op.Unlock()
	if r.isStopped() {
		return coreentitysync.Subscription{}, ErrRoomBroadcasterStopped
	}
	if r.isRetiring(subjectID) {
		return coreentitysync.Subscription{}, ErrRoomSubjectRetiring
	}
	state, err := r.subject(subjectID)
	if err != nil {
		return coreentitysync.Subscription{}, err
	}
	subscriber = subscriber.Normalize()
	_, already := r.coordinator.Get(subscriber, subjectID)
	if !already {
		r.mu.Lock()
		if r.subscribers[subscriber] == 0 && len(r.subscribers) >= r.maxSubscribers {
			r.mu.Unlock()
			return coreentitysync.Subscription{}, ErrRoomSubscriberLimit
		}
		if r.subscribers[subscriber] == 0 && r.budget != nil && !r.budget.reserveSubscriber() {
			r.mu.Unlock()
			return coreentitysync.Subscription{}, ErrRoomGlobalSubscriberLimit
		}
		r.subscribers[subscriber]++
		r.mu.Unlock()
	}
	subscription, err := r.coordinator.Subscribe(ctx, subscriber, state, profile)
	if err != nil && !already {
		r.releaseSubscriber(ctx, subscriber)
	} else if err == nil {
		r.touchActivity()
	}
	return subscription, err
}

// RetireSubject prevents new subscriptions immediately, transactionally
// admits leave frames for current subscribers, and unregisters the subject.
// Failures remain queued and are retried by the room worker.
func (r *RoomBroadcaster) RetireSubject(ctx context.Context, subjectID int64) error {
	if r == nil {
		return ErrRoomBroadcasterStopped
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	if _, err := r.subject(subjectID); err != nil {
		return err
	}
	r.dirtyMu.Lock()
	r.retiring[subjectID] = struct{}{}
	delete(r.dirty, subjectID)
	r.dirtyMu.Unlock()
	err := r.retryRetirement(ctx, subjectID)
	if err != nil {
		r.flushFailures.Add(1)
	}
	return err
}

func (r *RoomBroadcaster) Unsubscribe(ctx context.Context, subscriber coreentitysync.SubscriberRef, subjectID int64) error {
	if r == nil || subjectID == 0 {
		return ErrRoomSubjectInvalid
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	op := r.subjectOp(subjectID)
	op.Lock()
	defer op.Unlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	if _, err := r.subject(subjectID); err != nil {
		return err
	}
	return r.unsubscribeTracked(ctx, subscriber.Normalize(), subjectID)
}

func (r *RoomBroadcaster) unsubscribeTracked(ctx context.Context, subscriber coreentitysync.SubscriberRef, subjectID int64) error {
	if err := r.coordinator.Unsubscribe(ctx, subscriber, subjectID); err != nil {
		return err
	}
	r.releaseSubscriber(ctx, subscriber)
	return nil
}

func (r *RoomBroadcaster) releaseSubscriber(ctx context.Context, subscriber coreentitysync.SubscriberRef) {
	r.mu.Lock()
	count := r.subscribers[subscriber]
	if count <= 1 {
		delete(r.subscribers, subscriber)
	} else {
		r.subscribers[subscriber] = count - 1
	}
	released := count == 1
	r.mu.Unlock()
	if released {
		if r.budget != nil {
			r.budget.releaseSubscriber()
		}
		r.envelopeSink.ReleaseSubscriber(ctx, r.roomID, subscriber)
	}
}

func containsRoomSubscriber(items []coreentitysync.Subscription, target coreentitysync.SubscriberRef) bool {
	for _, item := range items {
		if item.Subscriber.Normalize() == target {
			return true
		}
	}
	return false
}

func (r *RoomBroadcaster) FlushSubject(ctx context.Context, subjectID int64) error {
	if r == nil {
		return ErrRoomBroadcasterStopped
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	state, err := r.subject(subjectID)
	if err != nil {
		return err
	}
	r.clearDirty(subjectID)
	err = r.flushState(ctx, state)
	if err == nil {
		r.touchActivity()
	}
	return err
}

// Start enables coalesced asynchronous flushes. The default interval is 50ms
// (20Hz). Dirty notification only schedules work; packing remains protected by
// the Entity mutex inside core.
func (r *RoomBroadcaster) Start(ctx context.Context, interval time.Duration) error {
	if r == nil {
		return ErrRoomIDInvalid
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if interval <= 0 {
		interval = DefaultRoomBroadcastInterval
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return ErrRoomBroadcasterStopped
	}
	if r.started {
		r.mu.Unlock()
		return nil
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	r.stopCh = stopCh
	r.doneCh = doneCh
	r.started = true
	r.mu.Unlock()
	go r.run(ctx, interval, stopCh, doneCh)
	return nil
}

// Stop flushes admitted dirty subjects once and waits for the worker. A room
// replication instance is intentionally not restartable after Stop.
func (r *RoomBroadcaster) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	return r.stop(ctx)
}

func (r *RoomBroadcaster) stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if r.stopped {
		doneCh := r.doneCh
		r.mu.Unlock()
		if doneCh == nil {
			return nil
		}
		select {
		case <-doneCh:
			r.detachNotifiers()
			return r.LastError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.stopped = true
	started := r.started
	stopCh := r.stopCh
	doneCh := r.doneCh
	r.mu.Unlock()
	if !started {
		err := r.flushDirty(ctx)
		r.detachNotifiers()
		return err
	}
	close(stopCh)
	select {
	case <-doneCh:
		r.detachNotifiers()
		return r.LastError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FlushDirty is the active flush API. It drains the current dirty set in
// stable subject order and requeues every state that remains dirty on failure
// or because a concurrent mutation arrived during delivery.
func (r *RoomBroadcaster) FlushDirty(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.lifecycleMu.RLock()
	defer r.lifecycleMu.RUnlock()
	if r.isStopped() {
		return ErrRoomBroadcasterStopped
	}
	return r.flushDirty(ctx)
}

func (r *RoomBroadcaster) flushDirty(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids := r.takeDirty()
	var flushErrors []error
	if err := r.flushStateBatch(ctx, ids); err != nil {
		flushErrors = append(flushErrors, err)
	}
	for _, subjectID := range r.retirementIDs() {
		if err := r.retryRetirement(ctx, subjectID); err != nil {
			r.flushFailures.Add(1)
			flushErrors = append(flushErrors, fmt.Errorf("retire subject %d: %w", subjectID, err))
		}
	}
	return errors.Join(flushErrors...)
}

// flushStateBatch prepares all dirty room subjects under stable subject
// stripes, then admits them through the coordinator as one global room frame.
func (r *RoomBroadcaster) flushStateBatch(ctx context.Context, subjectIDs []int64) error {
	if len(subjectIDs) == 0 {
		return nil
	}
	stripeSet := make(map[int]struct{}, len(subjectIDs))
	for _, subjectID := range subjectIDs {
		if !r.isRetiring(subjectID) {
			stripeSet[int(uint64(subjectID)%uint64(len(r.flushOps)))] = struct{}{}
		}
	}
	stripes := make([]int, 0, len(stripeSet))
	for stripe := range stripeSet {
		stripes = append(stripes, stripe)
	}
	sort.Ints(stripes)
	for _, stripe := range stripes {
		r.flushOps[stripe].Lock()
	}
	defer func() {
		for i := len(stripes) - 1; i >= 0; i-- {
			r.flushOps[stripes[i]].Unlock()
		}
	}()

	prepared := make([]*entity.PreparedSubjectSync, 0, len(subjectIDs))
	states := make([]*entity.SubjectSyncState, 0, len(subjectIDs))
	var prepareErrors []error
	for _, subjectID := range subjectIDs {
		if r.isRetiring(subjectID) {
			continue
		}
		state, err := r.subject(subjectID)
		if errors.Is(err, ErrRoomSubjectNotRegistered) {
			continue
		}
		if err != nil {
			prepareErrors = append(prepareErrors, fmt.Errorf("subject %d: %w", subjectID, err))
			continue
		}
		item, err := state.Prepare(r.coordinator.Profiles(subjectID))
		if errors.Is(err, entity.ErrSubjectSyncNotDirty) {
			continue
		}
		if err != nil {
			prepareErrors = append(prepareErrors, fmt.Errorf("subject %d: %w", subjectID, err))
			if state.PendingDirty() {
				r.markDirty(state)
			}
			continue
		}
		prepared = append(prepared, item)
		states = append(states, state)
	}
	if len(prepareErrors) > 0 {
		r.flushFailures.Add(uint64(len(prepareErrors)))
	}
	if len(prepared) > 0 {
		if err := r.coordinator.DistributeBatch(ctx, prepared); err != nil {
			r.flushFailures.Add(uint64(len(prepared)))
			prepareErrors = append(prepareErrors, fmt.Errorf("global frame (%d subjects): %w", len(prepared), err))
		}
	}
	for _, state := range states {
		if state.PendingDirty() {
			r.markDirty(state)
		}
	}
	if len(prepared) > 0 && len(prepareErrors) == 0 {
		r.touchActivity()
	}
	return errors.Join(prepareErrors...)
}

func (r *RoomBroadcaster) Subscribers(subjectID int64) []coreentitysync.Subscription {
	if r == nil {
		return nil
	}
	return r.coordinator.Subscribers(subjectID)
}

func (r *RoomBroadcaster) Profiles(subjectID int64) []entity.SyncProfile {
	if r == nil {
		return nil
	}
	return r.coordinator.Profiles(subjectID)
}

func (r *RoomBroadcaster) Stats() RoomBroadcasterStats {
	if r == nil {
		return RoomBroadcasterStats{}
	}
	stats := r.envelopeSink.Stats()
	stats.FlushFailures = r.flushFailures.Load()
	r.dirtyMu.Lock()
	stats.PendingSubjects = len(r.dirty)
	stats.PendingRetirements = len(r.retiring)
	r.dirtyMu.Unlock()
	r.mu.RLock()
	stats.ActiveSubjects = len(r.subjects)
	stats.ActiveSubscribers = len(r.subscribers)
	r.mu.RUnlock()
	if err := r.LastError(); err != nil {
		stats.LastError = err.Error()
	}
	return stats
}

func (r *RoomBroadcaster) LastError() error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	err := r.lastError
	r.mu.RUnlock()
	return err
}

func (r *RoomBroadcaster) setLastError(err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.lastError = err
	r.mu.Unlock()
}

func (r *RoomBroadcaster) subject(subjectID int64) (*entity.SubjectSyncState, error) {
	if r == nil || subjectID == 0 {
		return nil, ErrRoomSubjectInvalid
	}
	r.mu.RLock()
	state, ok := r.subjects[subjectID]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrRoomSubjectNotRegistered
	}
	return state, nil
}

func (r *RoomBroadcaster) flushState(ctx context.Context, state *entity.SubjectSyncState) error {
	if state == nil {
		return ErrRoomSubjectInvalid
	}
	subjectID := state.SubjectID()
	op := &r.flushOps[uint64(subjectID)%uint64(len(r.flushOps))]
	op.Lock()
	defer op.Unlock()
	err := r.coordinator.FlushSubject(ctx, state)
	if state.PendingDirty() {
		r.markDirty(state)
	}
	return err
}

func (r *RoomBroadcaster) markDirty(state *entity.SubjectSyncState) {
	if r == nil || state == nil || state.SubjectID() == 0 {
		return
	}
	r.mu.RLock()
	registered := r.subjects[state.SubjectID()] == state
	r.mu.RUnlock()
	if !registered || r.isRetiring(state.SubjectID()) {
		return
	}
	r.dirtyMu.Lock()
	r.dirty[state.SubjectID()] = struct{}{}
	r.dirtyMu.Unlock()
	r.touchActivity()
}

func (r *RoomBroadcaster) touchActivity() {
	if r != nil && r.onActivity != nil {
		r.onActivity()
	}
}

func (r *RoomBroadcaster) isStopped() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	stopped := r.stopped
	r.mu.RUnlock()
	return stopped
}

func (r *RoomBroadcaster) retryRetirement(ctx context.Context, subjectID int64) error {
	op := r.subjectOp(subjectID)
	op.Lock()
	defer op.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	subscriptions := r.coordinator.Subscribers(subjectID)
	var retireErrors []error
	for _, subscription := range subscriptions {
		if err := r.unsubscribeTracked(ctx, subscription.Subscriber.Normalize(), subjectID); err != nil && !errors.Is(err, coreentitysync.ErrSubscriptionNotFound) {
			retireErrors = append(retireErrors, err)
		}
	}
	if err := errors.Join(retireErrors...); err != nil {
		return err
	}
	if err := r.unregisterSubject(subjectID); err != nil && !errors.Is(err, ErrRoomSubjectNotRegistered) {
		return err
	}
	r.clearRetiring(subjectID)
	return nil
}

func (r *RoomBroadcaster) subjectOp(subjectID int64) *stdsync.Mutex {
	return &r.flushOps[uint64(subjectID)%uint64(len(r.flushOps))]
}

func (r *RoomBroadcaster) isRetiring(subjectID int64) bool {
	r.dirtyMu.Lock()
	_, ok := r.retiring[subjectID]
	r.dirtyMu.Unlock()
	return ok
}

func (r *RoomBroadcaster) clearRetiring(subjectID int64) {
	r.dirtyMu.Lock()
	delete(r.retiring, subjectID)
	r.dirtyMu.Unlock()
}

func (r *RoomBroadcaster) retirementIDs() []int64 {
	r.dirtyMu.Lock()
	ids := make([]int64, 0, len(r.retiring))
	for subjectID := range r.retiring {
		ids = append(ids, subjectID)
	}
	r.dirtyMu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (r *RoomBroadcaster) detachNotifiers() {
	r.mu.Lock()
	states := make([]*entity.SubjectSyncState, 0, len(r.subjects))
	for _, state := range r.subjects {
		states = append(states, state)
	}
	if r.budget != nil {
		r.budget.releaseSubscribers(len(r.subscribers))
	}
	clear(r.subscribers)
	r.mu.Unlock()
	for _, state := range states {
		state.SetDirtyNotifier(nil)
	}
	r.envelopeSink.ResetRoom(r.roomID)
}

func (r *RoomBroadcaster) clearDirty(subjectID int64) {
	r.dirtyMu.Lock()
	delete(r.dirty, subjectID)
	r.dirtyMu.Unlock()
}

func (r *RoomBroadcaster) takeDirty() []int64 {
	r.dirtyMu.Lock()
	ids := make([]int64, 0, len(r.dirty))
	for subjectID := range r.dirty {
		ids = append(ids, subjectID)
		delete(r.dirty, subjectID)
	}
	r.dirtyMu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (r *RoomBroadcaster) run(ctx context.Context, interval time.Duration, stopCh <-chan struct{}, doneCh chan<- struct{}) {
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		flushCtx, cancel := context.WithTimeout(context.Background(), interval)
		err := r.flushDirty(flushCtx)
		cancel()
		r.mu.Lock()
		r.lastError = err
		if r.doneCh == doneCh {
			r.started = false
		}
		r.mu.Unlock()
		close(doneCh)
	}()
	for {
		select {
		case <-ticker.C:
			flushCtx, cancel := context.WithTimeout(context.Background(), interval)
			err := r.flushDirty(flushCtx)
			cancel()
			r.mu.Lock()
			r.lastError = err
			r.mu.Unlock()
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		}
	}
}
