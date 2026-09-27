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
	// opening 登记正在等待传输 SessionOpened 的 ID；传输确认前会话不进入 sessions，
	// Subscribe/Flush 看不见它，同 ID 的并发 OpenSession 返回 ErrSessionOpening。
	opening map[SessionID]struct{}
	closed  bool
	closing bool

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
	windowByteBlocked bool          // 本窗口已遇到放不下的冷对象，留到下一窗口首位竞争
	snapshotNextClass snapshotClass // 小额度跨窗口仍轮流服务两类
	snapshotsDeferred atomic.Uint64

	// resubmitMu 保护框架撤销的订阅记录（policy_queue.go，RR-20260926-70）；叶子锁。
	resubmitMu      sync.Mutex
	retracted       map[int64][]retractedSubscription // 撤销时记下，等同 ID 重新登记
	resubmits       []queuedResubmit                  // 已重新登记，等下一次政策阶段交付
	delivering      []queuedResubmit                  // 政策阶段已取出、回调尚未返回（RR-20260926-78）
	resubmitEntries atomic.Int64                      // 三者的条数；退订热路径据此跳过加锁
	releases        []queuedRelease                   // 框架丢掉的来源订阅，等下一次政策阶段通知（RR-20260926-79）
	releaseEntries  atomic.Int64                      // releases 的条数；政策阶段据此跳过加锁
	stampClock      atomic.Uint64                     // 订阅戳与释放戳共用的逻辑时钟（SubscriptionStamp，RR-20260926-85）

	// forgetUnlinked 是测试缝：forget 把 subject 从表里摘下并放开 m.mu 之后调用，用来确定性地进入
	// “已离表、退役收尾未完成”的窗口（RR-20260926-69 回归）。生产中恒为 nil。
	forgetUnlinked func(subjectID int64)
	// registeredBeforeHandBack 是测试缝：register 把新 subject 放进表、放开 m.mu 并装好脏通知之后，交还撤销记录
	// （releaseRetracted）之前调用，用来确定性地在这一窗口再次撤销新 subject（OPEN-ITEMS B21，RR-20260926-78 自报的
	// 未验证窗口）。生产中恒为 nil。
	registeredBeforeHandBack func(subjectID int64)
	// unregisterLookedUp 是测试缝：Unregister 无锁取到 subject 之后、取 subj.mu 之前调用，用来确定性地在这一窗口让旧
	// subject 被 forget、同 ID 重新登记并订阅（OPEN-ITEMS B39，第五轮审计疑点）。生产中恒为 nil。
	unregisterLookedUp func(subjectID int64)
	// retractLookedUp 是测试缝：RetractSyncSubject 取到 subject 之后、比对状态并注销之前调用，用来确定性地在这一窗口让同 ID
	// 换成另一个状态登记（OPEN-ITEMS B41）。生产中恒为 nil。
	retractLookedUp func(subjectID int64)
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
		opening:          make(map[SessionID]struct{}),
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
	_, err := m.register(state, nil)
	return err
}

// register 是 Register 与 RegisterAfterRetirement 的共同路径。done 只在登记被排到退役完成之后
// （queued=true）时交给排队项，由它恰好报告一次结果；直接登记或被拒绝时不调用。
func (m *Manager) register(state *entity.SubjectSyncState, done func(error)) (queued bool, err error) {
	if m == nil {
		return false, ErrManagerClosed
	}
	if state == nil || !state.Enabled() || state.SubjectID() == 0 {
		return false, ErrSubjectInvalid
	}
	id := state.SubjectID()
	m.mu.Lock()
	if m.closed || m.closing {
		m.mu.Unlock()
		return false, ErrManagerClosed
	}
	if existing, ok := m.subjects[id]; ok {
		m.mu.Unlock()
		// 旧状态已关闭（实体被卸载或驱逐后重新加载）时接到新状态上并强制全量；否则照旧拒绝。
		return m.rebind(existing, state, done)
	}
	if len(m.subjects) >= m.config.MaxSubjects {
		m.mu.Unlock()
		return false, ErrSubjectLimit
	}
	created := newSubject(state)
	m.subjects[id] = created
	m.mu.Unlock()
	m.installDirtyNotifier(id, state)
	if m.registeredBeforeHandBack != nil {
		m.registeredBeforeHandBack(id)
	}
	// 同 ID 此前被框架撤销的政策订阅交还政策，在下一次政策阶段重新提交（RR-20260926-70）。
	m.releaseRetracted(created)
	return false, nil
}

