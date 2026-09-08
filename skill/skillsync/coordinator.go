package skillsync

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var (
	ErrRuntimeRequired    = errors.New("skillsync: runtime is required")
	ErrHistoryRequired    = errors.New("skillsync: history is required")
	ErrPublisherRequired  = errors.New("skillsync: publisher is required")
	ErrVisibilityRequired = errors.New("skillsync: visibility policy is required")
	ErrManifestMissing    = errors.New("skillsync: presentation manifest is missing")
	ErrTopicUnsupported   = errors.New("skillsync: topic is unsupported")
	ErrObserverClosed     = errors.New("skillsync: observer is closed")
	ErrCoordinatorInvalid = errors.New("skillsync: coordinator is not initialized")
)

type PacketPublisher interface{ Publish(syncstream.Packet) error }

type VisibilityPolicy interface {
	FilterStateSnapshot(syncstream.Observer, skill.RuntimeStateSnapshot) (skill.RuntimeStateSnapshot, error)
	FilterStateMutation(syncstream.Observer, skill.StateMutation) (skill.StateMutation, bool, error)
	FilterPresentation(syncstream.Observer, skill.PresentationEvent) (skill.PresentationEvent, bool, error)
}

type AllowAllVisibility struct{}

func (AllowAllVisibility) FilterStateSnapshot(_ syncstream.Observer, snapshot skill.RuntimeStateSnapshot) (skill.RuntimeStateSnapshot, error) {
	return snapshot, nil
}
func (AllowAllVisibility) FilterStateMutation(_ syncstream.Observer, mutation skill.StateMutation) (skill.StateMutation, bool, error) {
	return mutation, true, nil
}
func (AllowAllVisibility) FilterPresentation(_ syncstream.Observer, event skill.PresentationEvent) (skill.PresentationEvent, bool, error) {
	return event, true, nil
}

type CoordinatorOptions struct {
	Runtime              *skill.Runtime
	History              *syncstream.History
	Publisher            PacketPublisher
	Projector            Projector
	Visibility           VisibilityPolicy
	Outbox               *Outbox
	RequireDurableOutbox bool
	MaxPacketsPerFlush   int
}

type observerKey struct {
	observer syncstream.Observer
	key      int64
}
type sourceCursor struct {
	state        uint64
	presentation uint64
}

type viewLockEntry struct {
	mutex sync.Mutex
	refs  int
}

type CoordinatorMetrics struct {
	Published          uint64
	PublishFailures    uint64
	Filtered           uint64
	VisibilityFailures uint64
	SnapshotRecoveries uint64
}

type coordinatorCounters struct {
	published          atomic.Uint64
	publishFailures    atomic.Uint64
	filtered           atomic.Uint64
	visibilityFailures atomic.Uint64
	snapshotRecoveries atomic.Uint64
}

// Coordinator serializes preparation per observer/key but never holds its
// global mutex while invoking VisibilityPolicy, PacketPublisher, or Runtime.
type Coordinator struct {
	mutex            sync.RWMutex
	runtime          *skill.Runtime
	history          *syncstream.History
	publisher        PacketPublisher
	projector        Projector
	visibility       VisibilityPolicy
	outbox           *Outbox
	maxPackets       int
	cursors          map[observerKey]sourceCursor
	plans            map[int64]skill.PresentationPlan
	viewLocks        map[observerKey]*viewLockEntry
	closedObservers  map[syncstream.Observer]struct{}
	closingObservers map[syncstream.Observer]struct{}
	counters         coordinatorCounters
}

