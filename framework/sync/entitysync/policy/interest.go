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

// InterestConfig shapes an Interest.
type InterestConfig struct {
	// MaxQueuedFacts 限制正式提交事实队列；零值为 65536。
	MaxQueuedFacts int
	// BatchSize 是每批就绪事实的软上限，零值1024；不切开同一Nest提交。
	BatchSize int
	// Workers 限制同一批次并行视野计算数；零值取GOMAXPROCS。
	Workers int
	// Manager receives the subscriptions. Required.
	Manager *entitysync.Manager
	// AOI 配置空间网格、缺省进入/离开半径、视野范围、LOD 层级与距离分档；由 NewAOI 校验。
	// 每个空间视野（玩家主视野与业务的空间视野）各自是 AOI 的一个观察者，MaxVisible 与 MaxObserverBlocks 都按视野计。
	AOI AOIConfig
	// ViewChangeInterval 是同一玩家两次主视野修改（SetObserverView）之间的最小间隔，超频的请求拒绝并计数；
	// 零值 1 秒，负数拒绝构造。服务端改业务空间视野（SetSpatialView）不限速。
	ViewChangeInterval time.Duration
	// BandLayer 把空间视野的距离档换成同步层级（订阅要的视图）。nil 取 DefaultBandProfile：
	// 0 档 → 缺省视图，n 档 → {Key: "bandN", LOD: n}。
	BandLayer func(band int) entity.SyncProfile
	// BlockLayer 是额外块覆盖（QueueObservedBlocks）给的层级；零值跟随空间视野 0 档。
	BlockLayer entity.SyncProfile
	// SelfLayer 是玩家经 self 看自己的层级；零值是缺省视图（"default"）。
	SelfLayer entity.SyncProfile
	// ViewSets 非空时校验本 Interest 涉及的实体类型及其视图（每个类型都要声明用到的每个层级），并用各层级的字段
	// 判断包含关系：同一目标被几个层级看见时，被别的层级包含的不再单独订阅，剩下的取并集（见 selectedProfile）。
	// 不配置时不知道字段，几个不同的层级一律取并集身份（entity.UnionSyncProfiles）。构造时复制 map。
	ViewSets map[string]*entity.SyncViewSet
	// SelfVisible 控制玩家是否通过 self 看见自己；空间视野不会订阅自己跟随的实体。
	// nil 或 true 启用，只有显式 false 才关闭。
	SelfVisible *bool
	// MaxViewsPerPlayer 限制一个玩家同时挂接的视野数（含主视野），零值 64，超出 AttachView / Enter / Watch 返回 ErrViewLimit。
	MaxViewsPerPlayer int
	// MaxPlayersPerView 限制一个视野同时挂接的玩家数（共享视野的扇出），零值 4096，超出 AttachView 返回 ErrViewLimit。
	MaxPlayersPerView int
	// MaxPlayerBlocks 是一个玩家全部空间视野合计最多订阅的块数，启动期按 worstObserverBlocks × MaxViewsPerPlayer 核算，
	// 超出即拒绝构造（ErrInterestBudget）。零值取 DefaultMaxObserverBlocks × DefaultMaxViewsPerPlayer，
	// 即缺省配置下只有单个视野已经超预算时才会触发；调大 MaxViewsPerPlayer 或视野半径时由它把总成本挡在启动期。
	MaxPlayerBlocks int
}

// Interest 的视野与挂接缺省上限。
const (
	DefaultMaxViewsPerPlayer = 64
	DefaultMaxPlayersPerView = 4096
)

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