// installDirtyNotifier also fires the notifier when the state is already
// dirty, so a subject that changed before it was registered is not forgotten.
func (m *Manager) installDirtyNotifier(id int64, state *entity.SubjectSyncState) {
	state.SetDirtyNotifier(func(*entity.SubjectSyncState) {
		m.markPending(id)
		if state.SyncCommitReady() {
			if m.config.Trace != nil {
				m.config.Trace.Record(SyncTraceEvent{Stage: "commit_ready", SubjectID: id})
			}
			m.WakeSync()
		}
	})
}

// Rebind 把已登记的 subject 接到同一实体重新加载出来的新内容状态上。前提是旧状态已关闭——
// 实体被卸载或驱逐（例如原生 saga 步骤投影时被 lease fence 跳过，DataEngine 驱逐了含其效果的
// 内存实体，RR-20260926-30）。订阅关系保持；每个仍持有或等待该对象的订阅者下一帧收到全量
// （已持有的是整份替换的 ObjectUpdate），不在两份状态之间续发增量。待 remove 的订阅照旧发 remove。
//
// 未登记返回 ErrSubjectNotRegistered，退役中返回 ErrSubjectRetiring；已登记的状态仍在使用时返回
// ErrSubjectRegistered（不抢占活着的状态）；同一状态重复调用是空操作。Register 遇到旧状态已关闭的
// 同 ID subject 时走同一路径。旧状态关闭到重新绑定之间，该 subject 不捕获、不发送，也不计失败。
//
// 例外（RR-20260926-59）：subject 因卸载后重载不了被 RetractUnloadedSubject 退回 remove、退役尚未完成时，
// 实体又被加载出来（业务访问、kit 的 OnEntityLoaded）——Rebind / Register 把新状态排在退役完成之后登记
// （与 RegisterAfterRetirement 同一机制：最后一个 remove 交付的同一步登记，remove-before-create 不变），
// 返回 nil，调用方不会遇到 ErrSubjectRetiring。业务自己 Unregister 的退役不受影响。
func (m *Manager) Rebind(state *entity.SubjectSyncState) error {
	if m == nil {
		return ErrManagerClosed
	}
	if state == nil || !state.Enabled() || state.SubjectID() == 0 {
		return ErrSubjectInvalid
	}
	subj := m.subject(state.SubjectID())
	if subj == nil {
		return ErrSubjectNotRegistered
	}
	_, err := m.rebind(subj, state, nil)
	return err
}