func NewCoordinator(options CoordinatorOptions) (*Coordinator, error) {
	if options.Runtime == nil {
		return nil, ErrRuntimeRequired
	}
	if options.History == nil {
		return nil, ErrHistoryRequired
	}
	if options.Publisher == nil {
		return nil, ErrPublisherRequired
	}
	if options.Projector.SchemaVersion == 0 {
		return nil, ErrSchemaVersionRequired
	}
	if options.Visibility == nil {
		return nil, ErrVisibilityRequired
	}
	if options.MaxPacketsPerFlush <= 0 {
		options.MaxPacketsPerFlush = 256
	}
	if options.Outbox == nil {
		var err error
		options.Outbox, err = NewOutbox(OutboxOptions{RequireDurable: options.RequireDurableOutbox})
		if err != nil {
			return nil, err
		}
	} else if options.RequireDurableOutbox && options.Outbox.store == nil {
		return nil, ErrOutboxStoreRequired
	}
	if err := options.Outbox.Reconcile(options.History.Export()); err != nil {
		return nil, err
	}
	return &Coordinator{runtime: options.Runtime, history: options.History, publisher: options.Publisher, projector: options.Projector, visibility: options.Visibility, outbox: options.Outbox, maxPackets: options.MaxPacketsPerFlush, cursors: make(map[observerKey]sourceCursor), plans: make(map[int64]skill.PresentationPlan), viewLocks: make(map[observerKey]*viewLockEntry), closedObservers: make(map[syncstream.Observer]struct{}), closingObservers: make(map[syncstream.Observer]struct{})}, nil
}

func (coordinator *Coordinator) acquireView(key observerKey) (func(), error) {
	coordinator.mutex.Lock()
	if _, closed := coordinator.closedObservers[key.observer]; closed {
		coordinator.mutex.Unlock()
		return nil, ErrObserverClosed
	}
	entry := coordinator.viewLocks[key]
	if entry == nil {
		entry = &viewLockEntry{}
		coordinator.viewLocks[key] = entry
	}
	entry.refs++
	coordinator.mutex.Unlock()
	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		coordinator.mutex.Lock()
		entry.refs--
		if entry.refs == 0 && coordinator.viewLocks[key] == entry {
			delete(coordinator.viewLocks, key)
		}
		coordinator.mutex.Unlock()
	}, nil
}

func (coordinator *Coordinator) OpenObserver(observer syncstream.Observer) error {
	coordinator.mutex.Lock()
	defer coordinator.mutex.Unlock()
	if _, closing := coordinator.closingObservers[observer]; closing {
		return ErrApplyInProgress
	}
	for key := range coordinator.viewLocks {
		if key.observer == observer {
			return ErrApplyInProgress
		}
	}
	delete(coordinator.closedObservers, observer)
	return nil
}

func (coordinator *Coordinator) RegisterProgram(key int64, program *skill.Program) {
	coordinator.mutex.Lock()
	coordinator.plans[key] = skill.InspectPresentationPlan(program)
	coordinator.mutex.Unlock()
}

func (coordinator *Coordinator) plan(key int64) (skill.PresentationPlan, bool) {
	coordinator.mutex.RLock()
	defer coordinator.mutex.RUnlock()
	plan, ok := coordinator.plans[key]
	return plan, ok
}

func (coordinator *Coordinator) cursor(key observerKey) sourceCursor {
	coordinator.mutex.RLock()
	defer coordinator.mutex.RUnlock()
	return coordinator.cursors[key]
}
func (coordinator *Coordinator) setCursor(key observerKey, cursor sourceCursor) {
	coordinator.mutex.Lock()
	coordinator.cursors[key] = cursor
	coordinator.mutex.Unlock()
}

func (coordinator *Coordinator) PublishManifest(observer syncstream.Observer, key int64) error {
	view := observerKey{observer, key}
	release, err := coordinator.acquireView(view)
	if err != nil {
		return err
	}
	plan, ok := coordinator.plan(key)
	if !ok {
		release()
		return ErrManifestMissing
	}
	packet, err := coordinator.projector.ManifestPacket(observer, key, plan)
	if err == nil {
		_, err = coordinator.appendPending(packet)
	}
	release()
	if err != nil {
		return err
	}
	return coordinator.publishDue(observer, syncstream.Stream{Topic: TopicManifest, Key: key})
}

func (coordinator *Coordinator) PublishSnapshot(observer syncstream.Observer, key int64) error {
	view := observerKey{observer, key}
	release, err := coordinator.acquireView(view)
	if err != nil {
		return err
	}
	snapshot, err := coordinator.visibility.FilterStateSnapshot(observer, coordinator.runtime.StateSnapshot())
	if err != nil {
		coordinator.counters.visibilityFailures.Add(1)
		release()
		return err
	}
	packet, err := coordinator.projector.StateSnapshotPacket(observer, key, snapshot)
	if err == nil {
		_, err = coordinator.appendPending(packet)
	}
	if err == nil {
		cursor := coordinator.cursor(view)
		cursor.state = snapshot.LatestStateMutationSequence
		coordinator.setCursor(view, cursor)
	}
	release()
	if err != nil {
		return err
	}
	return coordinator.publishDue(observer, syncstream.Stream{Topic: TopicState, Key: key})
}

