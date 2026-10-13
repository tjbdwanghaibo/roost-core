package skillsync

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

var (
	ErrRuntimeRequired     = errors.New("skillsync: runtime is required")
	ErrHistoryRequired     = errors.New("skillsync: history is required")
	ErrPublisherRequired   = errors.New("skillsync: publisher is required")
	ErrVisibilityRequired  = errors.New("skillsync: visibility policy is required")
	ErrManifestMissing     = errors.New("skillsync: presentation manifest is missing")
	ErrTopicUnsupported    = errors.New("skillsync: topic is unsupported")
	ErrObserverClosed      = errors.New("skillsync: observer is closed")
	ErrCoordinatorInvalid  = errors.New("skillsync: coordinator is not initialized")
	ErrCoordinatorCapacity = errors.New("skillsync: coordinator resource capacity exceeded")
	ErrManifestConflict    = errors.New("skillsync: key already has a different manifest")
)

// PacketPublisher 把包交给传输层。
type PacketPublisher interface{ Publish(syncstream.Packet) error }

// VisibilityPolicy 按 observer 过滤状态快照、状态增量与表现事件；第二个返回值为 false 表示整条不下发。
type VisibilityPolicy interface {
	FilterStateSnapshot(syncstream.Observer, skill.RuntimeStateSnapshot) (skill.RuntimeStateSnapshot, error)
	FilterStateMutation(syncstream.Observer, skill.StateMutation) (skill.StateMutation, bool, error)
	FilterPresentation(syncstream.Observer, skill.PresentationEvent) (skill.PresentationEvent, bool, error)
}

// AllowAllVisibility 是不做任何过滤的 VisibilityPolicy。
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

// CoordinatorOptions 是 Coordinator 的依赖与上限；Runtime、History、Publisher、Projector 与 Visibility 必填。
type CoordinatorOptions struct {
	Runtime              *skill.Runtime
	History              *syncstream.History
	Publisher            PacketPublisher
	Projector            Projector
	Visibility           VisibilityPolicy
	Outbox               *Outbox
	RequireDurableOutbox bool
	MaxPacketsPerFlush   int
	MaxObservers         int
	MaxPrograms          int
}

type observerKey struct {
	observer syncstream.Observer
	key      int64
}
type sourceCursor struct {
	state             uint64
	presentation      uint64
	resetPresentation bool
}

type viewLockEntry struct {
	mutex sync.Mutex
	refs  int
}

// CoordinatorMetrics 是 Coordinator 的累计计数。
type CoordinatorMetrics struct {
	Published          uint64
	PublishFailures    uint64
	Filtered           uint64
	VisibilityFailures uint64
	SnapshotRecoveries uint64
}

type coordinatorCounters struct {
	filtered           atomic.Uint64
	visibilityFailures atomic.Uint64
	snapshotRecoveries atomic.Uint64
}

// Coordinator 按 observer / 键串行化准备工作，但调用 VisibilityPolicy、PacketPublisher 或 Runtime 时从不持有全局锁。
type Coordinator struct {
	mutex sync.RWMutex
	// journalMutex 串行 History/outbox 的交接和修复；不覆盖网络发送。
	journalMutex      sync.Mutex
	repairPending     bool
	historyEpoch      uint64
	runtime           *skill.Runtime
	history           *syncstream.History
	publisher         PacketPublisher
	projector         Projector
	visibility        VisibilityPolicy
	outbox            *Outbox
	maxPackets        int
	maxObservers      int
	maxPrograms       int
	cursors           map[observerKey]sourceCursor
	plans             map[int64]skill.PresentationPlan
	viewLocks         map[observerKey]*viewLockEntry
	activeObservers   map[syncstream.Observer]struct{}
	retiringObservers map[syncstream.Observer]struct{}
	closingObservers  map[syncstream.Observer]struct{}
	counters          coordinatorCounters
}