// rebind 返回 queued=true 表示 state 被排在卸载退役（RR-59）完成之后登记。done 随排队项保存，
// 由 forget 的登记或取消恰好报告一次；后到的排队替换先到的，被替换者的 done 收到 ErrRegistrationCancelled
// （RR-20260926-72）。Rebind / Register 传 nil：同一状态已在排队时保留原排队项（不取消业务排队的 done）。
func (m *Manager) rebind(subj *subject, state *entity.SubjectSyncState, done func(error)) (queued bool, err error) {
	subj.mu.Lock()
	switch {
	case subj.retiring && subj.unloadRetracted && !subj.forgotten:
		if current := subj.successor; done == nil && current != nil && current.state == state {
			subj.mu.Unlock()
			return true, nil
		}
		replaced := subj.successor
		subj.successor = &queuedRegistration{state: state, done: done}
		subj.mu.Unlock()
		replaced.finish(ErrRegistrationCancelled)
		return true, nil
	case subj.retiring:
		subj.mu.Unlock()
		return false, ErrSubjectRetiring
	case subj.state == state:
		subj.mu.Unlock()
		return false, nil
	case subj.state.Enabled():
		subj.mu.Unlock()
		return false, ErrSubjectRegistered
	}
	previous := subj.state
	subj.state = state
	for sid, sub := range subj.subscribers {
		if sub.kind == kindLeaving {
			continue
		}
		// 与换 profile 同一机制：baseVersion 清零、改为等待全量，revision 前移让旧状态的在途捕获作废。
		sub.baseVersion = 0
		m.changeSubscriptionKindLocked(subj, sid, sub, kindSnapshot)
		sub.revision++
	}
	subj.profilesValid = false
	subj.mu.Unlock()
	previous.DiscardFrozenSync()
	m.installDirtyNotifier(subj.id, state)
	m.markPending(subj.id)
	m.WakeSync()
	return false, nil
}

// queuedRegistration 是一次排在退役完成之后的登记。
type queuedRegistration struct {
	state *entity.SubjectSyncState
	done  func(error)
}

func (q *queuedRegistration) finish(err error) {
	if q != nil && q.done != nil {
		q.done(err)
	}
}

// RegisterAfterRetirement 与 Register 相同，只是 subject 正在退役（Unregister 之后仍欠订阅者
// ObjectRemove）时不返回 ErrSubjectRetiring，而是把 state 排在退役完成之后登记（RR-20260926-55）：
// 最后一个 remove 交付、或最后一个订阅者的会话关闭时，同一步里以 state 重新登记。remove-before-create
// 不变——退役期间对该 subject 的订阅仍被拒绝（ErrSubjectRetiring），登记完成后的订阅从全量快照开始。
//
// 返回 queued=true 表示已排队，结果由 done 报告且恰好一次：nil 为已登记；ErrRegistrationCancelled
// 为退役完成前再次 Unregister、被后一次排队替换、或排队的 state 已关闭；ErrManagerClosed 为
// Manager 已关闭。每个 subject 至多排一个。done 在 Manager 自己的调用路径上执行（Flush、
// CloseSession、Unregister、Close），不持有 Manager 的锁；它不得阻塞，也不得调用 Flush——
// 需要做事（例如让政策重说被拒绝的订阅）就交给自己的 goroutine。
//
// subject 因卸载后重载不了被 RetractUnloadedSubject 退回 remove、退役尚未完成时（RR-20260926-59），
// 同样返回 queued=true 并由 done 报告结果（RR-20260926-72）；这个排队位与 Rebind / Register 的排队
// （kit 的 OnEntityLoaded）共用，后到的替换先到的。
//
// queued=false 时它就是 Register 的结果，done 不会被调用。本方法不等待，可在快池调用。
func (m *Manager) RegisterAfterRetirement(state *entity.SubjectSyncState, done func(error)) (queued bool, err error) {
	if m == nil {
		return false, ErrManagerClosed
	}
	if state == nil || !state.Enabled() || state.SubjectID() == 0 {
		return false, ErrSubjectInvalid
	}
	id := state.SubjectID()
	// 一次循环要么返回，要么观察到退役刚好完成（subject 已被 forget），所以两轮足够；
	// 第三轮只在并发的 Unregister/Register 反复交错时出现，仍然有界。
	for range 3 {
		queued, err := m.register(state, done)
		if !errors.Is(err, ErrSubjectRetiring) {
			return queued, err
		}
		subj := m.subject(id)
		if subj == nil {
			continue // 退役在两步之间完成
		}
		subj.mu.Lock()
		if subj.forgotten || !subj.retiring {
			subj.mu.Unlock()
			continue
		}
		replaced := subj.successor
		subj.successor = &queuedRegistration{state: state, done: done}
		subj.mu.Unlock()
		replaced.finish(ErrRegistrationCancelled)
		return true, nil
	}
	return false, ErrSubjectRetiring
}

