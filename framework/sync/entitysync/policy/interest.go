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
	// BatchSize 是一步取用就绪事实的软上限，零值1024：到了上限之后在第一个前缀边界停（已取用的提交全部结束之后），
	// 不切开同一 Nest 提交，所以一步可以多出交错进来的条目（见 runLogLocked）。
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
	// ApplyMode 选排队事实（QueueMove / QueueSpatial / QueueObservedBlocks）的应用方式，由业务按游戏类型选，
	// 见 ApplyMode 与 DESIGN-TODO D19“两种应用模式”。零值 ApplySync。
	ApplyMode ApplyMode
	// MaxViewsPerPlayer 限制一个玩家同时挂接的视野数（含主视野与关系视野），超出 AttachView / Enter / Watch 返回 ErrViewLimit。
	// 零值：ApplySync 取 SyncApplyMaxViewsPerPlayer（9），ApplyAsync 取 DefaultMaxViewsPerPlayer（64）。
	// ApplySync 下显式配置超过 SyncApplyMaxViewsPerPlayer 拒绝构造（ErrInterestConfig），报错提示改用 ApplyAsync。
	MaxViewsPerPlayer int
	// MaxPlayersPerView 限制一个视野同时挂接的玩家数（共享视野的扇出），零值 4096，超出 AttachView 返回 ErrViewLimit。
	MaxPlayersPerView int
	// MaxPlayerBlocks 是一个玩家全部空间视野合计最多订阅的块数，启动期按 worstObserverBlocks × MaxViewsPerPlayer 核算，
	// 超出即拒绝构造（ErrInterestBudget）。零值取 DefaultMaxObserverBlocks × DefaultMaxViewsPerPlayer，
	// 即缺省配置下只有单个视野已经超预算时才会触发；调大 MaxViewsPerPlayer 或视野半径时由它把总成本挡在启动期。
	MaxPlayerBlocks int
	// MaxSpatialViews 限制本 Interest 经 DefineSpatialView 定义的空间视野总数（挂没挂接都算，不含玩家主视野），超出
	// DefineSpatialView 返回 ErrSpatialViewLimit 并计数。每个空间视野都是一个 AOI 观察者，定义了没挂接的照样参与每次
	// 移动的 AOI 计算，MaxViewsPerPlayer 只数挂接、管不到它们（维护者 2026-10-22）。零值取 DefaultMaxSpatialViews，负数
	// 拒绝构造。RemoveView 释放名额。
	MaxSpatialViews int
	// OnRefusal 收到 Manager 政策阶段里结算时被拒绝的订阅（显式 Apply 的拒绝由它的返回值给出，不再重复）：
	// 在政策阶段、持 Interest 的应用侧锁调用，不得阻塞、不得调用本 Interest。nil 不报告（指标与 Drain 诊断照旧）。
	OnRefusal func(Refusal)
}

// Interest 的视野与挂接缺省上限。DefaultMaxViewsPerPlayer 是 ApplyAsync 的缺省；ApplySync 的缺省与上限是
// SyncApplyMaxViewsPerPlayer。
const (
	DefaultMaxViewsPerPlayer = 64
	DefaultMaxPlayersPerView = 4096
)

// DefaultMaxSpatialViews 是 MaxSpatialViews 的缺省：8192。依据 D19“两种应用模式”的实测（1000 玩家、每视野可见约 50、
// 50ms 同步周期）：ApplySync 下 1000 玩家各 8 个额外空间视野（共 8000 个）时 Flush 约半个周期，正是阈值
// SyncApplyMaxExtraViews（8）取的“Flush 不超过半个周期”的位置；16000 个时 Flush 贴着周期、开始丢 tick。所以缺省放下
// “1000 玩家各挂满 8 个空间视野”，再多（眼位泄漏、单位视野没有 RemoveView）在定义时就拒绝，而不是拖慢整个场景的
// 政策阶段。视野更多的玩法（RTS，ApplyAsync）按自己的密度压测后显式调大。
const DefaultMaxSpatialViews = 8192

