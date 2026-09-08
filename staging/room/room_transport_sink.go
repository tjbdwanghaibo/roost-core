package room

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
	"github.com/tjbdwanghaibo/roost-core/health"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
	kit "github.com/tjbdwanghaibo/roost-kit/nettransport"
)

const (
	roomWireMagic          uint32 = 0x52524631 // RRF1
	roomSubjectMagic       uint32 = 0x52535531 // RSU1
	RoomWireVersion        uint16 = 1
	DefaultRoomComponentID uint16 = 1
	DefaultRoomArchetype   uint16 = 1
	roomWireHeaderBytes           = 28
	roomSubjectHeaderBytes        = 64
	roomSubjectFlagFull    uint16 = 1 << iota
)

var (
	ErrRoomTransportRequired = errors.New("room: atomic room transport is required")
	ErrRoomSessionResolver   = errors.New("room: room session resolver is required")
	ErrRoomSessionInvalid    = errors.New("room: resolved room session is invalid")
	ErrRoomWireFrame         = errors.New("room: invalid room wire frame")
	ErrRoomSubjectBaseline   = errors.New("room: room subject has no transport baseline")
	ErrRoomTransportClosed   = errors.New("room: room transport sink is closed")
)

const roomTransportLockStripes = 256

type RoomSessionResolver interface {
	ResolveRoomSession(context.Context, coreentitysync.SubscriberRef) (core.SessionID, error)
}

type RoomSessionResolverFunc func(context.Context, coreentitysync.SubscriberRef) (core.SessionID, error)

func (f RoomSessionResolverFunc) ResolveRoomSession(ctx context.Context, subscriber coreentitysync.SubscriberRef) (core.SessionID, error) {
	if f == nil {
		return 0, ErrRoomSessionResolver
	}
	return f(ctx, subscriber)
}

type RoomTransportSinkConfig struct {
	Transport              kit.AtomicBatchTransport
	Sessions               RoomSessionResolver
	Limits                 core.Limits
	MaxDatagramBytes       int
	FrameSchemaVersion     uint16
	ComponentTypeID        uint16
	ComponentSchemaVersion uint16
	Archetype              uint16
	SlowConsumerPolicy     SlowConsumerPolicy
	OnSlowConsumer         func(context.Context, RoomSlowConsumer)
	CallbackWorkers        int
	CallbackQueueCapacity  int
	CallbackTimeout        time.Duration
}

type RoomSlowConsumer struct {
	RoomID     int64
	Subscriber coreentitysync.SubscriberRef
	Session    core.SessionID
	Err        error
}

type SlowConsumerPolicy uint8

const (
	SlowConsumerEvict SlowConsumerPolicy = iota + 1
	SlowConsumerFailBatch
)

// RoomTransportSink encodes receiver-specific room frames onto the common
// replication wire format. Snapshot/leave frames use the reliable ordered
// lane; state-only deltas use fragmented latest-only datagrams.
type RoomTransportSink struct {
	mu                    stdsync.RWMutex
	roomLocks             [roomTransportLockStripes]stdsync.Mutex
	transport             kit.AtomicBatchTransport
	sessions              RoomSessionResolver
	config                RoomTransportSinkConfig
	rooms                 map[roomSessionKey]*roomObjectRefs
	evicted               map[roomSessionKey]coreentitysync.SubscriberRef
	deadSessions          map[core.SessionID]struct{}
	handlers              map[uint64]roomSlowConsumerHandler
	nextHandlerID         uint64
	callbackCtx           context.Context
	callbackCancel        context.CancelFunc
	callbackQueue         chan struct{}
	pendingCallbacks      map[roomSessionKey]RoomSlowConsumer
	callbackWG            stdsync.WaitGroup
	callbackDone          chan struct{}
	closeOnce             stdsync.Once
	closed                bool
	slowConsumerEvictions atomic.Uint64
	callbackCoalesced     atomic.Uint64
	callbackPanics        atomic.Uint64
}

type roomSlowConsumerHandler struct {
	id uint64
	fn func(context.Context, RoomSlowConsumer)
}

type roomSessionKey struct {
	roomID  uint64
	session core.SessionID
}

type roomObjectRefs struct {
	objects     map[int64]core.ObjectRef
	generations map[uint16]uint16
	free        []uint16
	next        uint16
	subscriber  coreentitysync.SubscriberRef
}