// Unregister retires a subject: every subscriber is owed an ObjectRemove on
// its next frame, and once the last one has gone out the subject is
// forgotten. Subscribing to a retiring subject is refused. A registration
// queued behind an earlier retirement (RegisterAfterRetirement) is cancelled.
//
// Unregister 按 ID 注销当前登记：取到 subject 之后、取锁之前，它若已退役完成被 forget、同 ID 已重新登记，
// 注销作用在重新登记的那一个上（RR-20260927-22）。
func (m *Manager) Unregister(subjectID int64) error {
	return m.unregister(subjectID, nil)
}

// unregister 是 Unregister 与 RetractSyncSubject 的共同路径。only 非 nil 时只注销以 only 登记的那一次：
// 取锁并确认表项之后，当前登记的状态不是 only 就什么也不做（RR-20260927-28）。
func (m *Manager) unregister(subjectID int64, only *entity.SubjectSyncState) error {
	for {
		subj := m.subject(subjectID)
		if subj == nil {
			return ErrSubjectNotRegistered
		}
		if m.unregisterLookedUp != nil {
			m.unregisterLookedUp(subjectID)
		}
		subj.mu.Lock()
		// RR-20260927-22：subject 是无锁取的，取锁前它可能已退役完成、被 forget 摘表，同 ID 随即重新登记，政策也已在新登记上
		// 订阅。之前不做这一步检查，旧 subject 上取的释放戳比新登记上的订阅戳大，按 ID 删撤销记录又删到新登记名下，
		// 结果新登记还在，它的订阅和待交还记录却被这次注销清掉——任何串行顺序都得不到这个状态。
		// 所以取锁后先确认它仍是表里的那一个，不是就重新查表，注销当前登记（业务按 ID 注销，就是要这个 ID 不再同步；
		// 只把它当作“旧登记已结束”的空操作，会让卸载后重载的新登记在业务注销之后继续存活）。
		// 锁序 subj.mu → m.mu 与 subscribe 相同。读锁一直持有到按 ID 删完撤销记录：forget 摘表要取 m.mu 写锁，
		// 所以这段时间里同 ID 不会出现新登记，按 ID 删掉的都是本次登记名下的记录。释放戳也在这段里取，
		// 新登记上的订阅戳一定更大（RR-20260926-85）。
		m.mu.RLock()
		if m.subjects[subjectID] != subj {
			m.mu.RUnlock()
			subj.mu.Unlock()
			continue
		}
		if only != nil && subj.state != only {
			// RR-20260927-28：同 ID 已换成另一个状态登记（旧状态关闭后 Rebind 到新状态，或旧登记被忘掉后新状态重新登记），
			// 要撤回的那次登记已经不在了，不能退役别人的登记。比对与退役在同一把 subj.mu 下、确认表项之后，中间不再有窗口。
			m.mu.RUnlock()
			subj.mu.Unlock()
			return nil
		}
		subj.unloadRetracted = false // 业务的注销意图优先：之后重新加载不再自动排队登记
		// 退役会清空来源：先记下要通知政策的释放（RR-20260926-79）。框架撤销的政策订阅同理不再交还（在 forget 可能发生之前），
		// 其中的 pair 也一并通知。
		stamp := m.nextStamp()
		var released []queuedRelease
		for sid, sub := range subj.subscribers {
			released = releasedLocked(released, sub.sources, sid, subjectID, stamp)
		}
		released = m.dropRetractedSubject(subjectID, released, stamp)
		m.mu.RUnlock()
		// retireLocked 查会话持有表要取 m.mu 读锁，不能在上面的读锁内重入（RWMutex 读锁重入遇到等待的写者会死锁）。
		remaining, cancelled := m.retireLocked(subj)
		subj.mu.Unlock()
		m.queueReleases(released)
		cancelled.finish(ErrRegistrationCancelled)
		m.finishRetire(subj, remaining)
		return nil
	}
}