// NewCoordinator 校验依赖、补齐上限默认值并构造 Coordinator（构造时对账一次 history 与 outbox）。
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
	if options.MaxObservers <= 0 {
		options.MaxObservers = 4096
	}
	if options.MaxPrograms <= 0 {
		options.MaxPrograms = 1024
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
	return &Coordinator{historyEpoch: options.History.Epoch(), runtime: options.Runtime, history: options.History, publisher: options.Publisher, projector: options.Projector, visibility: options.Visibility, outbox: options.Outbox, maxPackets: options.MaxPacketsPerFlush, maxObservers: options.MaxObservers, maxPrograms: options.MaxPrograms, cursors: make(map[observerKey]sourceCursor), plans: make(map[int64]skill.PresentationPlan), viewLocks: make(map[observerKey]*viewLockEntry), activeObservers: make(map[syncstream.Observer]struct{}), retiringObservers: make(map[syncstream.Observer]struct{}), closingObservers: make(map[syncstream.Observer]struct{})}, nil
}

func (coordinator *Coordinator) acquireView(key observerKey) (func(), error) {
	coordinator.mutex.Lock()
	if _, open := coordinator.activeObservers[key.observer]; !open {
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
	release := func() {
		entry.mutex.Unlock()
		coordinator.mutex.Lock()
		entry.refs--
		if entry.refs == 0 && coordinator.viewLocks[key] == entry {
			delete(coordinator.viewLocks, key)
		}
		coordinator.mutex.Unlock()
	}
	// 等待 view 锁期间可能已被 CloseObserver 关闭，拿锁后必须重新核对，
	// 否则旧请求会在关闭完成后重新向 History/outbox 入账。
	if !coordinator.observerOpen(key.observer) {
		release()
		return nil, ErrObserverClosed
	}
	return release, nil
}

// OpenObserver 登记一个 observer；已登记时是空操作。observer 正在关闭时返回 ErrApplyInProgress，超过
// MaxObservers 时返回 ErrCoordinatorCapacity。
func (coordinator *Coordinator) OpenObserver(observer syncstream.Observer) error {
	coordinator.mutex.Lock()
	defer coordinator.mutex.Unlock()
	if _, closing := coordinator.closingObservers[observer]; closing {
		return ErrApplyInProgress
	}
	if _, retiring := coordinator.retiringObservers[observer]; retiring {
		return ErrApplyInProgress
	}
	if _, open := coordinator.activeObservers[observer]; open {
		return nil
	}
	for key := range coordinator.viewLocks {
		if key.observer == observer {
			return ErrApplyInProgress
		}
	}
	if len(coordinator.activeObservers)+len(coordinator.retiringObservers) >= coordinator.maxObservers {
		return ErrCoordinatorCapacity
	}
	coordinator.activeObservers[observer] = struct{}{}
	return nil
}

// RegisterProgram 的 key 标识整个 Runtime 的同步视图，不筛选 Runtime 中的 Program。
// 一个 key 对应一个 manifest；不同计划不能静默覆盖客户端仍在使用的身份。
func (coordinator *Coordinator) RegisterProgram(key int64, program *skill.Program) error {
	if program == nil {
		return ErrManifestMissing
	}
	plan := skill.InspectPresentationPlan(program)
	coordinator.mutex.Lock()
	defer coordinator.mutex.Unlock()
	if previous, exists := coordinator.plans[key]; exists {
		if reflect.DeepEqual(previous, plan) {
			return nil
		}
		return ErrManifestConflict
	}
	if len(coordinator.plans) >= coordinator.maxPrograms {
		return ErrCoordinatorCapacity
	}
	coordinator.plans[key] = plan
	return nil
}

// UnregisterProgram 应在该 key 停止生产后调用；已入账的包仍按原身份交付。
func (coordinator *Coordinator) UnregisterProgram(key int64) {
	coordinator.mutex.Lock()
	delete(coordinator.plans, key)
	coordinator.mutex.Unlock()
}

func (coordinator *Coordinator) observerOpen(observer syncstream.Observer) bool {
	coordinator.mutex.RLock()
	defer coordinator.mutex.RUnlock()
	_, open := coordinator.activeObservers[observer]
	return open
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

// PublishManifest 把 key 对应的表现清单追加到 observer 的流并立即尝试发布。
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

// PublishSnapshot 按可见性过滤后的全量状态快照开始 observer 的状态流，并重置该视图的增量游标。
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
		packet, err = coordinator.appendPending(packet)
	}
	if packet.Sequence != 0 {
		cursor := coordinator.cursor(view)
		cursor.state = snapshot.LatestStateMutationSequence
		cursor.presentation = snapshot.LatestPresentationSequence
		cursor.resetPresentation = true
		coordinator.setCursor(view, cursor)
	}
	release()
	if err != nil {
		return err
	}
	return coordinator.publishDue(observer, syncstream.Stream{Topic: TopicState, Key: key})
}