// ApplyMode 是排队事实的应用方式。两种模式的语义逐项相同——只有提交后的事实生效（回滚 / 拒绝的永远不被看见）、
// 同一实体按入队顺序生效、Leave / Hide 对排队事实的改写、内容与它带来的进出视野在同一帧（发布屏障到结算之后才释放）、
// Drain 等到在途事实全部应用并结算、Close 释放全部屏障——只有“结果何时可见”和“线程模型”不同：
//
//   - ApplySync（缺省）：Manager Flush 的政策阶段在 Interest.mu 内执行日志、随即结算，执行 + 结算不超过同步周期的
//     一半（预计超出时剩余的留到之后的 Flush，见 planSyncStep）；不启动应用线程。提交之后的下一次 Flush 就能看到
//     结果，重负载下晚一两个周期。AOI 计算在 Flush 的关键路径上，
//     所以每个玩家的视野数有上限（SyncApplyMaxViewsPerPlayer）。适合每玩家额外视野不超过 8 个的游戏：
//     MMO、卡牌、休闲等（主视野 + 队伍 / 公会 / 联盟关系视野 + 少量召唤物）。
//   - ApplyAsync：每个 Interest 一个应用线程（第一条排队事实时启动）分批应用，Flush 的政策阶段只结算。Flush 不随
//     AOI 工作量增长、不会因 AOI 超过同步周期；代价是结果晚一点可见：提交后立即 Flush 不一定看得到，要看到全部
//     结果用 Manager.Drain；periodic 下尾延迟约多一个同步周期。适合每玩家额外视野多于 8 个的游戏：RTS（每个单位
//     一个视野）、状态同步 MOBA 的阵营视野（每个友方单位一个空间视野挂给全阵营）、大量召唤物 / 眼位。
type ApplyMode uint8

const (
	ApplySync ApplyMode = iota
	ApplyAsync
)

// SyncApplyMaxExtraViews 是 ApplySync 下每个玩家主视野之外最多的视野数；SyncApplyMaxViewsPerPlayer = 主视野 + 它。
// 来源：维护者 2026-10-21 决定按“每玩家额外视野 8 个以内 / 以上”分两种模式、由业务选择。实测依据（本机 M5、
// sync-aoi 原场景 1000 玩家 / 1 万实体 / 20Hz、每视野可见约 50，2026-10-21 两轮对照）：ApplySync 的 Flush 均值
// 4 / 8 / 16 个额外视野时约 15 / 26 / 46ms，8 个约半个 50ms 周期、帧数满，16 个贴着周期开始丢 tick；数据见 DESIGN-TODO
// D19“两种应用模式”。Flush 还与玩家数、可见数、移动比例成正比，这个上限只是按每玩家视野数的近似。
// 关系视野（队伍、联盟）也计入 MaxViewsPerPlayer。
const (
	SyncApplyMaxExtraViews     = 8
	SyncApplyMaxViewsPerPlayer = 1 + SyncApplyMaxExtraViews
)

// String 返回配置里用的名字：sync / async。
func (m ApplyMode) String() string {
	switch m {
	case ApplySync:
		return "sync"
	case ApplyAsync:
		return "async"
	}
	return fmt.Sprintf("ApplyMode(%d)", uint8(m))
}

