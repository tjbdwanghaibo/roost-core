package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// Interest 内建距离、额外块覆盖与自身来源；业务关系通过 Relation 添加。
const (
	SourceSpatial = "spatial"
	SourceSelf    = "self"
	SourceBlock   = "block"
)

// InterestConfig shapes an Interest.
type InterestConfig struct {
	// MaxQueuedFacts 限制正式提交事实队列；零值为 65536。
	MaxQueuedFacts int
	// BatchSize 是每批就绪事实的软上限，零值1024；不切开同一Nest提交。
	BatchSize int
	// Workers 限制同一批次并行观察者计算数；零值取GOMAXPROCS。
	Workers int
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// AOI 配置空间网格、缺省进入/离开半径、视野范围、LOD 层级与距离分档；由 NewAOI 校验。
	AOI AOIConfig
	// ObserverOf 把事务内入口（QueueMove / QueueSpatial 的 observer=true、QueueRelation / QueueRelationDelta）
	// 的实体换成观察者的玩家 ID；ok 为 false 表示这个实体不代表任何观察者，入队即拒绝。
	// nil 取 entity.GetUniqueIDFromEntityID（Player 实体的唯一号就是玩家 ID），结果为 0 视为不是观察者。
	// 玩家控制英雄、载具时视点实体不是 Player，业务覆盖它。它在业务线程的 Entity 锁内调用，必须是纯函数、不阻塞。
	ObserverOf func(entityID int64) (playerID int64, ok bool)
	// ViewChangeInterval 是同一玩家两次视野修改（SetObserverView）之间的最小间隔，超频的请求拒绝并计数；
	// 零值 1 秒，负数拒绝构造。
	ViewChangeInterval time.Duration
	// Profile turns a distance band into the profile a subscription asks
	// for. Nil means band 0 → the default profile, band n → {Key: "bandN", LOD: n}.
	Profile func(band int) entity.SyncProfile
	// SourceProfiles 显式覆盖某来源、某距离档的视图；未配置的档继续使用 Profile。
	// 例如 self→owner，spatial→near/far，team→team；构造时复制，来源名必须已声明。
	SourceProfiles map[string]map[int]entity.SyncProfile
	// ViewSets 非空时校验本 Interest 涉及的实体类型及其视图；构造时复制 map。
	// 每个类型应支持可选来源视图，动态关系 band 在订阅前校验。
	ViewSets map[string]*entity.SyncViewSet
	// Relations are the named relation sources to create ("team",
	// "friends", …); Relation(name) reaches them. SourceSelf is always there.
	Relations []string
	// SelfVisible 控制观察者是否通过 self 关系看见自己；空间来源不会建立自身订阅。
	// nil 或 true 启用，只有显式 false 才关闭。
	SelfVisible *bool
}

// Refusal is a subscribe the manager did not take. Observer 是玩家 ID（会话 ID），Subject 是实体 ID。 Interest keeps asking on
// every Apply until it is taken or the pair is released; the caller only
// decides how loudly to log it — Retry tells the first refusal from the ones
// after it.
type Refusal struct {
	Observer int64
	Subject  int64
	Retry    bool
	Err      error
}

