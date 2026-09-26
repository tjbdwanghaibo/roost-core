package entitysync

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// DefaultInterval 是脏数据的合并检查周期；不包含执行、网络或重试耗时，
// 不能作为端到端延迟上限。
const DefaultInterval = 50 * time.Millisecond

// ManagerConfig shapes the one Manager a process runs.
type ManagerConfig struct {
	// Trace 是可选的有界阶段诊断，nil 时不记录。
	Trace *SyncTrace
	Mode  SyncMode
	// MaxFrozenBytes 限制变化触发模式保留的内容字节；零值为 64MiB。
	MaxFrozenBytes int64
	// Transport receives each encoded frame; a session may need multiple frames per tick. Required.
	Transport Transport
	// Interval is the tick period for Start. Zero takes DefaultInterval.
	Interval time.Duration
	// ProfilePriorities 越小越优先；未配置的视图按 LOD 排序。构造时复制配置。
	// 只在已授权来源之间选一个视图，不合并字段或扩大授权。
	ProfilePriorities map[entity.SyncProfile]int
	// SnapshotBudget 只限制客户端尚未持有对象的创建；现有对象全量刷新不占额度，零值不限制。
	SnapshotBudget SnapshotBudget
	// Limits bound a frame. Zero fields take frame.DefaultLimits.
	// MaxObjects is also how many subjects one session may hold.
	Limits frame.Limits
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
	// the manager's subscription index does not own policy-layer membership (ARCH-10).
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
	defaults := frame.DefaultLimits()
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

// Manager 管理进程内的同步实体与接收会话。政策层通过 Subscribe 指定订阅关系，
// 实体通过脏通知进入待同步集合，Flush 按会话组织并交付内容。
//
// 本文件负责配置、实体注册和生命周期；subscriptions.go 管理会话与订阅，
// flush.go 负责捕获、准入与提交。订阅意图归 subject，已交付的时钟与引用归 session。
type Manager struct {
	nextLifetime      atomic.Uint64
	subscriptionCount atomic.Int64
	snapshotRequests  snapshotRequests
	heldSessions      int // mu
	frozenPeakBytes   atomic.Int64
	policyMu          sync.Mutex
	policies          map[uint64]policyHook
	nextPolicy        uint64
	wake              chan struct{}
	producerBound     atomic.Bool
	frozenBytes       atomic.Int64
	frozenDeferred    atomic.Uint64
	budgetWindow      time.Time
	windowAllowance   snapshotAllowance
	windowSessions    map[SessionID]int
	config            ManagerConfig
	wire              wireConfig

	mu       sync.RWMutex
	subjects map[int64]*subject
	sessions map[SessionID]*session
	closed   bool
	closing  bool

	pendingMu        sync.Mutex
	pending          map[int64]struct{}
	waitingOrder     list.List
	pendingOverlap   int                    // 同时在 pending 与 waitingSnapshots 中的 subject 数，pendingMu
	waitingSnapshots map[int64]snapshotWait // pendingMu；仅保存重查请求，不保存旧订阅意图

	// flushGate 串行化捕获与关闭，等待者可随 context 取消退出。
	flushGate chan struct{}
	runMu     sync.Mutex
	runState  *managerRun
	stopState *managerStop

	captureNanos      atomic.Uint64
	encodeNanos       atomic.Uint64
	admissionNanos    atomic.Uint64
	flushCalls        atomic.Uint64
	emptyFlushes      atomic.Uint64
	flushNanos        atomic.Uint64
	lastFlushNanos    atomic.Uint64
	dirtyCaptured     atomic.Uint64
	snapshotsCaptured atomic.Uint64
	createsAdmitted   atomic.Uint64
	updatesAdmitted   atomic.Uint64
	removesAdmitted   atomic.Uint64
	framesAdmitted    atomic.Uint64
	sessionsLost      atomic.Uint64
	deferred          atomic.Uint64
	flushFailures     atomic.Uint64
	errMu             sync.Mutex
	lastError         error
	flushWork         map[SessionID]*flushSession
	flushPool         []*flushSession
	snapshotCursor    [2]SessionID  // flushGate；两类分别保留会话轮转位置
	snapshotNextClass snapshotClass // 小额度跨窗口仍轮流服务两类
	snapshotsDeferred atomic.Uint64
}

func NewManager(config ManagerConfig) (*Manager, error) {
	if config.Mode != ModePeriodic && config.Mode != ModeOnChange {
		return nil, fmt.Errorf("entitysync: invalid sync mode %d", config.Mode)
	}
	if config.Interval < 0 || config.MaxFrozenBytes < 0 {
		return nil, fmt.Errorf("entitysync: negative interval or frozen capacity")
	}
	if config.MaxFrozenBytes == 0 {
		config.MaxFrozenBytes = 64 << 20
	}
	if config.Transport == nil {
		return nil, ErrTransportRequired
	}
	priorities := make(map[entity.SyncProfile]int, len(config.ProfilePriorities))
	for profile, priority := range config.ProfilePriorities {
		profile = profile.Normalize()
		if _, exists := priorities[profile]; exists {
			return nil, fmt.Errorf("entitysync: duplicate profile priority %+v", profile)
		}
		priorities[profile] = priority
	}
	config.ProfilePriorities = priorities
	config = config.normalized()
	if config.SnapshotBudget.MaxObjects < 0 || config.SnapshotBudget.MaxBytes < 0 || config.SnapshotBudget.PerSessionObjects < 0 {
		return nil, fmt.Errorf("entitysync: negative snapshot budget")
	}
	if bounded, ok := config.Transport.(FrameSizeLimiter); ok {
		if limit := bounded.MaxFrameBytes(); limit > 0 {
			config.Limits.MaxFrameBytes = min(config.Limits.MaxFrameBytes, limit)
		}
	}
	if config.Limits.MaxFrameBytes < 32 {
		return nil, fmt.Errorf("entitysync: frame limit smaller than protocol header")
	}
	return &Manager{
		config: config,
		wake:   make(chan struct{}, 1),
		wire: wireConfig{
			limits: config.Limits, schemaVersion: config.FrameSchemaVersion, archetype: config.Archetype,
			componentType: config.ComponentTypeID, componentSchema: config.ComponentSchemaVersion,
		},
		subjects:         make(map[int64]*subject),
		sessions:         make(map[SessionID]*session),
		pending:          make(map[int64]struct{}),
		waitingSnapshots: make(map[int64]snapshotWait),
		flushGate:        make(chan struct{}, 1),
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
	if m.closed || m.closing {
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
	state.SetDirtyNotifier(func(*entity.SubjectSyncState) {
		m.markPending(id)
		if state.SyncCommitReady() {
			if m.config.Trace != nil {
				m.config.Trace.Record(SyncTraceEvent{Stage: "commit_ready", SubjectID: id})
			}
			m.WakeSync()
		}
	})
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
		if !sub.inFlight && !m.sessionHoldsSubject(id, subjectID) {
			// 只有实际未持有对象的会话才不欠 remove；等待快照也可能是在换视图。
			m.removeSubscriptionLocked(subj, id)
			continue
		}
		clear(sub.sources)
		m.changeSubscriptionKindLocked(subj, id, sub, kindLeaving)
		sub.revision++
		subj.profilesValid = false
	}
	remaining := len(subj.subscribers)
	subj.mu.Unlock()
	if remaining == 0 {
		m.forget(subjectID)
		return nil
	}
	m.markPending(subjectID)
	m.WakeSync()
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
		m.pendingMu.Lock()
		if wait := m.waitingSnapshots[subjectID]; wait.subject == subj {
			m.removeSnapshotWaitLocked(subjectID)
		}
		m.pendingMu.Unlock()
		subj.state.SetDirtyNotifier(nil)
		subj.state.DiscardFrozenSync()
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

// ---- lifecycle ----

type managerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type managerStop struct {
	done chan struct{}
	err  error // done 关闭后只读
}

// Start 启动周期同步；父 context 取消或 Stop 都会取消在途 Push。
// 旧循环与最后一次 Flush 尚未退出时，拒绝启动新循环。
func (m *Manager) Start(ctx context.Context) error {
	if m != nil && m.config.Mode == ModeOnChange && !m.producerBound.Load() {
		return fmt.Errorf("entitysync: on_change requires a bound sync commit producer")
	}
	if m == nil {
		return ErrManagerClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()
	m.mu.RLock()
	closed := m.closed || m.closing
	m.mu.RUnlock()
	if closed {
		return ErrManagerClosed
	}
	if m.stopState != nil {
		return ErrManagerStopping
	}
	if m.runState != nil {
		select {
		case <-m.runState.done:
		default:
			return nil
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	state := &managerRun{cancel: cancel, done: make(chan struct{})}
	m.runState = state
	go m.run(runCtx, state)
	return nil
}

func (m *Manager) run(ctx context.Context, state *managerRun) {
	defer close(state.done)
	defer state.cancel()
	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = m.Flush(ctx)
		case <-m.wake:
			_ = m.Flush(ctx)
		}
	}
}

// Stop 取消周期循环，退出后用调用方 context 做最后一次 Flush。
// 超时只结束本次等待；旧循环真正退出前保持 stopping，防止并行重启。
// Transport 必须响应 Push 的 context；不响应的传输不能被强制终止。
func (m *Manager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.runMu.Lock()
	state := m.stopState
	if state == nil {
		state = &managerStop{done: make(chan struct{})}
		m.stopState = state
		running := m.runState
		if running != nil {
			running.cancel()
		}
		go m.finishStop(ctx, state, running)
	}
	m.runMu.Unlock()
	select {
	case <-state.done:
		return state.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) finishStop(ctx context.Context, state *managerStop, running *managerRun) {
	if running != nil {
		<-running.done
	}
	err := m.Flush(ctx)
	if errors.Is(err, ErrManagerClosed) {
		err = nil
	}
	m.runMu.Lock()
	state.err = err
	m.runState = nil
	m.stopState = nil
	close(state.done)
	m.runMu.Unlock()
}

// Close 先停循环再释放状态；超时返回后可再次调用以完成清理。
// closing 阻止新注册、开会话和 Start，但允许停机的最后一次 Flush。
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.runMu.Lock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.runMu.Unlock()
		return nil
	}
	m.closing = true
	m.mu.Unlock()
	m.runMu.Unlock()
	stopErr := m.Stop(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.acquireFlush(ctx); err != nil {
		return err
	}
	defer m.releaseFlush()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	subjects, sessions := m.subjects, m.sessions
	m.subjects = make(map[int64]*subject)
	m.sessions = make(map[SessionID]*session)
	m.heldSessions = 0
	m.mu.Unlock()
	for _, subj := range subjects {
		subj.mu.Lock()
		for sid := range subj.subscribers {
			m.removeSubscriptionLocked(subj, sid)
		}
		subj.mu.Unlock()
		subj.state.SetDirtyNotifier(nil)
		subj.state.DiscardFrozenSync()
	}
	m.pendingMu.Lock()
	clear(m.pending)
	clear(m.waitingSnapshots)
	m.waitingOrder.Init()
	m.pendingOverlap = 0
	m.pendingMu.Unlock()
	if lifecycle, ok := m.config.Transport.(SessionLifecycle); ok {
		for id := range sessions {
			lifecycle.SessionClosed(id)
		}
	}
	return stopErr
}

func (m *Manager) acquireFlush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.flushGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.releaseFlush()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) releaseFlush() { <-m.flushGate }

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
	FlushCalls              uint64
	EmptyFlushes            uint64
	FlushDuration           time.Duration
	LastFlushDuration       time.Duration
	DirtyCaptured           uint64
	SnapshotsCaptured       uint64
	SnapshotsDeferred       uint64
	CaptureDuration         time.Duration
	EncodeDuration          time.Duration
	AdmissionDuration       time.Duration
	CreatesAdmitted         uint64
	UpdatesAdmitted         uint64
	RemovesAdmitted         uint64
	Subjects                int
	Sessions                int
	HeldSessions            int
	Subscriptions           int
	Pending                 int
	PendingSnapshots        int
	WaitingSnapshotSubjects int
	OldestSnapshotWait      time.Duration
	FramesAdmitted          uint64
	SessionsLost            uint64
	DurabilityDeferred      uint64
	FlushFailures           uint64
	MaxSubjects             int
	MaxSessions             int
}

func (m *Manager) Stats() ManagerStats {
	if m == nil {
		return ManagerStats{}
	}

	m.mu.RLock()
	subjects, sessions, heldSessions := len(m.subjects), len(m.sessions), m.heldSessions
	m.mu.RUnlock()
	subscriptions := int(m.subscriptionCount.Load())
	m.snapshotRequests.mu.Lock()
	snapshots := len(m.snapshotRequests.entries)
	m.snapshotRequests.mu.Unlock()
	m.pendingMu.Lock()
	pending := len(m.pending) + len(m.waitingSnapshots) - m.pendingOverlap
	waiting := len(m.waitingSnapshots)
	var age time.Duration
	if first := m.waitingOrder.Front(); first != nil {
		age = time.Since(m.waitingSnapshots[first.Value.(int64)].since)
	}
	m.pendingMu.Unlock()
	return ManagerStats{
		PendingSnapshots: snapshots, WaitingSnapshotSubjects: waiting, OldestSnapshotWait: age,
		FlushCalls: m.flushCalls.Load(), EmptyFlushes: m.emptyFlushes.Load(),
		FlushDuration: time.Duration(m.flushNanos.Load()), LastFlushDuration: time.Duration(m.lastFlushNanos.Load()),
		DirtyCaptured: m.dirtyCaptured.Load(), SnapshotsCaptured: m.snapshotsCaptured.Load(),
		SnapshotsDeferred: m.snapshotsDeferred.Load(), CaptureDuration: time.Duration(m.captureNanos.Load()), EncodeDuration: time.Duration(m.encodeNanos.Load()), AdmissionDuration: time.Duration(m.admissionNanos.Load()),
		CreatesAdmitted: m.createsAdmitted.Load(), UpdatesAdmitted: m.updatesAdmitted.Load(), RemovesAdmitted: m.removesAdmitted.Load(),
		Subjects: subjects, Sessions: sessions, HeldSessions: heldSessions, Subscriptions: subscriptions, Pending: pending,
		FramesAdmitted: m.framesAdmitted.Load(), SessionsLost: m.sessionsLost.Load(),
		DurabilityDeferred: m.deferred.Load(), FlushFailures: m.flushFailures.Load(),
		MaxSubjects: m.config.MaxSubjects, MaxSessions: m.config.MaxSessions,
	}
}

// AuditStats 显式遍历订阅真相，用于低频诊断和静止状态下核对增量计数。
// 与 Stats 一样，各锁之间允许并发进展，因此不是全局原子快照。
func (m *Manager) AuditStats() ManagerStats {
	stats := m.Stats()
	if m == nil {
		return stats
	}
	m.mu.RLock()
	subjects := make([]*subject, 0, len(m.subjects))
	for _, subj := range m.subjects {
		subjects = append(subjects, subj)
	}
	stats.Subjects, stats.Sessions, stats.HeldSessions = len(subjects), len(m.sessions), 0
	for _, sess := range m.sessions {
		if sess.held {
			stats.HeldSessions++
		}
	}
	m.mu.RUnlock()
	stats.Subscriptions, stats.PendingSnapshots = 0, 0
	for _, subj := range subjects {
		subj.mu.Lock()
		stats.Subscriptions += len(subj.subscribers)
		for _, sub := range subj.subscribers {
			if sub.kind == kindSnapshot {
				stats.PendingSnapshots++
			}
		}
		subj.mu.Unlock()
	}
	m.pendingMu.Lock()
	stats.Pending = len(m.pending)
	stats.WaitingSnapshotSubjects = len(m.waitingSnapshots)
	var oldest time.Time
	for id, wait := range m.waitingSnapshots {
		if _, ok := m.pending[id]; !ok {
			stats.Pending++
		}
		if oldest.IsZero() || wait.since.Before(oldest) {
			oldest = wait.since
		}
	}
	m.pendingMu.Unlock()
	stats.OldestSnapshotWait = 0
	if !oldest.IsZero() {
		stats.OldestSnapshotWait = time.Since(oldest)
	}
	return stats
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