// ParseApplyMode 解析 sync / async（配置与压测开关用）；空串是缺省 ApplySync。
func ParseApplyMode(s string) (ApplyMode, error) {
	switch s {
	case "", "sync":
		return ApplySync, nil
	case "async":
		return ApplyAsync, nil
	}
	return 0, fmt.Errorf("%w: apply mode %q (want sync or async)", ErrInterestConfig, s)
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
// 单写者（REFACTOR-2026-10-21-interest-single-writer）：全部写入口——Enter / Leave / Show / Hide / Move / 视野操作 /
// QueueMove 等——只取登记侧短锁（registry.mu）：校验参数、改登记事实、追加一条有序日志，错误当场返回，不等待空间
// 计算。只有应用方持 mu 按日志顺序执行：同步模式在 Manager Flush 的政策阶段，异步模式在应用线程，另有显式 Apply 与
// 诊断读。所以业务线程（Nest worker、提交后回调）上的调用不会等 AOI 批次，且“先调用的先生效”对全部入口成立。
// 锁序 mu → registry.mu。
type Interest struct {
	// registry 是登记侧（视野登记表、已登记目标与代际、主视野限速、有序日志），见 view.go。
	registry  viewRegistry
	batchSize int
	workers   int
	// maxQueuedFacts / applyMode / syncBudget / onRefusal 构造后不变。syncBudget 是同步模式一次政策阶段（执行日志 +
	// 结算）的耗时预算，见 settleQueued。
	maxQueuedFacts int
	applyMode      ApplyMode
	syncBudget     time.Duration
	onRefusal      func(Refusal)
	wake           func()
	cancelQueue    func()
	// 应用线程（interest_queue.go）：applierStarted 受 registry.mu 保护；applierWake 容量 1，合并唤醒。
	applierStarted bool
	applierWake    chan struct{}
	applierStop    chan struct{}
	applierDone    chan struct{}
	applyGather    time.Duration
	// viewInterval / now：SetObserverView 的限速间隔与 Interest 的时钟（构造后不变；测试可替换 now）。now 也给同步模式
	// 政策阶段的预算计时（只在 settleQueued 里、持 mu 且不持 registry.mu 时读）。
	viewInterval time.Duration
	now          func() time.Time

	// ---- 应用侧：以下全部受 mu 保护，只有应用方读写 ----
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
	// covers 缓存层级之间的包含关系（ViewSets 不变）。
	covers map[[2]entity.SyncProfile]bool

	// 已应用的视野状态：spatialPlayers 是每个空间视野（AOI 观察者编号）已生效的挂接玩家，AOI 事件按它扇出；
	// followers 是实体 → 跟随它的空间视野，实体移动时一起移动；selfOf 是玩家 → 经 self 看见的自身实体；
	// placed 是已应用的目标 → 代际号（门控事实按它判断是否作废）。
	spatialPlayers map[int64]map[int64]struct{}
	followers      map[int64]map[int64]struct{}
	selfOf         map[int64]int64
	placed         map[int64]uint64
	// departedPlayers / departedSubjects：自上次结算以来被 Leave / Unwatch / Hide 撤除的玩家与目标。同一次结算里
	// 它们的 pair 又被持有时，先退订再订阅（业务可能在两次调用之间注销 / 重新登记了目标、关闭 / 重开了会话）。
	departedPlayers  map[int64]struct{}
	departedSubjects map[int64]struct{}

	// held maps a pair to the views currently claiming it and to whether
	// the manager has been told about it. touched 是自上次结算以来贡献变过的 pair。
	held    map[pair]*hold
	touched map[pair]struct{}
	// retry holds the pairs whose subscribe the manager refused, waiting
	// for the next settle to say them again.
	retry []pair
	// applied 是已应用到 AOI、还没结算的事实（发布屏障仍持有，计入预算）；appliedAt 是其中最早一批应用完成的时刻；
	// applyErr 是应用时攒下的错误，由下一次政策阶段返回。
	applied   []queuedInterestFact
	appliedAt time.Time
	applyErr  error
	// unsettled：应用方执行过条目（命令或事实）、还没结算——AOI 事件或 touched 里的变化还没说给 Manager。
	// queuedPending 据此让 Drain 等到结算；异步模式下应用线程可能恰在某次 Flush 结算之后、Drain 检查之前执行完一步。
	unsettled bool
	// unsettledUnits 是自上次结算以来应用方执行的单位数（一条命令或一条取用的事实），costs 是同步模式政策阶段的
	// 成本估计（每单位执行 / 结算的耗时），两者都只给预算用，见 planSyncStep。
	unsettledUnits int
	costs          applyCosts
	// refused counts consecutive refusals per held pair (with the first
	// refusal's time and the last error, for Drain diagnostics); a pair at
	// retryStallAfter no longer holds Drain open.
	refused map[pair]refusal
	closed  bool
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
	if config.BatchSize < 0 || config.Workers < 0 || config.MaxViewsPerPlayer < 0 || config.MaxPlayersPerView < 0 || config.MaxPlayerBlocks < 0 ||
		config.MaxSpatialViews < 0 {
		return nil, ErrInterestConfig
	}
	maxViews, err := viewsPerPlayerFor(config.ApplyMode, config.MaxViewsPerPlayer)
	if err != nil {
		return nil, err
	}
	maxPlayers := cmp.Or(config.MaxPlayersPerView, DefaultMaxPlayersPerView)
	playerBlocks := cmp.Or(config.MaxPlayerBlocks, DefaultMaxObserverBlocks*DefaultMaxViewsPerPlayer)
	if config.AOI.BlockSize > 0 {
		if worst := manager.config.worstObserverBlocks(); worst > int64(playerBlocks)/int64(maxViews) {
			return nil, fmt.Errorf("%w: %d views per player × %d blocks per view exceeds the per-player budget %d",
				ErrInterestBudget, maxViews, worst, playerBlocks)
		}
	}
	in := &Interest{
		batchSize: config.BatchSize, workers: config.Workers,
		aoi: manager, bandLayer: bandLayer, selfVisible: config.SelfVisible == nil || *config.SelfVisible,
		selfLayer:       config.SelfLayer.Normalize(),
		held:            make(map[pair]*hold),
		touched:         make(map[pair]struct{}),
		spatialPlayers:  make(map[int64]map[int64]struct{}),
		followers:       make(map[int64]map[int64]struct{}),
		selfOf:          make(map[int64]int64),
		placed:          make(map[int64]uint64),
		departedPlayers: make(map[int64]struct{}), departedSubjects: make(map[int64]struct{}),
		covers:       make(map[[2]entity.SyncProfile]bool),
		viewInterval: viewInterval, now: time.Now,
		onRefusal: config.OnRefusal,
	}
	in.registry.init(maxViews, maxPlayers, cmp.Or(config.MaxSpatialViews, DefaultMaxSpatialViews))
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
	in.applyMode = config.ApplyMode
	in.wake = config.Manager.WakeSync
	in.applierWake = make(chan struct{}, 1)
	in.applierStop = make(chan struct{})
	in.applierDone = make(chan struct{})
	in.applyGather = applyGatherFor(config.Manager)
	in.syncBudget = syncApplyBudgetFor(config.Manager)
	in.registry.push = in.pushedLocked
	in.cancelQueue = config.Manager.RegisterPolicy(entitysync.PolicyHooks{Apply: in.settleQueued, Pending: in.queuedPending, Stalls: in.drainStalls})
	return in, nil
}

// viewsPerPlayerFor 按应用模式取每个玩家的视野上限，并在启动期挡住同步模式下过多的视野：同步模式的 AOI 计算在
// Flush 的关键路径上，视野越多 Flush 越长，超过上限时直接拒绝构造，而不是运行中 Flush 超过同步周期。
func viewsPerPlayerFor(mode ApplyMode, configured int) (int, error) {
	switch mode {
	case ApplySync:
		if configured > SyncApplyMaxViewsPerPlayer {
			return 0, fmt.Errorf("%w: ApplySync allows at most %d views per player (main view + %d extra), MaxViewsPerPlayer is %d; "+
				"games with more views per player (RTS units, many summons / wards) should set ApplyMode: policy.ApplyAsync",
				ErrInterestConfig, SyncApplyMaxViewsPerPlayer, SyncApplyMaxExtraViews, configured)
		}
		return cmp.Or(configured, SyncApplyMaxViewsPerPlayer), nil
	case ApplyAsync:
		return cmp.Or(configured, DefaultMaxViewsPerPlayer), nil
	}
	return 0, fmt.Errorf("%w: unknown apply mode %d", ErrInterestConfig, mode)
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

// ---- 玩家与主视野（登记侧：只取 registry.mu，记一条命令，见 runOpLocked） ----
//
// 主视野是玩家自带的空间视野（镜头），只挂给玩家自己，在按玩家的入口里用 MainView（0）指代；业务视野的 ID 不能是 0。
// Enter 建立的主视野跟随玩家自身实体（实体移动时一起移动，并排除自己）；Watch 建立的不跟随（只看不被看）。
// 这些入口都不等待空间计算：返回时参数已校验、登记事实已改、命令已进日志，效果由应用方按调用顺序执行，下一次结算
// （同步模式：下一次 Flush；异步模式：应用线程走到之后的 Flush）或显式 Apply 之后可见。

// pushedLocked 是追加一条命令之后的唤醒：异步模式通知应用线程，同步模式提醒 Manager（on_change 下尽快进入政策阶段）。
// 持 registry.mu 调用，不阻塞。
func (in *Interest) pushedLocked() {
	if in.applyMode == ApplyAsync {
		in.signalApplierLocked()
		return
	}
	in.wake()
}

// Enter puts a player in: its entity is seen, its main view sees, and (when
// SelfVisible) it sees itself. playerID 是观察者（会话）编号，entityID 是玩家自身实体；view 为零值时取缺省视野。
// 已登记的玩家再次 Enter（重连）按这次的位置与视野重新登记主视野，要保留原视野就传 ObserverView(playerID) 的结果。
// 主视野计入 MaxViewsPerPlayer。实体换一个新代际：之前还没确认的排队事实确认后作废。
// Its subject must already be registered with the manager; its session must already be open（订阅在结算时说给 Manager）。
func (in *Interest) Enter(playerID, entityID int64, at spatial.Point, view ObserverView) error {
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
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	main, err := r.mainView(playerID)
	if err != nil {
		return err
	}
	main.at, main.sight, main.follow = at, normalized, entityID
	gen := r.newGen()
	r.subjects[entityID] = gen
	r.record(viewOp{kind: opEnter, player: playerID, handle: main.handle, subject: entityID, at: at, sight: normalized, gen: gen})
	return nil
}

// Watch 登记一个只看、不被看的玩家（观战、GM 视角）：主视野不跟随实体（self = 0），也没有 self。
// 它与 Enter（既看又被看）、Show（只被看）并列；视点用 MoveView 移动，用 Unwatch 退出。会话须已打开。
func (in *Interest) Watch(playerID int64, at spatial.Point, view ObserverView) error {
	if playerID == 0 {
		return ErrInterestUnknown
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	if main := r.mains[playerID]; main != nil && main.follow != 0 {
		// 改成只看会让玩家看见自己的实体，而它仍被别人看见；先 Leave 再 Watch。
		return fmt.Errorf("%w: player %d is in with entity %d; Leave first", ErrInterestConfig, playerID, main.follow)
	}
	if !in.aoi.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	normalized, err := in.aoi.config.normalizeView(view)
	if err != nil {
		return err
	}
	main, err := r.mainView(playerID)
	if err != nil {
		return err
	}
	main.at, main.sight, main.follow = at, normalized, 0
	r.record(viewOp{kind: opWatch, player: playerID, handle: main.handle, at: at, sight: normalized})
	return nil
}

// Move 直接移动一个目标（实体 ID），跟随它的空间视野（例如玩家的主视野、单位的视野）一起移动：先移目标再移视野，
// 视野没跟上的目标会从旧位置看世界。不走提交门（事务里的移动用 QueueMove）；目标换一个新代际。
func (in *Interest) Move(entityID int64, to spatial.Point) error {
	if !in.aoi.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	if _, exists := r.subjects[entityID]; !exists {
		return ErrInterestUnknown
	}
	gen := r.newGen()
	r.subjects[entityID] = gen
	r.record(viewOp{kind: opMove, subject: entityID, at: to, gen: gen})
	return nil
}

// MoveView 只移动玩家主视野的视点：Watch 登记的玩家用它移动。主视野跟随实体时，实体下一次移动会把它带回实体位置。
func (in *Interest) MoveView(playerID int64, to spatial.Point) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	main := r.mains[playerID]
	if main == nil {
		return ErrInterestUnknown
	}
	if !in.aoi.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	main.at = to
	r.record(viewOp{kind: opMoveView, handle: main.handle, at: to})
	return nil
}

// SetObserverView 按客户端的 LOD / 视野请求换这个玩家主视野的视野（不走提交门：它不改业务状态，没有回滚）：
//   - 层级必须是配置里有的，否则返回 ErrObserverView；视野大小由服务端夹到允许范围并量化（AOIConfig）。
//   - 归一后与当前视野相同的请求什么也不做，不占限速。
//   - 同一玩家距上次生效不足 ViewChangeInterval 的请求返回 ErrObserverViewRate，计数后丢弃。
//   - 生效后完整重评：视野变大时新进入的目标订阅（下发全量），变小时超出的退订；只换层级时按新分档重发视图，订阅块不变。
//
// 判断都在登记侧完成，返回此后实际生效的视野（拒绝时是当前视野），接入层可以把它告诉客户端；重评由应用方执行，
// 订阅变化在下一次结算说给 Manager。业务空间视野由服务端用 SetSpatialView 改，不限速。
func (in *Interest) SetObserverView(playerID int64, view ObserverView) (ObserverView, error) {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ObserverView{}, errors.New("policy: interest is closed")
	}
	main := r.mains[playerID]
	if main == nil {
		return ObserverView{}, ErrInterestUnknown
	}
	current := main.sight
	next, err := in.aoi.config.normalizeView(view)
	if err != nil {
		metrics.IncCounter(viewRejectedMetric, metrics.Labels{"reason": "lod"}, 1)
		return current, err
	}
	if next == current {
		return current, nil
	}
	now := in.now()
	if last, limited := r.viewChanged[playerID]; limited && now.Sub(last) < in.viewInterval {
		metrics.IncCounter(viewRejectedMetric, metrics.Labels{"reason": "rate"}, 1)
		return current, ErrObserverViewRate
	}
	main.sight = next
	r.viewChanged[playerID] = now
	r.record(viewOp{kind: opMainView, handle: main.handle, sight: next})
	return next, nil
}

// ObserverView 返回玩家主视野当前的视野（登记侧的值：最近一次 Enter / Watch / SetObserverView 给的、归一后的视野）。
func (in *Interest) ObserverView(playerID int64) (ObserverView, bool) {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ObserverView{}, false
	}
	main := r.mains[playerID]
	if main == nil {
		return ObserverView{}, false
	}
	return main.sight, true
}