// Interest 按视野组织订阅：视野（View）是可见性来源，分空间视野（一个点 + 视野大小与 LOD + 可选跟随实体）与
// 关系视野（显式实体集合 + 一个同步层级）两类；玩家与视野多对多（AttachView / DetachView），玩家 Enter / Watch 时
// 自带一个主视野。合并成每个 (玩家, 目标) 一份订阅说给 Manager。
//
// 编号：玩家 ID 就是会话 ID；目标是实体 ID；视野 ID（ViewID）由业务分配，0 保留给主视野。合并键 (player, subject)
// 就是 Manager 的 (session, subject)。
//
// 合并是要读懂的部分：一个 pair 可以同时被几个视野持有——站在身边的盟友既在主视野里又在联盟视野里，阵营的两个单位
// 都看得见同一个敌人——held 按视野记各自给的层级：第一个持有的视野订阅，最后一个释放的视野才退订，所以一个视野
// 看不见了不会把另一个视野还要的订阅退掉。层级按 selectedProfile 合并：包含关系取数据多的，不包含取并集。
//
// 锁：mu 串行化政策批次、生命周期、AOI 与订阅归并；批次内按空间视野并行计算。Queue* 只持 queueMu，不等待空间计算；
// 业务视野操作（Define* / Set* / Attach / Detach / Remove）只持 viewMu，在登记表里改事实并记一条变更，由下一次
// Apply（或排队模式下 Manager 的政策阶段）按顺序应用，同样不等待空间计算。锁序 mu → queueMu、mu → viewMu，
// queueMu 与 viewMu 不同时持有。
type Interest struct {
	// queueMu 只保护事实准入和角色登记；不能包住空间计算或订阅操作。
	queueMu     sync.Mutex
	queueClosed bool
	// subjectRoles 按实体 ID 记录已登记的目标，供事实准入检查。
	subjectRoles   map[int64]struct{}
	inflight       int
	batchSize      int
	workers        int
	queueActive    bool
	facts          []queuedInterestFact
	maxQueuedFacts int
	wake           func()
	cancelQueue    func()

	// 视野登记表，见 view.go；viewMu 保护。
	registry viewRegistry

	mu               sync.Mutex
	subscriptions    *entitysync.SubscriptionSource
	aoi              *AOI
	selfVisible      bool
	bandLayer        func(int) entity.SyncProfile
	blockLayer       entity.SyncProfile
	selfLayer        entity.SyncProfile
	compareProfiles  func(entity.SyncProfile, entity.SyncProfile) int
	viewSets         map[string]*entity.SyncViewSet
	validatePriority func(entity.SyncView) error
	// covers 缓存层级之间的包含关系（ViewSets 不变），受 mu 保护。
	covers map[[2]entity.SyncProfile]bool

	// 已应用的视野状态（mu 保护）：spatialPlayers 是每个空间视野（AOI 观察者编号）已生效的挂接玩家，AOI 事件按它扇出；
	// followers 是实体 → 跟随它的空间视野，实体移动时一起移动；selfOf 是玩家 → 经 self 看见的自身实体。
	spatialPlayers map[int64]map[int64]struct{}
	followers      map[int64]map[int64]struct{}
	selfOf         map[int64]int64

	// held maps a pair to the views currently claiming it and to whether
	// the manager has been told about it. touched 是自上次 Apply 以来贡献变过的 pair。
	held    map[pair]*hold
	touched map[pair]struct{}
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

// contribution 是一个视野对一个 pair 的持有：view 是视野的内部编号（空间视野即 AOI 观察者编号），
// block 区分空间视野的距离可见与额外块覆盖；view 为 0 是 self。
type contribution struct {
	view  int64
	block bool
}

// hold 记各视野给这个 pair 的层级，以及 Manager 是否已被告知、告知的是哪个视图。
type hold struct {
	layers     map[contribution]entity.SyncProfile
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
	if config.ViewChangeInterval < 0 {
		return nil, fmt.Errorf("%w: negative view change interval", ErrInterestConfig)
	}
	viewInterval := config.ViewChangeInterval
	if viewInterval == 0 {
		viewInterval = DefaultViewChangeInterval
	}
	bandLayer := config.BandLayer
	if bandLayer == nil {
		bandLayer = DefaultBandProfile
	}
	if config.BatchSize < 0 || config.Workers < 0 || config.MaxViewsPerPlayer < 0 || config.MaxPlayersPerView < 0 || config.MaxPlayerBlocks < 0 {
		return nil, ErrInterestConfig
	}
	maxViews := cmp.Or(config.MaxViewsPerPlayer, DefaultMaxViewsPerPlayer)
	maxPlayers := cmp.Or(config.MaxPlayersPerView, DefaultMaxPlayersPerView)
	playerBlocks := cmp.Or(config.MaxPlayerBlocks, DefaultMaxObserverBlocks*DefaultMaxViewsPerPlayer)
	if config.AOI.BlockSize > 0 {
		if worst := manager.config.worstObserverBlocks(); worst > int64(playerBlocks)/int64(maxViews) {
			return nil, fmt.Errorf("%w: %d views per player × %d blocks per view exceeds the per-player budget %d",
				ErrInterestBudget, maxViews, worst, playerBlocks)
		}
	}
	in := &Interest{
		subjectRoles: make(map[int64]struct{}),
		batchSize:    config.BatchSize, workers: config.Workers,
		aoi: manager, bandLayer: bandLayer, selfVisible: config.SelfVisible == nil || *config.SelfVisible,
		selfLayer:      config.SelfLayer.Normalize(),
		held:           make(map[pair]*hold),
		touched:        make(map[pair]struct{}),
		spatialPlayers: make(map[int64]map[int64]struct{}),
		followers:      make(map[int64]map[int64]struct{}),
		selfOf:         make(map[int64]int64),
		covers:         make(map[[2]entity.SyncProfile]bool),
		viewInterval:   viewInterval, viewChanged: make(map[int64]time.Time), now: time.Now,
	}
	in.registry.init(maxViews, maxPlayers)
	in.blockLayer = config.BlockLayer
	if in.blockLayer == (entity.SyncProfile{}) {
		in.blockLayer = bandLayer(0)
	}
	in.blockLayer = in.blockLayer.Normalize()
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
	if err := in.validateConfiguredLayers(); err != nil {
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
	in.registry.wake = in.wake
	in.cancelQueue = config.Manager.RegisterPolicy(entitysync.PolicyHooks{Apply: in.applyQueued, Pending: in.queuedPending, Stalls: in.drainStalls})
	return in, nil
}

// DefaultViewChangeInterval 是 InterestConfig.ViewChangeInterval 的缺省值：同一玩家每秒至多改一次视野。
const DefaultViewChangeInterval = time.Second

// sessionOf 是观察者到会话的换算：观察者就是玩家 ID，会话 ID 等于玩家 ID。
func sessionOf(observer int64) entitysync.SessionID { return entitysync.SessionID(observer) }

// DefaultBandProfile is the layer mapping used when none is configured:
// band 0 (nearest) is the default profile, band n asks for LOD n.
func DefaultBandProfile(band int) entity.SyncProfile {
	if band <= 0 {
		return entity.SyncProfile{}
	}
	return entity.SyncProfile{Key: fmt.Sprintf("band%d", band), LOD: uint8(band)}
}

// ---- 玩家与主视野 ----
//
// 主视野是玩家自带的空间视野（镜头），只挂给玩家自己，在按玩家的入口里用 MainView（0）指代；业务视野的 ID 不能是 0。
// Enter 建立的主视野跟随玩家自身实体（实体移动时一起移动，并排除自己）；Watch 建立的不跟随（只看不被看）。

// Enter puts a player in: its entity is seen, its main view sees, and (when
// SelfVisible) it sees itself. playerID 是观察者（会话）编号，entityID 是玩家自身实体；view 为零值时取缺省视野。
// 已登记的玩家再次 Enter（重连）按这次的位置与视野重新登记主视野，要保留原视野就传 ObserverView(playerID) 的结果。
// 主视野计入 MaxViewsPerPlayer。Its subject must already be registered with the manager; its session must already be open.
func (in *Interest) Enter(playerID, entityID int64, at spatial.Point, view ObserverView) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if playerID == 0 || entityID == 0 {
		return ErrInterestUnknown
	}
	if !in.aoi.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	normalized, err := in.aoi.config.normalizeView(view)
	if err != nil {
		return err
	}
	in.syncViewsLocked()
	main, err := in.registry.mainView(playerID)
	if err != nil {
		return err
	}
	if err := in.placeSubjectLocked(entityID, at); err != nil {
		return err
	}
	in.placeMainLocked(playerID, main, entityID, at, normalized)
	in.queueMu.Lock()
	in.subjectRoles[entityID] = struct{}{}
	in.queueMu.Unlock()
	if in.selfVisible {
		if old, ok := in.selfOf[playerID]; ok && old != entityID {
			in.contribute(pair{observer: playerID, subject: old}, contribution{}, entity.SyncProfile{}, false)
		}
		in.selfOf[playerID] = entityID
		in.contribute(pair{observer: playerID, subject: entityID}, contribution{}, in.selfLayer, true)
	}
	return nil
}

// Watch 登记一个只看、不被看的玩家（观战、GM 视角）：主视野不跟随实体（self = 0），也没有 self。
// 它与 Enter（既看又被看）、Show（只被看）并列；视点用 MoveView 移动，用 Unwatch 退出。会话须已打开。
func (in *Interest) Watch(playerID int64, at spatial.Point, view ObserverView) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	if playerID == 0 {
		return ErrInterestUnknown
	}
	in.syncViewsLocked()
	if main := in.registry.mainOf(playerID); main != nil {
		if observer := in.aoi.observers[main.handle]; observer != nil && observer.self != 0 {
			// 改成只看会让玩家看见自己的实体，而它仍被别人看见；先 Leave 再 Watch。
			return fmt.Errorf("%w: player %d is in with entity %d; Leave first", ErrInterestConfig, playerID, observer.self)
		}
	}
	if !in.aoi.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	normalized, err := in.aoi.config.normalizeView(view)
	if err != nil {
		return err
	}
	main, err := in.registry.mainView(playerID)
	if err != nil {
		return err
	}
	in.placeMainLocked(playerID, main, 0, at, normalized)
	return nil
}