func NewRoomTransportSink(config RoomTransportSinkConfig) (*RoomTransportSink, error) {
	if config.Transport == nil {
		return nil, ErrRoomTransportRequired
	}
	if config.Sessions == nil {
		return nil, ErrRoomSessionResolver
	}
	defaults := core.DefaultLimits()
	config.Limits = mergeRoomLimits(config.Limits, defaults)
	if config.MaxDatagramBytes <= 0 {
		config.MaxDatagramBytes = config.Limits.MaxDatagramBytes
	}
	if config.MaxDatagramBytes > config.Limits.MaxDatagramBytes || config.MaxDatagramBytes <= core.DatagramHeaderSize {
		return nil, core.ErrInvalidDatagram
	}
	if config.FrameSchemaVersion == 0 {
		config.FrameSchemaVersion = RoomWireVersion
	}
	if config.ComponentTypeID == 0 {
		config.ComponentTypeID = DefaultRoomComponentID
	}
	if config.ComponentSchemaVersion == 0 {
		config.ComponentSchemaVersion = RoomWireVersion
	}
	if config.Archetype == 0 {
		config.Archetype = DefaultRoomArchetype
	}
	if config.SlowConsumerPolicy == 0 {
		config.SlowConsumerPolicy = SlowConsumerEvict
	}
	if config.CallbackWorkers <= 0 {
		config.CallbackWorkers = 4
	}
	if config.CallbackQueueCapacity <= 0 {
		config.CallbackQueueCapacity = 4096
	}
	if config.CallbackTimeout <= 0 {
		config.CallbackTimeout = time.Second
	}
	callbackCtx, callbackCancel := context.WithCancel(context.Background())
	sink := &RoomTransportSink{
		transport: config.Transport, sessions: config.Sessions, config: config,
		rooms: make(map[roomSessionKey]*roomObjectRefs), evicted: make(map[roomSessionKey]coreentitysync.SubscriberRef),
		deadSessions: make(map[core.SessionID]struct{}), handlers: make(map[uint64]roomSlowConsumerHandler),
		callbackCtx: callbackCtx, callbackCancel: callbackCancel,
		callbackQueue:    make(chan struct{}, config.CallbackQueueCapacity),
		pendingCallbacks: make(map[roomSessionKey]RoomSlowConsumer), callbackDone: make(chan struct{}),
	}
	sink.callbackWG.Add(config.CallbackWorkers)
	for range config.CallbackWorkers {
		go sink.runCallbackWorker()
	}
	go func() {
		sink.callbackWG.Wait()
		close(sink.callbackDone)
	}()
	return sink, nil
}

// RegisterRoomSlowConsumerHandler installs one lifecycle handler per room so a
// shared transport sink cannot overwrite another room's eviction callback.
func (s *RoomTransportSink) RegisterRoomSlowConsumerHandler(roomID int64, handler func(context.Context, RoomSlowConsumer)) (func(), error) {
	if s == nil || roomID <= 0 || handler == nil {
		return nil, ErrRoomIDInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrRoomTransportClosed
	}
	key := uint64(roomID)
	if _, exists := s.handlers[key]; exists {
		return nil, fmt.Errorf("room: slow consumer handler already registered for room %d", roomID)
	}
	s.nextHandlerID++
	id := s.nextHandlerID
	s.handlers[key] = roomSlowConsumerHandler{id: id, fn: handler}
	return func() {
		s.mu.Lock()
		if current, ok := s.handlers[key]; ok && current.id == id {
			delete(s.handlers, key)
		}
		s.mu.Unlock()
	}, nil
}