// Leave takes a player out in both directions: 玩家（playerID）解除全部视野挂接、撤掉主视野与 self；
// 实体（entityID）作为目标退出空间索引，并从所有关系视野的成员里去掉。业务视野本身保留，玩家重新进场后由业务重新挂接。
// 玩家没有主视野时返回 ErrInterestUnknown（实体那一半照做）；实体没登记时同样返回 ErrInterestUnknown。
func (in *Interest) Leave(playerID, entityID int64) error {
	return in.leave(playerID, entityID)
}

// Unwatch 让 Watch 登记的玩家退出；它没有目标的一半。
func (in *Interest) Unwatch(playerID int64) error {
	return in.leave(playerID, 0)
}

func (in *Interest) leave(playerID, entityID int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	// 登记表里的解除（业务视野的挂接、关系成员）先进日志，再撤主视野与目标。
	main := r.leave(playerID, entityID)
	delete(r.viewChanged, playerID)
	op := viewOp{kind: opLeave, player: playerID}
	if main != nil {
		op.handle = main.handle
	}
	var err error
	if entityID != 0 {
		if _, known := r.subjects[entityID]; known {
			delete(r.subjects, entityID)
			op.subject = entityID
		} else {
			err = ErrInterestUnknown
		}
	}
	r.record(op)
	if main == nil {
		return ErrInterestUnknown
	}
	return err
}