// placeMainLocked 放置（或重新放置）主视野，并把它挂给玩家自己。调用方持有 mu、已同步登记表。
func (in *Interest) placeMainLocked(playerID int64, main *view, follow int64, at spatial.Point, normalized ObserverView) {
	if in.spatialPlayers[main.handle] == nil {
		in.spatialPlayers[main.handle] = map[int64]struct{}{playerID: {}}
	}
	in.placeViewLocked(main.handle, follow, at, normalized)
}

// Move 移动一个目标（实体 ID），跟随它的空间视野（例如玩家的主视野、单位的视野）一起移动：先移目标再移视野，
// 视野没跟上的目标会从旧位置看世界。
func (in *Interest) Move(entityID int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	in.syncViewsLocked()
	if _, exists := in.aoi.subjects[entityID]; !exists {
		return ErrInterestUnknown
	}
	return in.placeSubjectLocked(entityID, to)
}

// MoveView 只移动玩家主视野的视点：Watch 登记的玩家用它移动。主视野跟随实体时，实体下一次移动会把它带回实体位置。
func (in *Interest) MoveView(playerID int64, to spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	in.syncViewsLocked()
	main := in.registry.mainOf(playerID)
	if main == nil {
		return ErrInterestUnknown
	}
	return in.aoi.MoveObserver(main.handle, to)
}