// retireLocked 标记退役并把订阅改成 leaving（欠 remove），返回仍需发 remove 的订阅数与被取消的排队登记
// （调用方解锁后 finish：done 不得在持锁时执行）。调用方持有 subj.mu。
func (m *Manager) retireLocked(subj *subject) (int, *queuedRegistration) {
	subj.retiring = true
	cancelled := subj.successor
	subj.successor = nil
	for id, sub := range subj.subscribers {
		if !sub.inFlight && !m.sessionHoldsSubject(id, subj.id) {
			// 只有实际未持有对象的会话才不欠 remove；等待快照也可能是在换视图。
			m.removeSubscriptionLocked(subj, id)
			continue
		}
		clear(sub.sources)
		m.changeSubscriptionKindLocked(subj, id, sub, kindLeaving)
		sub.revision++
		subj.profilesValid = false
	}
	return len(subj.subscribers), cancelled
}

func (m *Manager) finishRetire(subj *subject, remaining int) {
	if remaining == 0 {
		m.forget(subj)
		return
	}
	m.markPending(subj.id)
	m.WakeSync()
}

// SubjectAwaitsReload 实现 entity.UnloadedSubjectSync（RR-20260926-59）：subject 已登记、未退役、内容状态已关闭
// （实体被卸载或驱逐），且仍有持有或等待该对象的订阅者（离开中的不算）。ManagerAccess.Unload 据此决定是否
// 主动从权威重载；重载 worker 每轮也先查它，业务已先重载并重新绑定、订阅者都已离开时不再重载。
func (m *Manager) SubjectAwaitsReload(subjectID int64) bool {
	subj := m.subject(subjectID)
	if subj == nil {
		return false
	}
	subj.mu.Lock()
	defer subj.mu.Unlock()
	if subj.retiring || subj.state.Enabled() {
		return false
	}
	for _, sub := range subj.subscribers {
		if sub.kind != kindLeaving {
			return true
		}
	}
	return false
}

// RetractUnloadedSubject 实现 entity.UnloadedSubjectSync：卸载后的实体重载不了（权威没有它、重试用尽、重载队列
// 已满）时退回 remove。只在 subject 仍停在已关闭的状态上时注销（与 Unregister 同一语义：持有对象的会话收到
// ObjectRemove，全部发出后 subject 被忘掉——remove-before-create）；已重新绑定到活状态或已退役时不动，返回 false。
// 检查与退役在同一把 subj.mu 下完成。退役完成前实体又被加载出来时，Rebind / Register 排队到退役完成后登记
// （见 Rebind）；退役完成后同 ID 的登记是新 subject。被撤销的订阅若来自政策来源（NewSubscriptionSourceWithResubmit），
// 同 ID 重新登记后交还政策按自己的判定重新提交（RR-20260926-70，见 policy_queue.go）；其他来源的订阅不恢复。
func (m *Manager) RetractUnloadedSubject(subjectID int64) bool {
	subj := m.subject(subjectID)
	if subj == nil {
		return false
	}
	subj.mu.Lock()
	if subj.retiring || subj.state.Enabled() {
		subj.mu.Unlock()
		return false
	}
	// 退役会清空订阅来源：先记下政策来源的订阅，同 ID 重新登记时交还政策重新提交（RR-20260926-70）。
	m.recordRetractedLocked(subj)
	remaining, cancelled := m.retireLocked(subj)
	subj.unloadRetracted = true
	subj.mu.Unlock()
	cancelled.finish(ErrRegistrationCancelled)
	m.finishRetire(subj, remaining)
	return true
}

var _ entity.UnloadedSubjectSync = (*Manager)(nil)