func (s *RoomTransportSink) AdmitRoomFrames(ctx context.Context, frames []RoomFrame) error {
	if s == nil || s.transport == nil {
		return ErrRoomTransportRequired
	}
	if len(frames) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	type resolvedFrame struct {
		frame RoomFrame
		key   roomSessionKey
	}
	resolved := make([]resolvedFrame, 0, len(frames))
	seenRoutes := make(map[roomSessionKey]struct{}, len(frames))
	for _, frame := range frames {
		if frame.RoomID <= 0 || frame.Frame == 0 || frame.SessionSequence == 0 || frame.Subscriber.Normalize().Empty() || len(frame.Entries) == 0 {
			return ErrRoomWireFrame
		}
		frame.Subscriber = frame.Subscriber.Normalize()
		session, err := s.sessions.ResolveRoomSession(ctx, frame.Subscriber)
		if err != nil {
			return errors.Join(ErrRoomSessionInvalid, err)
		}
		if session == 0 {
			return ErrRoomSessionInvalid
		}
		key := roomSessionKey{roomID: uint64(frame.RoomID), session: session}
		if _, exists := seenRoutes[key]; exists {
			return fmt.Errorf("%w: duplicate room/session route", ErrRoomWireFrame)
		}
		seenRoutes[key] = struct{}{}
		resolved = append(resolved, resolvedFrame{frame: frame, key: key})
	}

	unlockRooms := s.lockFrameRooms(frames)
	defer func() {
		if unlockRooms != nil {
			unlockRooms()
		}
	}()

	plans := make(map[roomSessionKey]*roomObjectRefs)
	routeSubscribers := make(map[roomSessionKey]coreentitysync.SubscriberRef, len(frames))
	outbound := make([]kit.OutboundFrame, 0, len(frames))
	componentCache := make(map[roomComponentCacheKey][]byte)
	for _, item := range resolved {
		frame, key := item.frame, item.key
		s.mu.RLock()
		_, closedSession := s.deadSessions[key.session]
		closed := s.closed
		base := s.rooms[key]
		s.mu.RUnlock()
		if closed {
			return ErrRoomTransportClosed
		}
		if closedSession {
			continue
		}
		routeSubscribers[key] = frame.Subscriber
		state := base
		if roomFrameMutatesObjectRefs(frame) {
			state = cloneRoomObjectRefs(base)
			state.subscriber = frame.Subscriber
			plans[key] = state
		} else if state == nil {
			// Keep the delta fast path allocation-free once a baseline exists,
			// while still returning a typed baseline error for a new session.
			state = cloneRoomObjectRefs(nil)
		}
		encoded, reliable, delta, err := s.encodeFrame(frame, state, componentCache)
		if err != nil {
			return err
		}
		out := kit.OutboundFrame{Session: key.session}
		if reliable {
			out.Reliable = encoded
		} else {
			sequence := nonzeroSequence(frame.SessionSequence)
			out.Datagrams, err = core.FragmentFrame(delta, sequence, encoded, s.config.MaxDatagramBytes, s.config.Limits)
			if err != nil {
				return err
			}
		}
		outbound = append(outbound, out)
	}
	events, err := s.admitWithSlowConsumerPolicy(ctx, outbound, plans, routeSubscribers)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for key, state := range plans {
		if _, dead := s.deadSessions[key.session]; !dead && !s.closed {
			s.rooms[key] = state
		}
	}
	s.mu.Unlock()
	unlockRooms()
	unlockRooms = nil
	s.dispatchSlowConsumers(events)
	return nil
}