func (coordinator *Coordinator) Flush(observer syncstream.Observer, key int64) error {
	view := observerKey{observer, key}
	release, err := coordinator.acquireView(view)
	if err != nil {
		return err
	}
	err = coordinator.prepareFlush(view)
	release()
	if err != nil {
		return err
	}
	return coordinator.publishObserver(observer)
}

func (coordinator *Coordinator) prepareFlush(view observerKey) error {
	cursor := coordinator.cursor(view)
	remaining := coordinator.maxPackets
	state := coordinator.runtime.StateDeltas(cursor.state, remaining)
	if state.CursorExpired {
		snapshot, err := coordinator.visibility.FilterStateSnapshot(view.observer, coordinator.runtime.StateSnapshot())
		if err != nil {
			coordinator.counters.visibilityFailures.Add(1)
			return err
		}
		packet, err := coordinator.projector.StateSnapshotPacket(view.observer, view.key, snapshot)
		if err != nil {
			return err
		}
		if _, err = coordinator.appendPending(packet); err != nil {
			return err
		}
		cursor.state = snapshot.LatestStateMutationSequence
		coordinator.setCursor(view, cursor)
		coordinator.counters.snapshotRecoveries.Add(1)
		remaining--
		if remaining == 0 {
			return nil
		}
	} else {
		for _, mutation := range state.Mutations {
			filtered, allowed, err := coordinator.visibility.FilterStateMutation(view.observer, mutation)
			if err != nil {
				coordinator.counters.visibilityFailures.Add(1)
				return err
			}
			if !allowed {
				cursor.state = mutation.Sequence
				coordinator.setCursor(view, cursor)
				coordinator.counters.filtered.Add(1)
				continue
			}
			packet, err := coordinator.projector.StateDeltaPacket(view.observer, view.key, filtered)
			if err != nil {
				return err
			}
			if _, err = coordinator.appendPending(packet); err != nil {
				return err
			}
			cursor.state = mutation.Sequence
			coordinator.setCursor(view, cursor)
			remaining--
			if remaining == 0 {
				return nil
			}
		}
	}
	presentation := coordinator.runtime.PollPresentation(cursor.presentation, remaining)
	if presentation.CursorExpired {
		snapshot := coordinator.runtime.PresentationSnapshot()
		packet, err := coordinator.projector.PresentationResetPacket(view.observer, view.key, snapshot)
		if err != nil {
			return err
		}
		if _, err = coordinator.appendPending(packet); err != nil {
			return err
		}
		cursor.presentation = snapshot.LatestPresentationSequence
		coordinator.setCursor(view, cursor)
		coordinator.counters.snapshotRecoveries.Add(1)
		return nil
	}
	for _, event := range presentation.Events {
		filtered, allowed, err := coordinator.visibility.FilterPresentation(view.observer, event)
		if err != nil {
			coordinator.counters.visibilityFailures.Add(1)
			return err
		}
		if !allowed {
			cursor.presentation = event.Sequence
			coordinator.setCursor(view, cursor)
			coordinator.counters.filtered.Add(1)
			continue
		}
		packet, err := coordinator.projector.PresentationPacket(view.observer, view.key, filtered)
		if err != nil {
			return err
		}
		if _, err = coordinator.appendPending(packet); err != nil {
			return err
		}
		cursor.presentation = event.Sequence
		coordinator.setCursor(view, cursor)
	}
	return nil
}

func (coordinator *Coordinator) appendPending(packet syncstream.Packet) (syncstream.Packet, error) {
	packet, err := coordinator.history.Append(packet)
	if err != nil {
		return syncstream.Packet{}, err
	}
	if err := coordinator.outbox.Put(packet); err != nil {
		// History is the authoritative WAL. Reconciliation closes the small
		// history->outbox crash window without generating a duplicate source
		// mutation on the next flush.
		if reconcileErr := coordinator.outbox.Reconcile(coordinator.history.Export()); reconcileErr != nil {
			return syncstream.Packet{}, errors.Join(err, reconcileErr)
		}
	}
	return packet, nil
}