// RetractSyncSubject 实现 entity.SyncSubjectRetractor：事务内新建的实体被回滚或拒绝时，
// Nest 在仍持有该实体锁时调用它（RR-20260926-35）。只注销以同一个状态对象登记的 subject，
// 语义与 Unregister 相同：未持有对象的订阅直接移除，已持有的会话先收到 ObjectRemove，
// 之后同 ID 的新实体才能重新登记（remove-before-create）。
//
// 按实例撤回（RR-20260927-28，OPEN-ITEMS B41）：状态比对在 unregister 里、取 subj.mu 并确认表项之后进行。
// 之前在锁外比对 subj.state == state 后按 ID 调 Unregister：两步之间同 ID 换成另一个状态登记时，注销的是新登记；
// 锁外读 subj.state 本身也与 rebind 的写并发。
func (m *Manager) RetractSyncSubject(state *entity.SubjectSyncState) {
	if m == nil || state == nil {
		return
	}
	id := state.SubjectID()
	if m.subject(id) == nil {
		return
	}
	if m.retractLookedUp != nil {
		m.retractLookedUp(id)
	}
	_ = m.unregister(id, state)
}

var _ entity.SyncSubjectRetractor = (*Manager)(nil)

// forget drops a subject whose last remove has gone out, and then registers
// the state queued behind the retirement, if any (RegisterAfterRetirement).
//
// 只忘掉调用方看到的那个 subject：表里同 ID 已是另一个 subject（上一次 forget 之后重新登记的）时什么都不做。
// 旧 subject 装在状态上的脏通知器与冻结内容在删表之前、同一把 m.mu 内清除（RR-20260926-69）：删表一放开，
// 同一个状态对象就可能被 Register（RegisterAfterRetirement 的直接登记，或下面排队的登记）装上新 subject 的
// 通知器，之后再清就会清掉新 subject 的，后续 MarkDirty 不再调度。锁序 m.mu → 状态锁：SetDirtyNotifier(nil)
// 不回调，DiscardFrozenSync 只归还原子计数，状态锁内不取 Manager 的锁。
func (m *Manager) forget(subj *subject) {
	// 退役中的 subject 不再换状态（rebind 对退役中的 subject 只排队或拒绝），这里读到的就是删表时的状态。
	state := subj.currentState()
	m.mu.Lock()
	if m.subjects[subj.id] != subj {
		m.mu.Unlock()
		return
	}
	state.SetDirtyNotifier(nil)
	state.DiscardFrozenSync()
	delete(m.subjects, subj.id)
	m.mu.Unlock()
	if m.forgetUnlinked != nil {
		m.forgetUnlinked(subj.id)
	}
	m.pendingMu.Lock()
	if wait := m.waitingSnapshots[subj.id]; wait.subject == subj {
		m.removeSnapshotWaitLocked(subj.id)
	}
	m.pendingMu.Unlock()
	subj.mu.Lock()
	subj.forgotten = true
	queued := subj.successor
	subj.successor = nil
	subj.mu.Unlock()
	if queued == nil {
		return
	}
	// 同一个状态对象重新登记也走这里：旧 subject 的通知与冻结内容已在删表前清掉，这里以新 subject 登记，
	// 状态若仍脏，installDirtyNotifier 会立即排队。
	if !queued.state.Enabled() {
		queued.finish(ErrRegistrationCancelled)
		return
	}
	// 表里若已是另一个又在卸载退役中的同 ID subject，登记会再次排队：done 随新的排队项走，这里不报告。
	if requeued, err := m.register(queued.state, queued.done); !requeued {
		queued.finish(err)
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
		state := subj.state
		subj.forgotten = true
		queued := subj.successor
		subj.successor = nil
		subj.mu.Unlock()
		state.SetDirtyNotifier(nil)
		state.DiscardFrozenSync()
		queued.finish(ErrManagerClosed)
	}
	m.clearRetracted()
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