// Flush 把 Runtime 新产生的状态增量与表现事件经可见性过滤后追加到 observer 的流，再发布到期的包。
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
	if err := coordinator.repairOutbox(); err != nil {
		return err
	}
	cursor := coordinator.cursor(view)
	remaining := coordinator.maxPackets
	if cursor.resetPresentation {
		snapshot, err := coordinator.presentationReset(view.observer)
		if err != nil {
			return err
		}
		packet, err := coordinator.projector.PresentationResetPacket(view.observer, view.key, snapshot)
		if err != nil {
			return err
		}
		accepted, err := coordinator.appendPending(packet)
		if accepted.Sequence != 0 {
			cursor.presentation = snapshot.LatestPresentationSequence
			cursor.resetPresentation = false
			coordinator.setCursor(view, cursor)
		}
		if err != nil {
			return err
		}
		remaining--
		if remaining == 0 {
			return nil
		}
	}
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
		accepted, err := coordinator.appendPending(packet)
		if accepted.Sequence != 0 {
			cursor.state = snapshot.LatestStateMutationSequence
			coordinator.setCursor(view, cursor)
		}
		if err != nil {
			return err
		}
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
			accepted, err := coordinator.appendPending(packet)
			if accepted.Sequence != 0 {
				cursor.state = mutation.Sequence
				coordinator.setCursor(view, cursor)
			}
			if err != nil {
				return err
			}
			remaining--
			if remaining == 0 {
				return nil
			}
		}
	}
	presentation := coordinator.runtime.PollPresentation(cursor.presentation, remaining)
	if presentation.CursorExpired {
		snapshot, err := coordinator.presentationReset(view.observer)
		if err != nil {
			return err
		}
		packet, err := coordinator.projector.PresentationResetPacket(view.observer, view.key, snapshot)
		if err != nil {
			return err
		}
		accepted, err := coordinator.appendPending(packet)
		if accepted.Sequence != 0 {
			cursor.presentation = snapshot.LatestPresentationSequence
			coordinator.setCursor(view, cursor)
		}
		if err != nil {
			return err
		}
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
		accepted, err := coordinator.appendPending(packet)
		if accepted.Sequence != 0 {
			cursor.presentation = event.Sequence
			coordinator.setCursor(view, cursor)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// appendPending 返回非零 Sequence 就表示 History 已接受。outbox 失败不能撤销这份身份，
// 调用方必须推进对应源游标；下一次追加前先修复派生 outbox，避免重复记录同一源变化。
func (coordinator *Coordinator) appendPending(packet syncstream.Packet) (syncstream.Packet, error) {
	coordinator.journalMutex.Lock()
	defer coordinator.journalMutex.Unlock()
	if coordinator.repairPending || coordinator.historyEpoch != coordinator.history.Epoch() {
		if err := coordinator.reconcileOutboxLocked(); err != nil {
			return syncstream.Packet{}, err
		}
	}
	packet, err := coordinator.history.Append(packet)
	if err != nil {
		return syncstream.Packet{}, err
	}
	if err := coordinator.outbox.Put(packet); err != nil {
		if repairErr := coordinator.reconcileOutboxLocked(); repairErr != nil {
			return packet, errors.Join(err, repairErr)
		}
	}
	return packet, nil
}

func (coordinator *Coordinator) reconcileOutboxLocked() error {
	snapshot := coordinator.history.Export()
	err := coordinator.outbox.Reconcile(snapshot)
	coordinator.repairPending = err != nil
	if err == nil && coordinator.historyEpoch != snapshot.Epoch {
		coordinator.historyEpoch = snapshot.Epoch
		coordinator.mutex.Lock()
		clear(coordinator.cursors)
		coordinator.mutex.Unlock()
	}
	return err
}

func (coordinator *Coordinator) repairOutbox() error {
	coordinator.journalMutex.Lock()
	defer coordinator.journalMutex.Unlock()
	if !coordinator.repairPending && coordinator.historyEpoch == coordinator.history.Epoch() {
		return nil
	}
	return coordinator.reconcileOutboxLocked()
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
	coordinator.journalMutex.Lock()
	defer coordinator.journalMutex.Unlock()
	// 删除派生副本之前先校验。持有这个视图期间 latest 只会前进，有效的序号在这里不会变得无效。
	if epoch != coordinator.history.Epoch() {
		return syncstream.ErrAckEpochMismatch
	}
	if sequence > coordinator.history.Status(observer, stream).LatestSequence {
		return syncstream.ErrAckAhead
	}
	// 先删 outbox 里的派生副本。之后 History ACK 失败时，启动时的对账能重建它；反过来的顺序在崩溃或存储失败后会永久
	// 留下一条过期的 outbox 记录。
	if err := coordinator.outbox.Acknowledge(observer, stream, epoch, sequence); err != nil {
		return errors.Join(err, coordinator.reconcileOutboxLocked())
	}
	if err := coordinator.history.AcknowledgeEpoch(observer, stream, epoch, sequence); err != nil {
		// 立即修复，同时保留 History 作为崩溃恢复的来源。合并两个错误时保留主要的 WAL 错误。
		return errors.Join(err, coordinator.reconcileOutboxLocked())
	}
	return nil
}

type snapshotProviderFunc func(syncstream.ResyncRequest) (syncstream.Packet, error)

func (provider snapshotProviderFunc) Snapshot(request syncstream.ResyncRequest) (syncstream.Packet, error) {
	return provider(request)
}

// Recover 处理客户端的重同步请求：先修复 outbox，再由 History 决定补发增量还是重发全量快照。
func (coordinator *Coordinator) Recover(request syncstream.ResyncRequest) (syncstream.ResyncResult, error) {
	view := observerKey{request.Observer, request.Stream.Key}
	release, err := coordinator.acquireView(view)
	if err != nil {
		return syncstream.ResyncResult{}, err
	}
	if err := coordinator.repairOutbox(); err != nil {
		release()
		return syncstream.ResyncResult{}, err
	}
	cursor := coordinator.cursor(view)
	// 捕获不持交接锁；History 自己核对捕获前后的 revision，拒绝失效快照。
	result, err := coordinator.history.Recover(request, snapshotProviderFunc(func(request syncstream.ResyncRequest) (syncstream.Packet, error) {
		return coordinator.snapshotPacketWithCursor(request, &cursor)
	}))
	coordinator.journalMutex.Lock()
	if err == nil {
		if result.Reason != syncstream.ResyncNone && len(result.Packets) == 1 && result.Packets[0].Full {
			coordinator.setCursor(view, cursor)
		}
		for _, packet := range result.Packets {
			if putErr := coordinator.outbox.Put(packet); putErr != nil {
				coordinator.repairPending = true
				err = putErr
				break
			}
		}
	}
	coordinator.journalMutex.Unlock()
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

func (coordinator *Coordinator) snapshotPacketWithCursor(request syncstream.ResyncRequest, cursor *sourceCursor) (syncstream.Packet, error) {
	switch request.Stream.Topic {
	case TopicManifest:
		plan, ok := coordinator.plan(request.Stream.Key)
		if !ok {
			return syncstream.Packet{}, ErrManifestMissing
		}
		return coordinator.projector.ManifestPacket(request.Observer, request.Stream.Key, plan)
	case TopicState:
		source := coordinator.runtime.StateSnapshot()
		snapshot, err := coordinator.visibility.FilterStateSnapshot(request.Observer, source)
		if err != nil {
			coordinator.counters.visibilityFailures.Add(1)
			return syncstream.Packet{}, err
		}
		cursor.state, cursor.presentation = source.LatestStateMutationSequence, source.LatestPresentationSequence
		cursor.resetPresentation = true
		return coordinator.projector.StateSnapshotPacket(request.Observer, request.Stream.Key, snapshot)
	case TopicPresentation:
		snapshot, err := coordinator.presentationReset(request.Observer)
		if err != nil {
			return syncstream.Packet{}, err
		}
		cursor.presentation, cursor.resetPresentation = snapshot.LatestPresentationSequence, false
		return coordinator.projector.PresentationResetPacket(request.Observer, request.Stream.Key, snapshot)
	default:
		return syncstream.Packet{}, fmt.Errorf("%w: %s", ErrTopicUnsupported, request.Stream.Topic)
	}
}

// presentationReset 是 presentation reset 的唯一构造点：每条持续表现按它对应的增量事件交给 observer 的
// VisibilityPolicy.FilterPresentation，不可见的整条去掉，可见的取回过滤后的 Anchor（目标清零、空间字段按策略清除）。
// 游标过期的 Flush 与 Recover 都走这里，不直接投影 Runtime.PresentationSnapshot()（那样不可见施法者的持续表现、目标与
// 坐标会发给所有 observer）。复用 FilterPresentation 而不在接口上加方法，自定义策略无需改动。
func (coordinator *Coordinator) presentationReset(observer syncstream.Observer) (skill.PresentationRecoverySnapshot, error) {
	snapshot := coordinator.runtime.PresentationSnapshot()
	// reset 的包头同样受时钟策略约束，不能只过滤每条持续表现的实体与坐标。
	clock, err := coordinator.visibility.FilterStateSnapshot(observer, skill.RuntimeStateSnapshot{Tick: snapshot.Tick, WorldRevision: snapshot.WorldRevision})
	if err != nil {
		coordinator.counters.visibilityFailures.Add(1)
		return skill.PresentationRecoverySnapshot{}, err
	}
	active := make([]skill.ActivePresentation, 0, len(snapshot.Active))
	for _, entry := range snapshot.Active {
		filtered, allowed, err := coordinator.visibility.FilterPresentation(observer, activePresentationEvent(snapshot, entry))
		if err != nil {
			coordinator.counters.visibilityFailures.Add(1)
			return skill.PresentationRecoverySnapshot{}, err
		}
		if !allowed {
			coordinator.counters.filtered.Add(1)
			continue
		}
		entry.Anchor, entry.PrimaryTarget = filtered.Anchor, filtered.PrimaryTarget
		active = append(active, entry)
	}
	snapshot.Active = active
	snapshot.Tick, snapshot.WorldRevision = clock.Tick, clock.WorldRevision
	return snapshot, nil
}

// activePresentationEvent 把一条持续表现还原成 Runtime 会为它发出的增量事件形状（presentation.go 的
// appendPresentation：Source 是施法者 / owner，Anchor.Target 是目标 / lifecycle 实体，PrimaryTarget 取条目的
// PrimaryTarget——仍归施法的衍生物是施法目标而不是 lifecycle 实体），让 reset 与增量经过同一条过滤规则。
// PrimaryTarget 不能用 Anchor.Target 代替，否则按 PrimaryTarget 判定的策略会放行增量里被挡住的衍生物表现。
func activePresentationEvent(snapshot skill.PresentationRecoverySnapshot, entry skill.ActivePresentation) skill.PresentationEvent {
	event := skill.PresentationEvent{
		Sequence: snapshot.LatestPresentationSequence, Tick: snapshot.Tick, WorldRevision: snapshot.WorldRevision,
		Kind: skill.PresentationCast, ProgramID: entry.ProgramID, GameplayDigest: entry.GameplayDigest, PresentationDigest: entry.PresentationDigest,
		CastID: entry.CastID, VisualIndex: entry.VisualIndex, Source: entry.Anchor.Source, PrimaryTarget: entry.PrimaryTarget, Anchor: entry.Anchor,
	}
	if entry.Kind == skill.ActivePresentationSpawn {
		event.Kind = skill.PresentationSpawnUpdate
		event.SpawnID, event.SpawnTemplate, event.HasSpawn, event.SpawnStatus = entry.SpawnID, entry.SpawnTemplate, true, entry.SpawnStatus
	}
	return event
}

func (coordinator *Coordinator) publishDue(observer syncstream.Observer, stream syncstream.Stream) error {
	if !coordinator.observerOpen(observer) {
		return ErrObserverClosed
	}
	if err := coordinator.repairOutbox(); err != nil {
		return err
	}
	err := coordinator.outbox.PublishDue(coordinator.publisher, time.Now(), &observer, &stream)
	return err
}
func (coordinator *Coordinator) publishNow(observer syncstream.Observer, stream syncstream.Stream) error {
	if !coordinator.observerOpen(observer) {
		return ErrObserverClosed
	}
	if err := coordinator.repairOutbox(); err != nil {
		return err
	}
	err := coordinator.outbox.PublishNow(coordinator.publisher, time.Now(), &observer, &stream)
	return err
}
func (coordinator *Coordinator) publishObserver(observer syncstream.Observer) error {
	if !coordinator.observerOpen(observer) {
		return ErrObserverClosed
	}
	if err := coordinator.repairOutbox(); err != nil {
		return err
	}
	err := coordinator.outbox.PublishDue(coordinator.publisher, time.Now(), &observer, nil)
	return err
}

// RetryPending 修复 outbox 后发布所有到期的待发包，供调用方定期调用。
func (coordinator *Coordinator) RetryPending(now time.Time) error {
	if err := coordinator.repairOutbox(); err != nil {
		return err
	}
	err := coordinator.outbox.publishDue(coordinator.publisher, now, nil, nil, coordinator.observerOpen)
	return err
}

// ReconcilePending 显式修复 history / outbox 之间的崩溃窗口。正常重试不做 O(history) 的重扫：构造时对账一次，
// 之后每次实时追加都直接持久化到 outbox。
func (coordinator *Coordinator) ReconcilePending() error {
	if coordinator == nil || coordinator.outbox == nil || coordinator.history == nil {
		return ErrCoordinatorInvalid
	}
	coordinator.journalMutex.Lock()
	defer coordinator.journalMutex.Unlock()
	return coordinator.reconcileOutboxLocked()
}

// CloseObserver 关闭 observer：先删掉它在 outbox 里的待发包，再删 History 里的流。成功返回才算关闭完成；
// 有在途发布时返回可重试的 ErrApplyInProgress。
func (coordinator *Coordinator) CloseObserver(observer syncstream.Observer) error {
	coordinator.mutex.Lock()
	if _, closing := coordinator.closingObservers[observer]; closing {
		coordinator.mutex.Unlock()
		return ErrApplyInProgress
	}
	_, active := coordinator.activeObservers[observer]
	_, retiring := coordinator.retiringObservers[observer]
	if !active && !retiring && len(coordinator.activeObservers)+len(coordinator.retiringObservers) >= coordinator.maxObservers {
		coordinator.mutex.Unlock()
		return ErrCoordinatorCapacity
	}
	delete(coordinator.activeObservers, observer)
	coordinator.retiringObservers[observer] = struct{}{}
	coordinator.closingObservers[observer] = struct{}{}
	type lockedView struct {
		key   observerKey
		entry *viewLockEntry
	}
	views := make([]lockedView, 0)
	for key, entry := range coordinator.viewLocks {
		if key.observer == observer {
			entry.refs++ // 关闭操作持有一个引用
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
	coordinator.journalMutex.Lock()
	defer coordinator.journalMutex.Unlock()
	if err := coordinator.outbox.DiscardObserver(observer); err != nil {
		return errors.Join(err, coordinator.reconcileOutboxLocked())
	}
	// 在每条 outbox 记录都已持久删除之前，History 一直是修复来源。此后崩溃是安全的：保留的 History 可以对账。
	if _, err := coordinator.history.DeleteObserver(observer); err != nil {
		return err
	}
	coordinator.mutex.Lock()
	delete(coordinator.retiringObservers, observer)
	for key := range coordinator.cursors {
		if key.observer == observer {
			delete(coordinator.cursors, key)
		}
	}
	coordinator.mutex.Unlock()
	return nil
}

// Metrics 返回累计计数的快照。
func (coordinator *Coordinator) Metrics() CoordinatorMetrics {
	// 发布计数由 outbox 唯一记账，不能累加可能重叠的前后快照差。
	outbox := coordinator.outbox.Metrics()
	return CoordinatorMetrics{Published: outbox.PublishSuccesses, PublishFailures: outbox.PublishFailures, Filtered: coordinator.counters.filtered.Load(), VisibilityFailures: coordinator.counters.visibilityFailures.Load(), SnapshotRecoveries: coordinator.counters.snapshotRecoveries.Load()}
}