// SetObserverView 按客户端的 LOD / 视野请求换这个玩家主视野的视野，直接生效（不排队、不走提交门：它不改业务状态，没有回滚）：
//   - 层级必须是配置里有的，否则返回 ErrObserverView；视野大小由服务端夹到允许范围并量化（AOIConfig）。
//   - 归一后与当前视野相同的请求什么也不做，不占限速。
//   - 同一玩家距上次生效不足 ViewChangeInterval 的请求返回 ErrObserverViewRate，计数后丢弃。
//   - 生效后完整重评：视野变大时新进入的目标订阅（下发全量），变小时超出的退订；只换层级时按新分档重发视图，订阅块不变。
//
// 返回此后实际生效的视野（拒绝时是当前视野），接入层可以把它告诉客户端。订阅变化由下一次 Apply
// （或排队模式下 Manager 的政策阶段）说给 Manager。业务空间视野由服务端用 SetSpatialView 改，不限速。
func (in *Interest) SetObserverView(playerID int64, view ObserverView) (ObserverView, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return ObserverView{}, errors.New("policy: interest is closed")
	}
	in.syncViewsLocked()
	main := in.registry.mainOf(playerID)
	if main == nil {
		return ObserverView{}, ErrInterestUnknown
	}
	current, exists := in.aoi.ObserverViewOf(main.handle)
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
	applied, err := in.aoi.SetObserverView(main.handle, next)
	if err != nil {
		return current, err
	}
	in.viewChanged[playerID] = now
	return applied, nil
}