func (s *RoomTransportSink) admitWithSlowConsumerPolicy(ctx context.Context, outbound []kit.OutboundFrame, plans map[roomSessionKey]*roomObjectRefs, routeSubscribers map[roomSessionKey]coreentitysync.SubscriberRef) ([]RoomSlowConsumer, error) {
	var events []RoomSlowConsumer
	for len(outbound) > 0 {
		err := s.transport.AdmitBatch(ctx, outbound)
		if err == nil {
			return events, nil
		}
		var admission kit.AdmissionError
		if s.config.SlowConsumerPolicy != SlowConsumerEvict || !errors.As(err, &admission) || !errors.Is(err, kit.ErrReliableBackpressure) || admission.Session == 0 {
			return nil, err
		}
		filtered := outbound[:0]
		for _, item := range outbound {
			if item.Session != admission.Session {
				filtered = append(filtered, item)
			}
		}
		outbound = filtered
		eventByRoom := make(map[uint64]RoomSlowConsumer)
		for key, state := range plans {
			if key.session == admission.Session {
				subscriber := routeSubscribers[key]
				if subscriber.Empty() && state != nil {
					subscriber = state.subscriber
				}
				eventByRoom[key.roomID] = RoomSlowConsumer{RoomID: int64(key.roomID), Subscriber: subscriber, Session: admission.Session, Err: err}
				delete(plans, key)
			}
		}
		s.mu.Lock()
		s.deadSessions[admission.Session] = struct{}{}
		for key := range s.rooms {
			if key.session == admission.Session {
				state := s.rooms[key]
				subscriber := coreentitysync.SubscriberRef{}
				if state != nil {
					subscriber = state.subscriber
				}
				s.evicted[key] = subscriber
				eventByRoom[key.roomID] = RoomSlowConsumer{RoomID: int64(key.roomID), Subscriber: subscriber, Session: admission.Session, Err: err}
				delete(s.rooms, key)
			}
		}
		s.mu.Unlock()
		if remover, ok := s.transport.(interface{ RemoveSession(core.SessionID) bool }); ok {
			remover.RemoveSession(admission.Session)
		}
		s.slowConsumerEvictions.Add(1)
		for _, event := range eventByRoom {
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *RoomTransportSink) lockFrameRooms(frames []RoomFrame) func() {
	indexes := make([]int, 0, len(frames))
	seen := make(map[int]struct{}, len(frames))
	for _, frame := range frames {
		index := int(uint64(frame.RoomID) % roomTransportLockStripes)
		if _, exists := seen[index]; !exists {
			seen[index] = struct{}{}
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		s.roomLocks[index].Lock()
	}
	return func() {
		for i := len(indexes) - 1; i >= 0; i-- {
			s.roomLocks[indexes[i]].Unlock()
		}
	}
}

func (s *RoomTransportSink) dispatchSlowConsumers(events []RoomSlowConsumer) {
	for _, event := range events {
		key := roomSessionKey{roomID: uint64(event.RoomID), session: event.Session}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		if _, exists := s.pendingCallbacks[key]; exists {
			s.callbackCoalesced.Add(1)
		}
		s.pendingCallbacks[key] = event
		s.mu.Unlock()
		select {
		case s.callbackQueue <- struct{}{}:
		default:
		}
	}
}

func (s *RoomTransportSink) runCallbackWorker() {
	defer s.callbackWG.Done()
	for {
		select {
		case <-s.callbackCtx.Done():
			return
		case <-s.callbackQueue:
			for s.runNextCallback() {
			}
		}
	}
}

func (s *RoomTransportSink) runNextCallback() bool {
	if s.callbackCtx.Err() != nil {
		return false
	}
	s.mu.Lock()
	var event RoomSlowConsumer
	var found bool
	for key, pending := range s.pendingCallbacks {
		event = pending
		delete(s.pendingCallbacks, key)
		found = true
		break
	}
	handler := s.handlers[uint64(event.RoomID)].fn
	s.mu.Unlock()
	if !found {
		return false
	}
	ctx, cancel := context.WithTimeout(s.callbackCtx, s.config.CallbackTimeout)
	func() {
		defer func() {
			if recover() != nil {
				s.callbackPanics.Add(1)
			}
		}()
		if handler != nil && !event.Subscriber.Normalize().Empty() {
			handler(ctx, event)
		}
		if s.config.OnSlowConsumer != nil {
			s.config.OnSlowConsumer(ctx, event)
		}
	}()
	cancel()
	return true
}

func (s *RoomTransportSink) lockRoom(roomID int64) func() {
	index := int(uint64(roomID) % roomTransportLockStripes)
	s.roomLocks[index].Lock()
	return s.roomLocks[index].Unlock
}

func (s *RoomTransportSink) lockAllRooms() func() {
	for i := range s.roomLocks {
		s.roomLocks[i].Lock()
	}
	return func() {
		for i := len(s.roomLocks) - 1; i >= 0; i-- {
			s.roomLocks[i].Unlock()
		}
	}
}

type roomComponentCacheKey struct {
	subjectID   int64
	version     uint64
	baseVersion uint64
	mask        uint64
	reason      uint32
	profile     entity.SyncProfile
	namespace   string
	full        bool
}

func (s *RoomTransportSink) encodeFrame(frame RoomFrame, state *roomObjectRefs, componentCache map[roomComponentCacheKey][]byte) ([]byte, bool, core.DeltaFrame, error) {
	epoch, tick, baseTick := roomWireClock(frame.Frame)
	delta := core.DeltaFrame{
		SnapshotMeta: core.SnapshotMeta{
			RoomID: uint64(frame.RoomID), Epoch: epoch, Tick: tick,
			SchemaVersion: s.config.FrameSchemaVersion,
		},
		Kind: core.FrameDelta, BaseTick: baseTick,
		Objects: make([]core.ObjectDelta, 0, len(frame.Entries)),
	}
	if len(frame.Entries) > s.config.Limits.MaxObjects {
		return nil, false, core.DeltaFrame{}, core.ErrObjectLimit
	}
	reliable := false
	var previousSubjectID int64
	for entryIndex, entry := range frame.Entries {
		update := entry.Update
		if update.SubjectID == 0 {
			return nil, false, core.DeltaFrame{}, ErrRoomSubjectInvalid
		}
		if entryIndex > 0 && update.SubjectID <= previousSubjectID {
			return nil, false, core.DeltaFrame{}, fmt.Errorf("%w: subjects must be strictly ordered: %d after %d", ErrRoomWireFrame, update.SubjectID, previousSubjectID)
		}
		previousSubjectID = update.SubjectID
		ref, exists := state.objects[update.SubjectID]
		object := core.ObjectDelta{Ref: ref}
		switch entry.Kind {
		case coreentitysync.EnvelopeSnapshot:
			reliable = true
			if !exists {
				var err error
				ref, err = state.allocate(update.SubjectID, s.config.Limits.MaxObjects)
				if err != nil {
					return nil, false, core.DeltaFrame{}, err
				}
			}
			object.Ref = ref
			object.Operation = core.ObjectCreate
			object.Archetype = s.config.Archetype
		case coreentitysync.EnvelopeDelta:
			if !exists {
				return nil, false, core.DeltaFrame{}, fmt.Errorf("%w: room=%d subject=%d", ErrRoomSubjectBaseline, frame.RoomID, update.SubjectID)
			}
			object.Operation = core.ObjectUpdate
		case coreentitysync.EnvelopeLeave:
			reliable = true
			if !exists {
				return nil, false, core.DeltaFrame{}, fmt.Errorf("%w: room=%d subject=%d", ErrRoomSubjectBaseline, frame.RoomID, update.SubjectID)
			}
			object.Operation = core.ObjectRemove
		default:
			return nil, false, core.DeltaFrame{}, ErrRoomWireFrame
		}
		if object.Operation != core.ObjectRemove {
			cacheKey := roomComponentCacheKey{subjectID: update.SubjectID, version: update.Version, baseVersion: update.BaseVersion, mask: update.Mask, reason: update.Reason, profile: update.Profile.Normalize(), namespace: update.Namespace, full: update.Full}
			payload := componentCache[cacheKey]
			if payload == nil {
				var err error
				payload, err = EncodeRoomSubjectUpdate(update, s.config.Limits.MaxComponentBytes)
				if err != nil {
					return nil, false, core.DeltaFrame{}, err
				}
				componentCache[cacheKey] = payload
			}
			object.Components = []core.ComponentDelta{{
				Operation: core.ComponentSet, TypeID: s.config.ComponentTypeID,
				SchemaVersion: s.config.ComponentSchemaVersion, Data: payload,
			}}
		}
		delta.Objects = append(delta.Objects, object)
	}
	inner, err := core.EncodeFrame(delta, s.config.Limits)
	if err != nil {
		return nil, false, core.DeltaFrame{}, err
	}
	return encodeRoomWireFrame(frame.Frame, frame.SessionSequence, inner), reliable, delta, nil
}

func roomFrameMutatesObjectRefs(frame RoomFrame) bool {
	for _, entry := range frame.Entries {
		if entry.Kind != coreentitysync.EnvelopeDelta {
			return true
		}
	}
	return false
}

func (s *RoomTransportSink) ReleaseRoomSubject(roomID, subjectID int64) {
	if s == nil || roomID <= 0 || subjectID == 0 {
		return
	}
	unlock := s.lockRoom(roomID)
	defer unlock()
	s.mu.Lock()
	for key, state := range s.rooms {
		if key.roomID == uint64(roomID) && state != nil {
			state.release(subjectID)
		}
	}
	s.mu.Unlock()
}

func (s *RoomTransportSink) ResetRoom(roomID int64) {
	if s == nil || roomID <= 0 {
		return
	}
	unlock := s.lockRoom(roomID)
	defer unlock()
	s.mu.Lock()
	for key := range s.rooms {
		if key.roomID == uint64(roomID) {
			delete(s.rooms, key)
		}
	}
	for key := range s.evicted {
		if key.roomID == uint64(roomID) {
			delete(s.evicted, key)
		}
	}
	s.mu.Unlock()
}

func (s *RoomTransportSink) ReleaseRoomSubscriber(ctx context.Context, roomID int64, subscriber coreentitysync.SubscriberRef) {
	if s == nil || roomID <= 0 || subscriber.Normalize().Empty() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := s.sessions.ResolveRoomSession(ctx, subscriber.Normalize())
	if err != nil || session == 0 {
		return
	}
	unlock := s.lockRoom(roomID)
	defer unlock()
	s.mu.Lock()
	delete(s.rooms, roomSessionKey{roomID: uint64(roomID), session: session})
	delete(s.evicted, roomSessionKey{roomID: uint64(roomID), session: session})
	s.mu.Unlock()
}

// ReleaseSession must be called by the connection lifecycle when a session is
// disconnected. It removes state for every room before a numeric ID can be
// reused by a new connection.
func (s *RoomTransportSink) ReleaseSession(session core.SessionID) {
	if s == nil || session == 0 {
		return
	}
	unlock := s.lockAllRooms()
	defer unlock()
	s.mu.Lock()
	for key := range s.rooms {
		if key.session == session {
			delete(s.rooms, key)
		}
	}
	for key := range s.evicted {
		if key.session == session {
			delete(s.evicted, key)
		}
	}
	delete(s.deadSessions, session)
	s.mu.Unlock()
}

type RoomTransportSinkStats struct {
	RoomSessions          int
	SlowConsumerEvictions uint64
	EvictedRoomSessions   int
	DeadSessions          int
	CallbackPending       int
	CallbackCoalesced     uint64
	CallbackPanics        uint64
}

func (s *RoomTransportSink) Stats() RoomTransportSinkStats {
	if s == nil {
		return RoomTransportSinkStats{}
	}
	s.mu.RLock()
	count := len(s.rooms)
	evicted := len(s.evicted)
	dead := len(s.deadSessions)
	pending := len(s.pendingCallbacks)
	s.mu.RUnlock()
	return RoomTransportSinkStats{
		RoomSessions: count, SlowConsumerEvictions: s.slowConsumerEvictions.Load(), EvictedRoomSessions: evicted,
		DeadSessions: dead, CallbackPending: pending, CallbackCoalesced: s.callbackCoalesced.Load(), CallbackPanics: s.callbackPanics.Load(),
	}
}

func (s *RoomTransportSink) CheckHealth(context.Context) health.Result {
	if s == nil {
		return health.Result{Status: health.StatusFail, Message: "room transport sink is nil"}
	}
	stats := s.Stats()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return health.Result{Status: health.StatusFail, Message: "room transport sink is closed"}
	}
	message := fmt.Sprintf("room_sessions=%d dead_sessions=%d callback_pending=%d callback_coalesced=%d callback_panics=%d", stats.RoomSessions, stats.DeadSessions, stats.CallbackPending, stats.CallbackCoalesced, stats.CallbackPanics)
	if stats.CallbackPending >= s.config.CallbackQueueCapacity {
		return health.Result{Status: health.StatusFail, Message: message}
	}
	if stats.CallbackPanics > 0 || stats.CallbackPending*10 >= s.config.CallbackQueueCapacity*8 {
		return health.Result{Status: health.StatusDegraded, Message: message}
	}
	return health.Result{Status: health.StatusOK, Message: message}
}

// Close stops the bounded lifecycle/application callback executor. A caller
// supplied deadline bounds callbacks that do not honor their callback context.
func (s *RoomTransportSink) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.callbackCancel()
	})
	select {
	case <-s.callbackDone:
		s.mu.Lock()
		clear(s.pendingCallbacks)
		clear(s.handlers)
		clear(s.rooms)
		clear(s.evicted)
		clear(s.deadSessions)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RoomTransportSink) Decode(data []byte) (uint64, uint64, core.DeltaFrame, error) {
	if s == nil {
		return 0, 0, core.DeltaFrame{}, ErrRoomWireFrame
	}
	return DecodeRoomWireFrame(data, s.config.Limits)
}