// Show / Hide are for things that are seen but do not see by themselves: a
// monster, a dropped item. 参数都是实体 ID；移动用 Move。Show 时已有空间视野跟随它的，视野跳到它的位置。
// 目标换一个新代际。
func (in *Interest) Show(id int64, at spatial.Point) error {
	if id == 0 {
		return ErrInterestUnknown
	}
	if !in.aoi.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	gen := r.newGen()
	r.subjects[id] = gen
	r.record(viewOp{kind: opShow, subject: id, at: at, gen: gen})
	return nil
}

// Hide 让目标退出空间索引，并从所有关系视野的成员里去掉；跟随它的空间视野留在原地（跟随关系保留）。
// 之前还没确认的排队事实确认后作废。目标没登记时只做关系视野那一半，返回 ErrInterestUnknown。
func (in *Interest) Hide(id int64) error {
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("policy: interest is closed")
	}
	r.forgetLocked(id)
	if _, known := r.subjects[id]; !known {
		return ErrInterestUnknown
	}
	delete(r.subjects, id)
	r.record(viewOp{kind: opHide, subject: id})
	return nil
}

// Resubscribe says every pair this observer holds to the manager again on the
// next settle. It is for an observer whose session the manager dropped on its
// own — a transport failure it reported through ManagerConfig.SessionLost —
// while the observer stays in the policy (it still has a live connection) and
// its session has been opened again: the manager forgot the subscriptions, the
// policy did not, and without this the reopened session would receive nothing
// until the pairs changed. Pairs other observers hold on this subject are
// untouched; they never left.
//
// The re-said pairs go through the retry list, so a manager that still
// refuses one (the session is not open yet) is asked again on every settle.
//
// 实体被框架退回 remove（卸载后重载不了）之后重新登记的情形不需要调用它：Manager 把被撤销的订阅
// 交还 Interest，由 resubmit 自动重新提交。observer 是玩家 ID。它只登记一条命令，按调用顺序生效。
func (in *Interest) Resubscribe(observer int64) {
	if in == nil {
		return
	}
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.record(viewOp{kind: opResubscribe, player: observer})
}