// ObserverView 返回玩家主视野当前生效的视野。
func (in *Interest) ObserverView(playerID int64) (ObserverView, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return ObserverView{}, false
	}
	in.syncViewsLocked()
	main := in.registry.mainOf(playerID)
	if main == nil {
		return ObserverView{}, false
	}
	return in.aoi.ObserverViewOf(main.handle)
}

// Leave takes a player out in both directions: 玩家（playerID）解除全部视野挂接、撤掉主视野与 self；
// 实体（entityID）作为目标退出空间索引，并从所有关系视野的成员里去掉。业务视野本身保留，玩家重新进场后由业务重新挂接。
func (in *Interest) Leave(playerID, entityID int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.leaveLocked(playerID, entityID)
}

// Unwatch 让 Watch 登记的玩家退出；它没有目标的一半。
func (in *Interest) Unwatch(playerID int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	return in.leaveLocked(playerID, 0)
}

// leaveLocked：entityID 为 0 时只退出玩家。调用方持有 mu。
func (in *Interest) leaveLocked(playerID, entityID int64) error {
	in.syncViewsLocked()
	main := in.registry.leave(playerID, entityID)
	// 登记表里的解除（业务视野的挂接、关系成员）先按顺序应用，再撤主视野。
	in.syncViewsLocked()
	if self, ok := in.selfOf[playerID]; ok {
		delete(in.selfOf, playerID)
		in.contribute(pair{observer: playerID, subject: self}, contribution{}, entity.SyncProfile{}, false)
	}
	delete(in.viewChanged, playerID)
	if main != nil {
		in.removeSpatialLocked(main.handle)
	}
	if entityID != 0 {
		if err := in.aoi.RemoveSubject(entityID); err != nil {
			return err
		}
		in.supersedeQueuedMovesLocked(entityID)
	}
	if main == nil {
		return ErrInterestUnknown
	}
	return nil
}

// Show / Hide are for things that are seen but do not see by themselves: a
// monster, a dropped item. 参数都是实体 ID；移动用 Move。Show 时已有空间视野跟随它的，视野跳到它的位置。
func (in *Interest) Show(id int64, at spatial.Point) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	in.syncViewsLocked()
	if err := in.placeSubjectLocked(id, at); err != nil {
		return err
	}
	in.queueMu.Lock()
	in.subjectRoles[id] = struct{}{}
	in.queueMu.Unlock()
	return nil
}

// Hide 让目标退出空间索引，并从所有关系视野的成员里去掉；跟随它的空间视野留在原地（跟随关系保留）。
func (in *Interest) Hide(id int64) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return errors.New("policy: interest is closed")
	}
	in.syncViewsLocked()
	in.registry.forget(id)
	in.syncViewsLocked()
	if err := in.aoi.RemoveSubject(id); err != nil {
		return err
	}
	in.supersedeQueuedMovesLocked(id)
	return nil
}

// placeSubjectLocked 登记或移动目标，并把跟随它的空间视野移到同一位置。调用方持有 mu。
func (in *Interest) placeSubjectLocked(id int64, at spatial.Point) error {
	if err := in.aoi.AddSubject(id, at); err != nil {
		return err
	}
	in.moveFollowersLocked(id, at)
	return nil
}