// Interest is the area-of-interest policy: a spatial index plus any number of
// relation sources, aggregated into one subscription per (observer, subject)
// and said to the manager.
//
// 编号：观察者是玩家 ID，也就是会话 ID；目标是实体 ID。所以合并键 (observer, subject) 就是 Manager 的
// (session, subject)，一个会话只对应一个观察者，不会出现两个观察者合用一份订阅、其中一个退订把另一个也退掉的情况。
// 关系源同样是“玩家关心哪些实体”：RelationSource.Set(playerID, entityIDs)。
//
// Aggregation is the part worth reading. A pair can be held by more than one
// source at once — a teammate standing next to you is both spatial and team —
// so Interest retains each source: the FIRST source to claim a pair subscribes,
// and only the LAST source to drop it unsubscribes. Without that, a teammate
// walking out of view would take the team subscription with them.
//
// Interest.mu 串行化政策批次、生命周期和订阅归并；批次内按 observer 并行计算。
// Queue* 只持独立 queueMu，不等待空间计算。两个锁仅按 mu→queueMu 获取。
type Interest struct {
	// queueMu 只保护事实准入和角色登记；不能包住空间计算或订阅操作。
	queueMu     sync.Mutex
	queueClosed bool
	// subjectRoles 按实体 ID、observerRoles 按玩家 ID 记录已登记的角色，供事实准入检查。
	subjectRoles     map[int64]struct{}
	observerRoles    map[int64]struct{}
	inflight         int
	batchSize        int
	workers          int
	queueActive      bool
	facts            []queuedInterestFact
	maxQueuedFacts   int
	wake             func()
	cancelQueue      func()
	mu               sync.Mutex
	subscriptions    *entitysync.SubscriptionSource
	aoi              *AOI
	self             *RelationSource
	relations        map[string]*RelationSource
	sources          []Source
	observerOf       func(int64) (int64, bool)
	profile          func(int) entity.SyncProfile
	sourceProfiles   map[string]map[int]entity.SyncProfile
	compareProfiles  func(entity.SyncProfile, entity.SyncProfile) int
	viewSets         map[string]*entity.SyncViewSet
	validatePriority func(entity.SyncView) error

	// held maps a pair to the sources currently claiming it and to whether
	// the manager has been told about it.
	held map[pair]*hold
	// retry holds the pairs whose subscribe the manager refused, waiting
	// for the next Apply to say them again.
	retry []pair
	// refused counts consecutive refusals per held pair (with the first
	// refusal's time and the last error, for Drain diagnostics); a pair at
	// retryStallAfter no longer holds Drain open.
	refused map[pair]refusal
	closed  bool

	// viewInterval / viewChanged：SetObserverView 的限速，按玩家记最近一次生效的时间；受 mu 保护。
	viewInterval time.Duration
	viewChanged  map[int64]time.Time
	now          func() time.Time
}

type pair struct{ observer, subject int64 }

type hold struct {
	bands      map[string]int
	subscribed bool
	profile    entity.SyncProfile
}

func NewInterest(config InterestConfig) (*Interest, error) {
	if config.Manager == nil {
		return nil, entitysync.ErrManagerClosed
	}
	manager, err := NewAOI(config.AOI)
	if err != nil {
		return nil, err
	}
	observerOf := config.ObserverOf
	if observerOf == nil {
		observerOf = DefaultObserverOf
	}
	if config.ViewChangeInterval < 0 {
		return nil, fmt.Errorf("%w: negative view change interval", ErrInterestConfig)
	}
	viewInterval := config.ViewChangeInterval
	if viewInterval == 0 {
		viewInterval = DefaultViewChangeInterval
	}
	profile := config.Profile
	if profile == nil {
		profile = DefaultBandProfile
	}
	if config.BatchSize < 0 || config.Workers < 0 {
		return nil, ErrInterestConfig
	}
	in := &Interest{
		subjectRoles: make(map[int64]struct{}), observerRoles: make(map[int64]struct{}),
		batchSize: config.BatchSize, workers: config.Workers,
		aoi: manager, observerOf: observerOf, profile: profile,
		relations: make(map[string]*RelationSource), held: make(map[pair]*hold),
		viewInterval: viewInterval, viewChanged: make(map[int64]time.Time), now: time.Now,
	}
	in.subscriptions = config.Manager.NewSubscriptionSourceWithResubmit(in.resubmit)
	in.compareProfiles = config.Manager.CompareProfiles
	in.validatePriority = config.Manager.ValidateViewPriority
	in.viewSets = make(map[string]*entity.SyncViewSet, len(config.ViewSets))
	for name, views := range config.ViewSets {
		if name == "" || views == nil {
			return nil, fmt.Errorf("policy: invalid view set %q", name)
		}
		in.viewSets[name] = views
	}
	in.sourceProfiles = make(map[string]map[int]entity.SyncProfile, len(config.SourceProfiles))
	for source, bands := range config.SourceProfiles {
		if source != SourceSpatial && source != SourceSelf && source != SourceBlock && !slices.Contains(config.Relations, source) {
			return nil, fmt.Errorf("policy: unknown profile source %q", source)
		}
		copy := make(map[int]entity.SyncProfile, len(bands))
		for band, profile := range bands {
			if band < 0 {
				return nil, fmt.Errorf("policy: invalid profile band %d", band)
			}
			copy[band] = profile.Normalize()
		}
		in.sourceProfiles[source] = copy
	}
	in.sources = []Source{aoiSource{manager: manager}, blockSource{manager: manager}}
	if config.SelfVisible == nil || *config.SelfVisible {
		in.self = NewRelationSource(SourceSelf)
		in.sources = append(in.sources, in.self)
	}
	for _, name := range config.Relations {
		if name == "" || name == SourceSpatial || name == SourceSelf || name == SourceBlock {
			return nil, fmt.Errorf("policy: relation name %q is reserved or empty", name)
		}
		if _, dup := in.relations[name]; dup {
			return nil, fmt.Errorf("policy: relation %q named twice", name)
		}
		relation := NewRelationSource(name)
		in.relations[name] = relation
		in.sources = append(in.sources, relation)
	}
	if err := in.validateConfiguredProfiles(config); err != nil {
		return nil, err
	}
	if config.MaxQueuedFacts < 0 {
		return nil, errors.New("policy: negative queued fact capacity")
	}
	in.maxQueuedFacts = config.MaxQueuedFacts
	if in.maxQueuedFacts == 0 {
		in.maxQueuedFacts = 65536
	}
	if in.batchSize == 0 {
		in.batchSize = 1024
	}
	in.wake = config.Manager.WakeSync
	in.cancelQueue = config.Manager.RegisterPolicy(entitysync.PolicyHooks{Apply: in.applyQueued, Pending: in.queuedPending, Stalls: in.drainStalls})
	return in, nil
}