func EncodeRoomSubjectUpdate(update entity.SubjectSyncUpdate, maxBytes int) ([]byte, error) {
	profile := update.Profile.Normalize()
	namespace := []byte(update.Namespace)
	profileKey := []byte(profile.Key)
	if len(namespace) > int(^uint16(0)) || len(profileKey) > int(^uint16(0)) {
		return nil, core.ErrComponentTooLarge
	}
	total := roomSubjectHeaderBytes + len(namespace) + len(profileKey) + update.Payload.Len()
	if maxBytes <= 0 {
		maxBytes = core.DefaultLimits().MaxComponentBytes
	}
	if total > maxBytes || update.SubjectID == 0 {
		return nil, core.ErrComponentTooLarge
	}
	out := make([]byte, roomSubjectHeaderBytes, total)
	binary.BigEndian.PutUint32(out[0:4], roomSubjectMagic)
	binary.BigEndian.PutUint16(out[4:6], RoomWireVersion)
	flags := uint16(0)
	if update.Full {
		flags |= roomSubjectFlagFull
	}
	binary.BigEndian.PutUint16(out[6:8], flags)
	binary.BigEndian.PutUint64(out[8:16], uint64(update.SubjectID))
	binary.BigEndian.PutUint32(out[16:20], update.SubjectKind)
	binary.BigEndian.PutUint64(out[20:28], update.Version)
	binary.BigEndian.PutUint64(out[28:36], update.BaseVersion)
	binary.BigEndian.PutUint64(out[36:44], update.Mask)
	binary.BigEndian.PutUint32(out[44:48], update.Reason)
	binary.BigEndian.PutUint16(out[48:50], update.Payload.Codec())
	out[50] = profile.LOD
	binary.BigEndian.PutUint32(out[52:56], profile.SchemaVersion)
	binary.BigEndian.PutUint16(out[56:58], uint16(len(namespace)))
	binary.BigEndian.PutUint16(out[58:60], uint16(len(profileKey)))
	binary.BigEndian.PutUint32(out[60:64], uint32(update.Payload.Len()))
	out = append(out, namespace...)
	out = append(out, profileKey...)
	out = update.Payload.AppendTo(out)
	return out, nil
}