// moveFollowersLocked 把跟随实体 id 的空间视野移到 at（按视野编号顺序，结果确定）。
func (in *Interest) moveFollowersLocked(id int64, at spatial.Point) {
	handles := in.followers[id]
	if len(handles) == 0 {
		return
	}
	ordered := make([]int64, 0, len(handles))
	for handle := range handles {
		ordered = append(ordered, handle)
	}
	slices.Sort(ordered)
	for _, handle := range ordered {
		_ = in.aoi.MoveObserver(handle, at)
	}
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
// Manager 忘了，Interest 没忘。仍持有的 pair 按 Interest 自己的规则重新订阅（层级合并、ViewSets 校验、
// Manager 的优先级与字段白名单）；缺席期间已释放的 pair（视野走远、解除挂接、Hide）不在 held 里，不恢复。
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

// Visible lists what a player's main view currently sees through distance.
func (in *Interest) Visible(playerID int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.syncViewsLocked()
	main := in.registry.mainOf(playerID)
	if main == nil {
		return nil
	}
	return in.aoi.Visible(main.handle)
}

// Seen 返回玩家经任一视野（含 self）持有的全部目标（升序），即本 Interest 要它订阅的集合。先应用登记表与 AOI 的变化；
// 遍历全部 pair，供诊断与压测核对，不要放在热路径上。
func (in *Interest) Seen(playerID int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.syncViewsLocked()
	in.drainAOILocked()
	var seen []int64
	for key, current := range in.held {
		if key.observer == playerID && len(current.layers) > 0 {
			seen = append(seen, key.subject)
		}
	}
	slices.Sort(seen)
	return seen
}

// Apply 把视野登记表里的变更与 AOI 的可见性变化合并进 held，再说给 Manager：
// subscribes for pairs a first view claimed, unsubscribes for pairs the
// last view dropped, re-subscribes for pairs whose layers changed. Subscribes
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
	in.syncViewsLocked()
	in.drainAOILocked()
	keys := make([]pair, 0, len(in.touched))
	for key := range in.touched {
		keys = append(keys, key)
	}
	clear(in.touched)
	slices.SortFunc(keys, func(a, b pair) int {
		if order := cmp.Compare(a.observer, b.observer); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})
	// 先收齐所有视野的最终贡献，再向 Manager 发布：空间转为块覆盖或关系可见时不能先退订后重建，否则会打断对象基线并产生闪烁。
	for _, key := range keys {
		in.settlePair(key, &refusals)
	}
	// Retries after the events, so that a pair a view already spoke
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

// contribute 记录一个视野对 pair 的持有（enter）或释放，不立即发布；pair 记进 touched，由下一次 Apply 结算。
func (in *Interest) contribute(key pair, source contribution, layer entity.SyncProfile, enter bool) {
	current := in.held[key]
	if !enter {
		if current != nil {
			if _, held := current.layers[source]; held {
				delete(current.layers, source)
				in.touched[key] = struct{}{}
			}
		}
		return
	}
	if current == nil {
		current = &hold{layers: make(map[contribution]entity.SyncProfile, 2)}
		in.held[key] = current
	}
	if previous, held := current.layers[source]; held && previous == layer {
		return
	}
	current.layers[source] = layer
	in.touched[key] = struct{}{}
}

func (in *Interest) settlePair(key pair, refusals *[]Refusal) {
	current := in.held[key]
	if current == nil {
		return
	}
	if len(current.layers) == 0 {
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
// here — that is what makes the view counting work — and a refusal puts it
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

// Close 释放本 Interest 持有的订阅；其他政策、会话和实体注册继续存在。之后的视野操作返回错误。
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
	in.registry.close()
	in.queueMu.Lock()
	in.queueClosed = true
	for _, fact := range in.facts {
		fact.finish()
	}
	in.facts = nil
	clear(in.subjectRoles)
	in.queueMu.Unlock()
	for key := range in.held {
		_ = in.subscriptions.Unsubscribe(sessionOf(key.observer), key.subject)
	}
	clear(in.viewChanged)
	in.held = nil
	in.touched = nil
	in.retry = nil
	in.refused = nil
}