// ---- 应用侧的命令实现（持 mu，在 AOI 批次写阶段之外） ----

// placeSubjectLocked 登记或移动目标并记下它的代际，把跟随它的空间视野移到同一位置。
func (in *Interest) placeSubjectLocked(id int64, at spatial.Point, gen uint64) {
	if err := in.aoi.AddSubject(id, at); err != nil {
		in.applyErr = errors.Join(in.applyErr, fmt.Errorf("policy: place subject %d: %w", id, err))
		return
	}
	in.placed[id] = gen
	in.moveFollowersLocked(id, at)
}

// placeMainLocked 放置（或重新放置）主视野，并把它挂给玩家自己。
func (in *Interest) placeMainLocked(playerID, handle, follow int64, at spatial.Point, sight ObserverView) {
	if in.spatialPlayers[handle] == nil {
		in.spatialPlayers[handle] = map[int64]struct{}{playerID: {}}
	}
	in.placeViewLocked(handle, follow, at, sight)
}

// leaveLocked 撤掉玩家的 self 与主视野（handle 为 0 时没有），subject 非 0 时让目标退出空间索引。
func (in *Interest) leaveLocked(playerID, handle, subject int64) {
	in.departedPlayers[playerID] = struct{}{}
	if self, ok := in.selfOf[playerID]; ok {
		delete(in.selfOf, playerID)
		in.contribute(pair{observer: playerID, subject: self}, contribution{}, entity.SyncProfile{}, false)
	}
	if handle != 0 {
		in.removeSpatialLocked(handle)
	}
	if subject != 0 {
		in.removeSubjectLocked(subject)
	}
}