func DecodeRoomSubjectUpdate(data []byte, maxBytes int) (entity.SubjectSyncUpdate, error) {
	if maxBytes <= 0 {
		maxBytes = core.DefaultLimits().MaxComponentBytes
	}
	if len(data) < roomSubjectHeaderBytes || len(data) > maxBytes || binary.BigEndian.Uint32(data[0:4]) != roomSubjectMagic || binary.BigEndian.Uint16(data[4:6]) != RoomWireVersion {
		return entity.SubjectSyncUpdate{}, ErrRoomWireFrame
	}
	flags := binary.BigEndian.Uint16(data[6:8])
	if flags & ^roomSubjectFlagFull != 0 || data[51] != 0 {
		return entity.SubjectSyncUpdate{}, ErrRoomWireFrame
	}
	namespaceLength := int(binary.BigEndian.Uint16(data[56:58]))
	profileLength := int(binary.BigEndian.Uint16(data[58:60]))
	payloadLength := int(binary.BigEndian.Uint32(data[60:64]))
	if roomSubjectHeaderBytes+namespaceLength+profileLength+payloadLength != len(data) {
		return entity.SubjectSyncUpdate{}, ErrRoomWireFrame
	}
	offset := roomSubjectHeaderBytes
	update := entity.SubjectSyncUpdate{
		SubjectID: int64(binary.BigEndian.Uint64(data[8:16])), SubjectKind: binary.BigEndian.Uint32(data[16:20]),
		Version: binary.BigEndian.Uint64(data[20:28]), BaseVersion: binary.BigEndian.Uint64(data[28:36]),
		Mask: binary.BigEndian.Uint64(data[36:44]), Reason: binary.BigEndian.Uint32(data[44:48]), Full: flags&roomSubjectFlagFull != 0,
		Namespace: string(data[offset : offset+namespaceLength]),
	}
	offset += namespaceLength
	update.Profile = entity.SyncProfile{Key: string(data[offset : offset+profileLength]), LOD: data[50], SchemaVersion: binary.BigEndian.Uint32(data[52:56])}.Normalize()
	offset += profileLength
	update.Payload = entity.CopyFrozenSyncPayload(binary.BigEndian.Uint16(data[48:50]), data[offset:offset+payloadLength])
	if update.SubjectID == 0 {
		return entity.SubjectSyncUpdate{}, ErrRoomWireFrame
	}
	return update, nil
}