// DefaultViewChangeInterval 是 InterestConfig.ViewChangeInterval 的缺省值：同一玩家每秒至多改一次视野。
const DefaultViewChangeInterval = time.Second

// DefaultObserverOf 是 InterestConfig.ObserverOf 的缺省值：实体 ID 的唯一号就是玩家 ID（Player 实体），为 0 时不是观察者。
func DefaultObserverOf(entityID int64) (int64, bool) {
	playerID := entity.GetUniqueIDFromEntityID(entityID)
	return playerID, playerID != 0
}

// sessionOf 是观察者到会话的换算：观察者就是玩家 ID，会话 ID 等于玩家 ID。
func sessionOf(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }

// DefaultBandProfile is the profile mapping used when none is configured:
// band 0 (nearest) is the default profile, band n asks for LOD n.
func DefaultBandProfile(band int) entity.SyncProfile {
	if band <= 0 {
		return entity.SyncProfile{}
	}
	return entity.SyncProfile{Key: fmt.Sprintf("band%d", band), LOD: uint8(band)}
}

// aoiSource adapts the core manager to Source. Flush is called with
// Interest's lock held, which is why it takes none of its own.
type aoiSource struct{ manager *AOI }

func (aoiSource) Name() string             { return SourceSpatial }
func (s aoiSource) Flush() []InterestEvent { return s.manager.Flush() }

// Relation returns a configured relation source by name, or nil.
func (in *Interest) Relation(name string) *RelationSource {
	if in == nil {
		return nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.relations[name]
}

// Enter puts a player in: it both sees and is seen, and (when SelfVisible)
// it sees itself. playerID 是观察者（会话）编号，entityID 是玩家自身实体；view 为零值时取缺省视野。
// 已登记的玩家再次 Enter（重连）按这次的位置与视野重新登记，要保留原视野就传 ObserverView(playerID) 的结果。
// Its subject must already be registered with the manager; its session must already be open.
func (in *Interest) Enter(playerID, entityID int64, at spatial.Point, view ObserverView) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if playerID == 0 || entityID == 0 {
		return ErrInterestUnknown
	}
	if _, err := in.aoi.config.normalizeView(view); err != nil {
		return err
	}
	if err := in.aoi.AddSubject(entityID, at); err != nil {
		return err
	}
	if err := in.aoi.AddObserver(playerID, entityID, at, view); err != nil {
		return err
	}
	in.queueMu.Lock()
	in.subjectRoles[entityID] = struct{}{}
	in.observerRoles[playerID] = struct{}{}
	in.queueMu.Unlock()
	if in.self != nil {
		in.self.Set(playerID, []int64{entityID})
	}
	return nil
}

// Watch 登记一个只看、不被看的观察者（观战、GM 视角）：没有自身实体（self = 0），也没有 self 关系。
// 它与 Enter（既看又被看）、Show（只被看）并列；视点用 MoveView 移动，用 Unwatch 退出。会话须已打开。
func (in *Interest) Watch(playerID int64, at spatial.Point, view ObserverView) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if observer := in.aoi.observers[playerID]; observer != nil && observer.self != 0 {
		// 改成只看会让玩家看见自己的实体，而它仍被别人看见；先 Leave 再 Watch。
		return fmt.Errorf("%w: player %d is in with entity %d; Leave first", ErrInterestConfig, playerID, observer.self)
	}
	if err := in.aoi.AddObserver(playerID, 0, at, view); err != nil {
		return err
	}
	in.queueMu.Lock()
	in.observerRoles[playerID] = struct{}{}
	in.queueMu.Unlock()
	return nil
}