// removeSubjectLocked 让目标退出空间索引（Leave / Hide）。它之前还没确认的排队事实随 placed 的删除作废。
func (in *Interest) removeSubjectLocked(id int64) {
	in.departedSubjects[id] = struct{}{}
	delete(in.placed, id)
	_ = in.aoi.RemoveSubject(id)
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

func (in *Interest) resubscribeLocked(observer int64) {
	for key, current := range in.held {
		if key.observer != observer || !current.subscribed {
			continue
		}
		current.subscribed = false
		in.retry = append(in.retry, key)
	}
}

// resubmit 是 Interest 的重新提交入口：卸载后重载不了，框架退回 remove 撤销了本 Interest
// 的订阅，同 ID 实体又重新登记后，Manager 在 Flush 的政策阶段把这些订阅交还这里。与 Resubscribe 同一种修补：
// Manager 忘了，Interest 没忘。仍持有的 pair 按 Interest 自己的规则重新订阅（层级合并、ViewSets 校验、
// Manager 的优先级与字段白名单）；缺席期间已释放的 pair（视野走远、解除挂接、Hide）不在 held 里，不恢复。
// 被拒绝的 pair 进入重试表，下一次结算以 Retry 报告并继续重说。
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
	var refusals []Refusal // 已记入重试表，由下一次结算报告
	for key, current := range in.held {
		sessions, retractedSubject := wanted[key.subject]
		if !retractedSubject || !current.subscribed || !slices.Contains(sessions, sessionOf(key.observer)) {
			continue
		}
		in.subscribe(key, current, true, &refusals)
	}
}