func DecodeRoomWireFrame(data []byte, limits core.Limits) (uint64, uint64, core.DeltaFrame, error) {
	if len(data) < roomWireHeaderBytes || binary.BigEndian.Uint32(data[0:4]) != roomWireMagic || binary.BigEndian.Uint16(data[4:6]) != RoomWireVersion || binary.BigEndian.Uint16(data[6:8]) != 0 {
		return 0, 0, core.DeltaFrame{}, ErrRoomWireFrame
	}
	frame := binary.BigEndian.Uint64(data[8:16])
	sequence := binary.BigEndian.Uint64(data[16:24])
	innerLength := int(binary.BigEndian.Uint32(data[24:28]))
	if frame == 0 || sequence == 0 || innerLength != len(data)-roomWireHeaderBytes {
		return 0, 0, core.DeltaFrame{}, ErrRoomWireFrame
	}
	delta, err := core.DecodeFrame(data[roomWireHeaderBytes:], limits)
	if err != nil {
		return 0, 0, core.DeltaFrame{}, err
	}
	return frame, sequence, delta, nil
}

func encodeRoomWireFrame(frame, sequence uint64, inner []byte) []byte {
	out := make([]byte, roomWireHeaderBytes, roomWireHeaderBytes+len(inner))
	binary.BigEndian.PutUint32(out[0:4], roomWireMagic)
	binary.BigEndian.PutUint16(out[4:6], RoomWireVersion)
	binary.BigEndian.PutUint64(out[8:16], frame)
	binary.BigEndian.PutUint64(out[16:24], sequence)
	binary.BigEndian.PutUint32(out[24:28], uint32(len(inner)))
	return append(out, inner...)
}