func (coordinator *Coordinator) Acknowledge(observer syncstream.Observer, stream syncstream.Stream, epoch, sequence uint64) error {
	if coordinator == nil || coordinator.history == nil || coordinator.outbox == nil {
		return ErrCoordinatorInvalid
	}
	release, err := coordinator.acquireView(observerKey{observer: observer, key: stream.Key})
	if err != nil {
		return err
	}
	defer release()
	// Validate before deleting the derived copy. latest can only advance while
	// this view is held, so a valid sequence cannot become invalid here.
	if epoch != coordinator.history.Epoch() {
		return syncstream.ErrAckEpochMismatch
	}
	if sequence > coordinator.history.Status(observer, stream).LatestSequence {
		return syncstream.ErrAckAhead
	}
	// Delete the derived outbox copy first. If History ACK then fails, startup
	// reconciliation can recreate it. The opposite order can permanently orphan
	// a stale outbox record after a crash or store failure.
	if err := coordinator.outbox.Acknowledge(observer, stream, epoch, sequence); err != nil {
		return errors.Join(err, coordinator.outbox.Reconcile(coordinator.history.Export()))
	}
	if err := coordinator.history.AcknowledgeEpoch(observer, stream, epoch, sequence); err != nil {
		// Repair immediately as well as retaining History as the crash-recovery
		// source. Joining both failures preserves the primary WAL error.
		return errors.Join(err, coordinator.outbox.Reconcile(coordinator.history.Export()))
	}
	return nil
}

type snapshotProviderFunc func(syncstream.ResyncRequest) (syncstream.Packet, error)

func (provider snapshotProviderFunc) Snapshot(request syncstream.ResyncRequest) (syncstream.Packet, error) {
	return provider(request)
}

func (coordinator *Coordinator) Recover(request syncstream.ResyncRequest) (syncstream.ResyncResult, error) {
	view := observerKey{request.Observer, request.Stream.Key}
	release, err := coordinator.acquireView(view)
	if err != nil {
		return syncstream.ResyncResult{}, err
	}
	result, err := coordinator.history.Recover(request, snapshotProviderFunc(coordinator.snapshotPacket))
	if err == nil {
		for _, packet := range result.Packets {
			if putErr := coordinator.outbox.Put(packet); putErr != nil {
				err = putErr
				break
			}
		}
	}
	release()
	if err != nil {
		return result, err
	}
	if result.Reason != syncstream.ResyncNone && len(result.Packets) == 1 && result.Packets[0].Full {
		coordinator.counters.snapshotRecoveries.Add(1)
	}
	err = coordinator.publishNow(request.Observer, request.Stream)
	return result, err
}

func (coordinator *Coordinator) snapshotPacket(request syncstream.ResyncRequest) (syncstream.Packet, error) {
	switch request.Stream.Topic {
	case TopicManifest:
		plan, ok := coordinator.plan(request.Stream.Key)
		if !ok {
			return syncstream.Packet{}, ErrManifestMissing
		}
		return coordinator.projector.ManifestPacket(request.Observer, request.Stream.Key, plan)
	case TopicState:
		snapshot, err := coordinator.visibility.FilterStateSnapshot(request.Observer, coordinator.runtime.StateSnapshot())
		if err != nil {
			coordinator.counters.visibilityFailures.Add(1)
			return syncstream.Packet{}, err
		}
		return coordinator.projector.StateSnapshotPacket(request.Observer, request.Stream.Key, snapshot)
	case TopicPresentation:
		return coordinator.projector.PresentationResetPacket(request.Observer, request.Stream.Key, coordinator.runtime.PresentationSnapshot())
	default:
		return syncstream.Packet{}, fmt.Errorf("%w: %s", ErrTopicUnsupported, request.Stream.Topic)
	}
}