// Move is for something that both sees and is seen. Both tables, and in this
// order: a subject that moved without its observer having moved would see the
// world from where it used to be.
func (in *Interest) Move(playerID, entityID int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if err := in.aoi.MoveSubject(entityID, to); err != nil {
		return err
	}
	return in.aoi.MoveObserver(playerID, to)
}

// MoveView 只移动观察者的视点：Watch 登记的观察者用它移动；玩家控制英雄、载具时也可以让视点跟随被控制的实体。
func (in *Interest) MoveView(playerID int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.aoi.MoveObserver(playerID, to)
}

// SetObserverView 按客户端的 LOD / 视野请求换这个玩家的视野，直接生效（不排队、不走提交门：它不改业务状态，没有回滚）：
//   - 层级必须是配置里有的，否则返回 ErrObserverView；视野大小由服务端夹到允许范围并量化（AOIConfig）。
//   - 归一后与当前视野相同的请求什么也不做，不占限速。
//   - 同一玩家距上次生效不足 ViewChangeInterval 的请求返回 ErrObserverViewRate，计数后丢弃。
//   - 生效后完整重评：视野变大时新进入的目标订阅（下发全量），变小时超出的退订；只换层级时按新分档重发视图，订阅块不变。
//
// 返回此后实际生效的视野（拒绝时是当前视野），接入层可以把它告诉客户端。与 Move 一样，订阅变化由下一次 Apply
// （或排队模式下 Manager 的政策阶段）说给 Manager。
func (in *Interest) SetObserverView(playerID int64, view ObserverView) (ObserverView, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return ObserverView{}, errors.New("policy: interest is closed")
	}
	current, exists := in.aoi.ObserverViewOf(playerID)
	if !exists {
		return ObserverView{}, ErrInterestUnknown
	}
	next, err := in.aoi.config.normalizeView(view)
	if err != nil {
		metrics.IncCounter(viewRejectedMetric, metrics.Labels{"reason": "lod"}, 1)
		return current, err
	}
	if next == current {
		return current, nil
	}
	now := in.now()
	if last, limited := in.viewChanged[playerID]; limited && now.Sub(last) < in.viewInterval {
		metrics.IncCounter(viewRejectedMetric, metrics.Labels{"reason": "rate"}, 1)
		return current, ErrObserverViewRate
	}
	applied, err := in.aoi.SetObserverView(playerID, next)
	if err != nil {
		return current, err
	}
	in.viewChanged[playerID] = now
	return applied, nil
}

// ObserverView 返回玩家当前生效的视野。
func (in *Interest) ObserverView(playerID int64) (ObserverView, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return ObserverView{}, false
	}
	return in.aoi.ObserverViewOf(playerID)
}

// Leave takes a player out in both directions and out of every relation:
// playerID 作为观察者退出（关系 Clear），entityID 作为目标退出（关系 Forget）。
func (in *Interest) Leave(playerID, entityID int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.leaveLocked(playerID, entityID)
}

// Unwatch 让 Watch 登记的观察者退出；它没有目标的一半。
func (in *Interest) Unwatch(playerID int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.leaveLocked(playerID, 0)
}

// leaveLocked：entityID 为 0 时只退出观察者。调用方持有 mu。
func (in *Interest) leaveLocked(playerID, entityID int64) error {
	if in.self != nil {
		in.self.Clear(playerID)
	}
	for _, relation := range in.relations {
		relation.Clear(playerID)
		if entityID != 0 {
			relation.Forget(entityID)
		}
	}
	delete(in.viewChanged, playerID)
	// 退出期间 Queue* 仍可入队；最后一次短队列锁统一作废退出前的关系事实。
	// 空间事实按实际移除的角色分别作废，不把 Hide 当成观察者也退出。
	defer in.supersedeQueuedRelationsLocked(playerID, entityID)
	if err := in.aoi.RemoveObserver(playerID); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(0, playerID)
	if entityID == 0 {
		return nil
	}
	if err := in.aoi.RemoveSubject(entityID); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(entityID, 0)
	return nil
}