func roomWireClock(frame uint64) (uint32, uint32, uint32) {
	const period = uint64(^uint32(0) - 1)
	value := frame - 1
	epoch := uint32(value/period) + 1
	tick := uint32(value%period) + 2
	return epoch, tick, tick - 1
}

func nonzeroSequence(sequence uint64) uint32 {
	return uint32((sequence-1)%uint64(^uint32(0))) + 1
}

func mergeRoomLimits(value, defaults core.Limits) core.Limits {
	if value.MaxObjects <= 0 {
		value.MaxObjects = defaults.MaxObjects
	}
	if value.MaxComponentsPerObject <= 0 {
		value.MaxComponentsPerObject = defaults.MaxComponentsPerObject
	}
	if value.MaxComponentBytes <= 0 {
		value.MaxComponentBytes = defaults.MaxComponentBytes
	}
	if value.MaxFrameBytes <= 0 {
		value.MaxFrameBytes = defaults.MaxFrameBytes
	}
	if value.MaxDatagramBytes <= 0 {
		value.MaxDatagramBytes = defaults.MaxDatagramBytes
	}
	if value.MaxFragments <= 0 {
		value.MaxFragments = defaults.MaxFragments
	}
	if value.MaxInflightFrames <= 0 {
		value.MaxInflightFrames = defaults.MaxInflightFrames
	}
	return value
}

func cloneRoomObjectRefs(source *roomObjectRefs) *roomObjectRefs {
	if source == nil {
		return &roomObjectRefs{objects: make(map[int64]core.ObjectRef), generations: make(map[uint16]uint16), next: 1}
	}
	clone := &roomObjectRefs{
		objects: make(map[int64]core.ObjectRef, len(source.objects)), generations: make(map[uint16]uint16, len(source.generations)),
		free: append([]uint16(nil), source.free...), next: source.next, subscriber: source.subscriber,
	}
	for id, ref := range source.objects {
		clone.objects[id] = ref
	}
	for id, generation := range source.generations {
		clone.generations[id] = generation
	}
	return clone
}

func (state *roomObjectRefs) allocate(subjectID int64, maxObjects int) (core.ObjectRef, error) {
	if len(state.objects) >= maxObjects {
		return core.ObjectRef{}, core.ErrObjectLimit
	}
	var id uint16
	if len(state.free) != 0 {
		id = state.free[len(state.free)-1]
		state.free = state.free[:len(state.free)-1]
	} else {
		id = state.next
		if id == 0 {
			return core.ObjectRef{}, core.ErrObjectLimit
		}
		state.next++
	}
	generation := state.generations[id] + 1
	if generation == 0 {
		generation = 1
	}
	state.generations[id] = generation
	ref := core.ObjectRef{ID: id, Generation: generation}
	state.objects[subjectID] = ref
	return ref, nil
}

func (state *roomObjectRefs) release(subjectID int64) {
	ref, exists := state.objects[subjectID]
	if !exists {
		return
	}
	delete(state.objects, subjectID)
	state.free = append(state.free, ref.ID)
}

var _ ReliableRoomFrameSink = (*RoomTransportSink)(nil)
var _ RoomSubjectLifecycle = (*RoomTransportSink)(nil)
var _ RoomSubscriberLifecycle = (*RoomTransportSink)(nil)
var _ RoomLifecycle = (*RoomTransportSink)(nil)