// ---- 诊断读（取应用侧锁，先执行已登记的条目再读；不在 Nest 快池上调用） ----

// Visible lists what a player's main view currently sees through distance.
func (in *Interest) Visible(playerID int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.drainLogLocked()
	main := in.registry.mainOf(playerID)
	if main == nil {
		return nil
	}
	return in.aoi.Visible(main.handle)
}

// Seen 返回玩家经任一视野（含 self）持有的全部目标（升序），即本 Interest 要它订阅的集合。先执行已登记的条目并
// 归并 AOI 的变化；遍历全部 pair，供诊断与压测核对，不要放在热路径上。
func (in *Interest) Seen(playerID int64) []int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.drainLogLocked()
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

// Apply 在调用方线程执行全部已登记、可执行的条目（未确认的排队事实除外），把视野与 AOI 的变化合并进 held，
// 再说给 Manager：subscribes for pairs a first view claimed, unsubscribes for pairs the last view dropped,
// re-subscribes for pairs whose layers changed. Subscribes the manager refuses come back as Refusals and are
// said again on the next settle, for as long as the pair is held — a refusal is not a verdict on the pair.
//
// 正常运行不需要调用它：Manager Flush 的政策阶段（同步模式）或应用线程 + 政策阶段（异步模式）会做同样的事。
// 它给测试、工具与离线驱动用；它取应用侧锁、会等正在进行的一步，不能在 Nest 快池上调用。
func (in *Interest) Apply() []Refusal {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.drainLogLocked()
	return in.applyLocked()
}

// applyLocked 是结算：把 AOI 事件与关系变更归并进 held 并说给 Manager，然后释放已应用事实的发布屏障。调用方持有 mu。
func (in *Interest) applyLocked() []Refusal {
	if in.closed {
		return nil
	}
	var refusals []Refusal
	// Take the retry list first: a refusal during this settle goes on the list
	// for the NEXT one, not for a second attempt a few lines further down.
	queued := in.retry
	in.retry = nil
	in.unsettled = false
	in.unsettledUnits = 0
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
	clear(in.departedPlayers)
	clear(in.departedSubjects)
	// Retries after the events, so that a pair a view already spoke
	// for in this settle is not said twice.
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
	in.releaseAppliedLocked()
	return refusals
}

// contribute 记录一个视野对 pair 的持有（enter）或释放，不立即发布；pair 记进 touched，由下一次结算处理。
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
	switch {
	case !current.subscribed:
		in.subscribe(key, current, false, refusals)
	case in.departedLocked(key):
		// 撤除后又在同一次结算里被持有：让 Manager 看到一次离开和一次进入（目标可能已重新登记、会话可能已重开）。
		_ = in.subscriptions.Unsubscribe(sessionOf(key.observer), key.subject)
		in.subscribe(key, current, false, refusals)
	default:
		in.reband(key, current, refusals)
	}
}

func (in *Interest) departedLocked(key pair) bool {
	if _, gone := in.departedPlayers[key.observer]; gone {
		return true
	}
	_, gone := in.departedSubjects[key.subject]
	return gone
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
	// 先关登记侧（不再有新条目、不再启动应用线程），再停应用线程并等它退出——它要拿 mu，所以不能持 mu 等。
	r := &in.registry
	r.mu.Lock()
	alreadyClosed := r.closed
	r.closed = true
	started := in.applierStarted
	r.mu.Unlock()
	if started && !alreadyClosed {
		close(in.applierStop)
		<-in.applierDone
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.closed = true
	for _, fact := range in.applied {
		fact.finish()
	}
	in.applied = nil
	r.mu.Lock()
	for _, entry := range r.log {
		if entry.op == nil {
			entry.fact.finish()
		}
	}
	r.log = nil
	r.facts = 0
	clear(r.subjects)
	r.mu.Unlock()
	for key := range in.held {
		_ = in.subscriptions.Unsubscribe(sessionOf(key.observer), key.subject)
	}
	in.held = nil
	in.touched = nil
	in.retry = nil
	in.refused = nil
}