// Show / MoveShown / Hide are for things that are seen but do not see: a
// monster, a dropped item. 参数都是实体 ID。
func (in *Interest) Show(id int64, at spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if err := in.aoi.AddSubject(id, at); err != nil {
		return err
	}
	in.queueMu.Lock()
	in.subjectRoles[id] = struct{}{}
	in.queueMu.Unlock()
	return nil
}

func (in *Interest) MoveShown(id int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.aoi.MoveSubject(id, to)
}

func (in *Interest) Hide(id int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	for _, relation := range in.relations {
		relation.Forget(id)
	}
	defer in.supersedeQueuedRelationsLocked(0, id)
	if err := in.aoi.RemoveSubject(id); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(id, 0)
	return nil
}

// Resubscribe says every pair this observer holds to the manager again on the
// next Apply, and reports how many it queued. It is for an observer whose
// session the manager dropped on its own — a transport failure it reported
// through ManagerConfig.SessionLost — while the observer stays in the policy
// (it still has a live connection) and its session has been opened again: the
// manager forgot the subscriptions, the policy did not, and without this the
// reopened session would receive nothing until the pairs changed. Pairs other
// observers hold on this subject are untouched; they never left.
//
// The re-said pairs go through the retry list, so a manager that still
// refuses one (the session is not open yet) is asked again on every Apply.
//
// 实体被框架退回 remove（卸载后重载不了）之后重新登记的情形不需要调用它：Manager 把被撤销的订阅
// 交还 Interest，由 resubmit 自动重新提交。
// observer 是玩家 ID。
func (in *Interest) Resubscribe(observer int64) int {
	if in == nil {
		return 0
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return 0
	}
	queued := 0
	for key, current := range in.held {
		if key.observer != observer || !current.subscribed {
			continue
		}
		current.subscribed = false
		in.retry = append(in.retry, key)
		queued++
	}
	return queued
}

// resubmit 是 Interest 的重新提交入口：卸载后重载不了，框架退回 remove 撤销了本 Interest
// 的订阅，同 ID 实体又重新登记后，Manager 在 Flush 的政策阶段把这些订阅交还这里。与 Resubscribe 同一种修补：
// Manager 忘了，Interest 没忘。仍持有的 pair 按 Interest 自己的规则重新订阅（来源视图选择、ViewSets 校验、
// Manager 的优先级与字段白名单）；缺席期间已释放的 pair（观察者走远、关系解除、Hide）不在 held 里，不恢复。
// 被拒绝的 pair 进入重试表，下一次 Apply 以 Retry 报告并继续重说。
func (in *Interest) resubmit(retracted []entitysync.RetractedSubscription) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed || len(in.held) == 0 {
		return
	}
	wanted := make(map[int64][]entitysync.SessionID, len(retracted))
	for _, subscription := range retracted {
		wanted[subscription.Subject] = append(wanted[subscription.Subject], subscription.Session)
	}
	var refusals []Refusal // 已记入重试表，由下一次 Apply 报告
	for key, current := range in.held {
		sessions, retractedSubject := wanted[key.subject]
		if !retractedSubject || !current.subscribed || !slices.Contains(sessions, sessionOf(key.observer)) {
			continue
		}
		in.subscribe(key, current, true, &refusals)
	}
}

// Visible lists what an observer (player id) currently sees through distance.
func (in *Interest) Visible(observer int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	return in.aoi.Visible(observer)
}

// Apply drains every source, aggregates, and says the result to the manager:
// subscribes for pairs a first source claimed, unsubscribes for pairs the
// last source dropped, re-subscribes for pairs whose band changed. Subscribes
// the manager refuses come back as Refusals and are said again on the next
// Apply, for as long as the pair is held — a refusal is not a verdict on the
// pair. Calling it is the caller's choice: Interest is a
// state machine, not a goroutine.
func (in *Interest) Apply() []Refusal {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.applyLocked()
}