func (coordinator *Coordinator) publishDue(observer syncstream.Observer, stream syncstream.Stream) error {
	before := coordinator.outbox.Metrics()
	err := coordinator.outbox.PublishDue(coordinator.publisher, time.Now(), &observer, &stream)
	coordinator.capturePublishMetrics(before, coordinator.outbox.Metrics())
	return err
}
func (coordinator *Coordinator) publishNow(observer syncstream.Observer, stream syncstream.Stream) error {
	before := coordinator.outbox.Metrics()
	err := coordinator.outbox.PublishNow(coordinator.publisher, time.Now(), &observer, &stream)
	coordinator.capturePublishMetrics(before, coordinator.outbox.Metrics())
	return err
}
func (coordinator *Coordinator) publishObserver(observer syncstream.Observer) error {
	before := coordinator.outbox.Metrics()
	err := coordinator.outbox.PublishDue(coordinator.publisher, time.Now(), &observer, nil)
	coordinator.capturePublishMetrics(before, coordinator.outbox.Metrics())
	return err
}
func (coordinator *Coordinator) RetryPending(now time.Time) error {
	before := coordinator.outbox.Metrics()
	err := coordinator.outbox.PublishDue(coordinator.publisher, now, nil, nil)
	coordinator.capturePublishMetrics(before, coordinator.outbox.Metrics())
	return err
}

// ReconcilePending explicitly repairs the history/outbox crash window. Normal
// retries avoid an O(history) rescan: construction reconciles once and each
// live append persists directly to the outbox.
func (coordinator *Coordinator) ReconcilePending() error {
	if coordinator == nil || coordinator.outbox == nil || coordinator.history == nil {
		return ErrCoordinatorInvalid
	}
	return coordinator.outbox.Reconcile(coordinator.history.Export())
}
func (coordinator *Coordinator) capturePublishMetrics(before, after OutboxMetrics) {
	coordinator.counters.published.Add(after.PublishSuccesses - before.PublishSuccesses)
	coordinator.counters.publishFailures.Add(after.PublishFailures - before.PublishFailures)
}

func (coordinator *Coordinator) CloseObserver(observer syncstream.Observer) error {
	coordinator.mutex.Lock()
	if _, closing := coordinator.closingObservers[observer]; closing {
		coordinator.mutex.Unlock()
		return ErrApplyInProgress
	}
	coordinator.closedObservers[observer] = struct{}{}
	coordinator.closingObservers[observer] = struct{}{}
	type lockedView struct {
		key   observerKey
		entry *viewLockEntry
	}
	views := make([]lockedView, 0)
	for key, entry := range coordinator.viewLocks {
		if key.observer == observer {
			entry.refs++ // close operation owns a reference
			views = append(views, lockedView{key, entry})
		}
	}
	coordinator.mutex.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].key.key < views[j].key.key })
	for _, view := range views {
		view.entry.mutex.Lock()
	}
	defer func() {
		for index := len(views) - 1; index >= 0; index-- {
			views[index].entry.mutex.Unlock()
		}
		coordinator.mutex.Lock()
		delete(coordinator.closingObservers, observer)
		for _, view := range views {
			view.entry.refs--
			if view.entry.refs == 0 && coordinator.viewLocks[view.key] == view.entry {
				delete(coordinator.viewLocks, view.key)
			}
		}
		coordinator.mutex.Unlock()
	}()
	if err := coordinator.outbox.DiscardObserver(observer); err != nil {
		return errors.Join(err, coordinator.outbox.Reconcile(coordinator.history.Export()))
	}
	// History remains the repair source until every outbox record is durably
	// removed. A crash after this point is safe: retained History can reconcile.
	if _, err := coordinator.history.DeleteObserver(observer); err != nil {
		return err
	}
	coordinator.mutex.Lock()
	for key := range coordinator.cursors {
		if key.observer == observer {
			delete(coordinator.cursors, key)
		}
	}
	coordinator.mutex.Unlock()
	return nil
}

func (coordinator *Coordinator) Metrics() CoordinatorMetrics {
	return CoordinatorMetrics{Published: coordinator.counters.published.Load(), PublishFailures: coordinator.counters.publishFailures.Load(), Filtered: coordinator.counters.filtered.Load(), VisibilityFailures: coordinator.counters.visibilityFailures.Load(), SnapshotRecoveries: coordinator.counters.snapshotRecoveries.Load()}
}