func (in *Interest) applyLocked() []Refusal {
	if in.closed {
		return nil
	}
	var refusals []Refusal
	// Take the retry list first: a refusal during this Apply goes on the list
	// for the NEXT one, not for a second attempt a few lines further down.
	queued := in.retry
	in.retry = nil
	touched := make(map[pair]struct{})
	for _, source := range in.sources {
		name := source.Name()
		events := source.Flush()
		for _, event := range events {
			key := pair{observer: event.Observer, subject: event.Subject}
			touched[key] = struct{}{}
			in.recordEvent(name, event)
		}
		// 仅内部 AOI 返回的本批事件由 Interest 独占；公开 Flush 的所有权不变。
		if name == SourceSpatial && len(events) > 0 && cap(events) <= 4096 {
			in.aoi.pending = events[:0]
		}
	}
	keys := make([]pair, 0, len(touched))
	for key := range touched {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b pair) int {
		if order := cmp.Compare(a.observer, b.observer); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})

	for _, key := range keys {
		in.settlePair(key, &refusals)
	}
	// Retries after the events, so that a pair a source event already spoke
	// for in this Apply is not said twice.
	for _, key := range queued {
		hold := in.held[key]
		if hold == nil || hold.subscribed {
			if hold == nil {
				delete(in.refused, key)
			}
			continue
		}
		in.subscribe(key, hold, true, &refusals)
	}
	return refusals
}

// recordEvent 记录一个来源对 pair 的最新可见性与 band，不立即发布。
// 先收齐所有来源的最终状态，再向 Manager 发布；spatial 转为 block/team
// 可见时不能先退订后重建，否则会打断对象基线并产生闪烁。
func (in *Interest) recordEvent(source string, event InterestEvent) {
	key := pair{observer: event.Observer, subject: event.Subject}
	current := in.held[key]
	if event.Kind == InterestLeave {
		if current != nil {
			delete(current.bands, source)
		}
		return
	}
	if current == nil {
		current = &hold{bands: make(map[string]int, 2)}
		in.held[key] = current
	}
	current.bands[source] = max(event.Band, 0)
}

func (in *Interest) settlePair(key pair, refusals *[]Refusal) {
	current := in.held[key]
	if current == nil {
		return
	}
	if len(current.bands) == 0 {
		delete(in.held, key)
		delete(in.refused, key)
		err := in.subscriptions.Unsubscribe(sessionOf(key.observer), key.subject)
		if err != nil && !errors.Is(err, entitysync.ErrSubscriptionNotFound) && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
			*refusals = append(*refusals, Refusal{Observer: key.observer, Subject: key.subject, Err: err})
		}
		return
	}
	if !current.subscribed {
		in.subscribe(key, current, false, refusals)
	} else {
		in.reband(key, current, refusals)
	}
}

// subscribe says one pair to the manager. The pair counts as subscribed from
// here — that is what makes the source counting work — and a refusal puts it
// on the retry list instead.
func (in *Interest) subscribe(key pair, current *hold, retry bool, refusals *[]Refusal) {
	current.profile = in.selectedProfile(current)
	current.subscribed = true
	err := in.validateProfile(current.profile)
	if err == nil {
		err = in.subscriptions.Subscribe(sessionOf(key.observer), key.subject, current.profile)
	}
	if err == nil {
		delete(in.refused, key)
		return
	}
	current.subscribed = false
	in.retry = append(in.retry, key)
	in.noteRefusedLocked(key, err)
	*refusals = append(*refusals, Refusal{Observer: key.observer, Subject: key.subject, Retry: retry, Err: err})
}

func (in *Interest) reband(key pair, current *hold, refusals *[]Refusal) {
	if !current.subscribed || in.selectedProfile(current) == current.profile {
		return
	}
	in.subscribe(key, current, false, refusals)
}

// selectedProfile 先解析每个来源的业务视图，再用 Manager 的同一规则选优。
// 不先折叠成最小 band，避免丢失 self/team 等来源的字段语义。
func (in *Interest) selectedProfile(current *hold) entity.SyncProfile {
	var best entity.SyncProfile
	first := true
	for source, band := range current.bands {
		profile := in.profileFor(source, band)
		if first || in.compareProfiles(profile, best) < 0 {
			best, first = profile, false
		}
	}
	return best
}

// Close 释放本 Interest 来源持有的订阅；其他政策、会话和实体注册继续存在。
func (in *Interest) Close() {
	if in == nil {
		return
	}
	if in.cancelQueue != nil {
		in.cancelQueue()
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.closed = true
	in.queueMu.Lock()
	in.queueClosed = true
	for _, fact := range in.facts {
		fact.finish()
	}
	in.facts = nil
	clear(in.subjectRoles)
	clear(in.observerRoles)
	in.queueMu.Unlock()
	for key := range in.held {
		_ = in.subscriptions.Unsubscribe(sessionOf(key.observer), key.subject)
	}
	clear(in.viewChanged)
	in.held = nil
	in.retry = nil
	in.refused = nil
}
