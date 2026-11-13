package cellworld

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ErrNodeConfig 表示 NodeConfig 不合法。
var ErrNodeConfig = errors.New("cellworld: invalid node config")

// NodeConfig 配置一个进程的 cellworld 端点。零值字段取 withDefaults 的缺省值。
//
// 一个进程一个 Node：Cell 侧（托管一个格：Cell 非空）与 Base 侧（托管玩家：PlayerEntity 非空）可以同时启用。
type NodeConfig struct {
	// World 是主题命名空间，只允许字母、数字、'-'、'_'；SID 是本进程 sid（收件主题按它区分）。
	World string
	SID   int32
	// Cell 必须使用共享授权记录和 App 单实例锁的本次启动令牌；Base-only 不需要。
	Owners      *OwnerRegistry
	Incarnation string
	// CheckLease 是 App SingletonLease.Check；只检查本地窗口，不做 I/O。
	// Cell 必填。过期后这个 Node 不再执行、提交或发出旧写者的业务结果。
	CheckLease func() error
	// OwnershipWorkers / OwnershipQueue / OwnershipTimeout 只用于入场、交接、退出的存储 I/O。
	// 普通心跳、移动和伤害不经过这个池。零值为 8 / 1024 / 2s。
	OwnershipWorkers, OwnershipQueue int
	OwnershipTimeout                 time.Duration
	// Grid 是静态分区，所有进程一致。
	Grid      *Grid
	Transport Transport
	// Nest 是本进程的 Nest 客户端（传输回调与出站队列把消息投成 Nest 请求）。handler 要在引擎启动前装到引擎实例上：
	// Node 先于引擎建好时调 RegisterHandlers；Node 在引擎启动之后才建（正式进程：服务 Init 里）时，服务先在引擎启动前
	// Register 一份 Handlers，这里交给 Handlers，NewNode 把它接上本 Node。
	Nest     nest.Client
	Handlers *Handlers
	// Destroy 销毁一个实体（正式路径：EntityManager.Destroy），在 Node 的出站 goroutine 上调用，不在业务 worker 上。
	Destroy func(context.Context, fentity.IThreadSafeEntity) error

	// ---- Cell 侧（Cell 为 nil 时本进程不托管格）----

	// Cell 是本进程负责的格（必须是 Grid 里映射到 SID 的那一格）；Scene 是持有 Region 的 Scene 实体 ID。
	Cell  *CellID
	Scene int64
	// CreateActor 在当前 Nest handler 内新建一个角色 / NPC 实体（EntityManager.Create：handler 内走 CreateInScope，
	// 由事务捕获，回滚即撤销发布）。kind 区分玩家的 SceneActor 与 NPC 的 Monster。
	CreateActor func(id int64, kind EntityKind) (Actor, error)
	// Period 是角色与区域心跳的周期，必须是 heartbeat.Periods 的档位；零值 50ms。
	Period time.Duration
	// ViewRadius 是 witness 半径 R：必须为正且 R + Hysteresis ≤ GhostBand。AttackRange 零值等于 R，不能超过 R。
	ViewRadius, AttackRange int64
	// IndexBlock 是 witness 现建索引的块边长；零值等于 ViewRadius。
	IndexBlock int64
	// MaxViewEntities：一次 witness 的对象上限（按距离取最近的）；零值 256。MaxReals：本格 Real 上限；零值 50000。
	MaxViewEntities, MaxReals int
	// MaxSpeed 是玩家输入速度每个分量的上限；零值 2000。
	MaxSpeed int64
	// GhostChunk：一条 Ghost 消息最多的更新 + 删除条数；零值 256。
	GhostChunk int
	// OfferRetry / HandoffAbort / RejectBackoff：交接重传间隔、放弃上限、被拒后的退避；零值 100ms / 3s / 500ms。
	OfferRetry, HandoffAbort, RejectBackoff time.Duration
	// GhostTTL：Ghost 超过它没更新就删除；零值 1s。SeenTTL：交接 epoch 记录保留时间，必须长于 HandoffAbort；零值 30s。
	GhostTTL, SeenTTL time.Duration
	// AttackRetry / AttackDeadline：跨格伤害重发间隔与结论期限；零值 100ms / 2s。DedupWindow 必须长于 AttackDeadline；零值 10s。
	AttackRetry, AttackDeadline, DedupWindow time.Duration
	// MaxAppliedHits：单个目标去重窗口内最多记录的伤害数，满了拒绝（ReasonBusy）；零值 128。
	MaxAppliedHits int
	// LeaseTimeout：玩家 SceneActor 多久没收到 Base 的输入 / 续租就离场；零值 3s，必须长于 LeasePeriod。
	LeaseTimeout time.Duration
	// WritebackEvery：玩家 SceneActor 定时回写 Player 的间隔（战斗中每 N 秒）；零值 5s。
	WritebackEvery time.Duration
	// IdlePeriod：附近没有玩家的 NPC 的心跳档位（D1，见 tier.go）；必须是长于 Period 的 heartbeat 档位。零值不降档
	// （NPC 恒为 Period）。
	IdlePeriod time.Duration
	// WakeRadius：NPC 半径 W 内有玩家（本格玩家 Real 或邻格玩家 Ghost）就算“有人在附近”；零值等于 ViewRadius。
	// 必须 ViewRadius ≤ W 且 W + Hysteresis ≤ GhostBand：W 内的邻格玩家一定是本格的 Ghost，本格才看得见他。
	WakeRadius int64
	// IdleAfter：NPC 连续这么久附近没有玩家才降档（滞后，玩家在 W 边上来回时不抖动）；零值 2s。升档不等待。
	IdleAfter time.Duration
	// Surface 是带高度的地面钩子（可编辑地形 P3 跨格移动，见 Surface 与 integrate）：非 nil 时角色每拍开始调用一次，
	// 取这一拍的地面；返回 nil 表示这一拍没有高度规则。为 nil 或返回 nil 时积分与不设逐位相同。用函数而不是值：
	// 地面可能在 Node 建好之后才装上。返回值不能是带类型的 nil（*T(nil) 装进接口不等于 nil）。
	Surface func() Surface

	// ---- Base 侧（PlayerEntity 为 nil 时本进程不托管玩家）----

	// PlayerEntity 把角色（SceneActor）ID 换成本进程 Player 实体 ID。
	PlayerEntity func(actorID int64) int64
	// WatchPeriod：Base 心跳（看门狗 + 续租）周期，必须是 heartbeat 档位；零值 100ms。
	WatchPeriod time.Duration
	// SilenceTimeout：多久没收到 witness 就按最后位置发入场查询（新建仍须确认共享授权）；零值 1s。LostGrace：收到交接放弃后等多久再重进；零值 500ms。
	SilenceTimeout, LostGrace time.Duration
	// LeasePeriod：Base 向 owner 格续租（重发最新输入）的间隔，也是待确认面板的重发间隔；零值 500ms。
	LeasePeriod time.Duration
	// PanelDeadline：面板变化多久没有确认就报一次“结果未知”（Hooks.PanelResult），必须长于 LeasePeriod；零值 2s。
	PanelDeadline time.Duration
	// Mirrors / Interest / Session / MirrorKind：把 witness 喂给客户端（Sync mirror）。Mirrors 为 nil 时只记 owner，
	// 不推客户端（测试与不需要客户端的进程）。Session 把角色 ID 换成 Interest 的玩家（会话）ID；MirrorKind 给出对象
	// 在帧上的 namespace 与 kind。
	Mirrors    *policy.Mirrors[EntityState]
	Interest   *policy.Interest
	Session    func(actorID int64) int64
	MirrorKind func(EntityState) (string, uint32)
	// View 是玩家在 Interest 里的主视野（零值取缺省）。
	View policy.ObserverView

	// ---- 队伍面板（Base 侧；GroupView 为 nil 时不启用，group.go）----

	// GroupView 给出玩家（Player 实体 ID）的组关系视野 ID：成员是其他队员的 SceneActor，玩家挂接它，不论距离都看见队员；
	// 在空间视野里的队员与它是同一个 subject，Interest 合并成一份订阅，不重复推。每个玩家一个，不能与本进程别的视野相同。
	// 启用时 Mirrors 必填。GroupLayer 是这个视野给的同步层级（零值是缺省视图）。
	GroupView  func(playerEntity int64) policy.ViewID
	GroupLayer fentity.SyncProfile
	// GroupEvery：本进程每名队员的快照发给其他队员的 Base 的间隔，零值 1s，不短于 WatchPeriod。GroupHold：收到的快照
	// 保留多久，超过就从面板移除；零值 3 × GroupEvery（维护者：3 个周期没更新视为过期），必须长于 GroupEvery。
	GroupEvery, GroupHold time.Duration
	// GroupResync 从权威重读玩家的成员表（game 的一笔事务，demo 是 [Team, Player]）：在出站 goroutine 上调用，只做准入、
	// 不等结论。缓存的版本为 0（Base 重启、Player 重新加载）时下一个 GroupEvery 就调，之后每 GroupResyncEvery 一次，兜住
	// 丢失的成员变化通知。nil 表示不重读。GroupResyncEvery 零值 10s，必须长于 GroupEvery。
	GroupResync      func(ctx context.Context, group, playerEntity int64) error
	GroupResyncEvery time.Duration

	// ---- 扩展消息（v7，ext.go）----

	// Ext 是本进程收扩展消息的处理器，键是命名空间（只允许字母、数字、'-'、'_'，至多 64 个字符）。别的进程经 SendExt
	// 发来、命名空间在这里的消息交给对应的处理器；没有处理器的计 ExtDrops 丢弃。处理器在传输回调上调用（与 Node 自己的
	// 收件同一个准入门：CloseIngress 之后不再调用），必须快速返回、不能阻塞、不能做 I/O——要做重活就投给自己的
	// goroutine。body 只在调用期间有效。
	Ext map[string]ExtHandler

	// OutboxSize：出站队列容量（提交后的发送、报告、销毁）；满了丢弃并计数。零值 65536。
	OutboxSize int
	// 网络控制消息与完整状态独立排队；销毁 I/O 不与网络或本地 Nest 报告串行。
	// ControlQueue / DestroyWorkers / DestroyQueue 零值为 4096 / 4 / 1024。
	ControlQueue, DestroyWorkers, DestroyQueue int
	// 传输明确报错时最多尝试 3 次，每次间隔 20ms；不替代协议级重传。
	PublishAttempts int
	PublishRetry    time.Duration
	// 内部增量缓存两端各最多 ReplicaCacheSize 项，默认 131072；每 250ms 全量修复。
	// WitnessBatch 按同一 Base 合批，默认 16 名观察者；只按完整对象/视野分包。
	ReplicaCacheSize, WitnessBatch int
	// SettleDeath 只在获奖 Player 的 strict handler 内调用；允许 nil（记死亡回执但不发奖）。
	// 接入方在此修改同一个 Player 的奖励 DAO，不能做 I/O 或另开事务。
	SettleDeath                 func(BasePlayer, DeathFact) error
	MaxDeathReceipts            int
	DeathRetry, DeathScanPeriod time.Duration
	ReplicaRefresh              time.Duration
	// MaxWireBytes 是单条消息硬上限；超限拒绝，不按字节截断。默认 512KiB。
	MaxWireBytes int
	// ReportBatch：一次投给 Scene 的角色报告上限；零值 512。
	ReportBatch int
	Hooks       Hooks
}

// Hooks 是测试与测量的观察点，都在提交之后（AfterCommit 或出站 goroutine）调用，必须快速返回、不能阻塞。
type Hooks struct {
	// RealChanged：本格取得（acquired）或失去某实体 Real 身份，事务已提交。
	RealChanged func(cell CellID, id int64, epoch uint64, acquired bool)
	// HandoffDone：旧格得到交接结论（accepted=false 是被拒后恢复）；elapsed 从冻结起算。
	HandoffDone func(cell CellID, id int64, epoch uint64, accepted bool, elapsed time.Duration)
	// HandoffAborted：旧格放弃交接（超时，删除）。
	HandoffAborted func(cell CellID, id int64, epoch uint64)
	// GhostApplied：应用一条 Ghost 消息；latency 是发送方打时间戳到这里的系统时间差。
	GhostApplied func(cell CellID, latency time.Duration, entries int)
	// DamageApplied：目标 Real 上真正扣血（不含重复请求）。
	DamageApplied func(cell CellID, target int64, hit HitID, hp int32)
	// TickDone：一次区域心跳结束；elapsed 是这拍 handler 的耗时。
	TickDone func(cell CellID, elapsed time.Duration, reals, ghosts int)
	// Witness：Base 接受了一份 witness（player 是角色 ID）。
	Witness func(View)
	// AttackResult：Base 收到攻击结论。
	AttackResult func(AttackResult)
	// Writeback：Base 接受了一次回写（版本更新的快照）。
	Writeback func(actorID int64, state EntityState)
	// OwnerChanged：Base 看到玩家换了 owner 格。
	OwnerChanged func(actorID int64, sid int32, epoch uint64)
	// PanelApplied：Real 上真正应用了一版更新的面板（不含重复与过期的命令）；hp 是按新上限夹过之后的当前 HP。
	PanelApplied func(cell CellID, id int64, panel Panel, hp int32)
	// PanelResult：Base 得到面板结论（Applied 或 Unknown）。
	PanelResult func(PanelResult)
}

func (cfg NodeConfig) withDefaults() NodeConfig {
	setDur := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	setInt := func(n *int, v int) {
		if *n == 0 {
			*n = v
		}
	}
	setDur(&cfg.Period, heartbeat.Periods[0])
	if cfg.AttackRange == 0 {
		cfg.AttackRange = cfg.ViewRadius
	}
	if cfg.IndexBlock == 0 {
		cfg.IndexBlock = cfg.ViewRadius
	}
	setInt(&cfg.MaxViewEntities, 256)
	setInt(&cfg.MaxReals, 50000)
	if cfg.MaxSpeed == 0 {
		cfg.MaxSpeed = 2000
	}
	setInt(&cfg.GhostChunk, 256)
	setDur(&cfg.OfferRetry, 100*time.Millisecond)
	setDur(&cfg.HandoffAbort, 3*time.Second)
	setDur(&cfg.RejectBackoff, 500*time.Millisecond)
	setDur(&cfg.GhostTTL, time.Second)
	setDur(&cfg.SeenTTL, 30*time.Second)
	setDur(&cfg.AttackRetry, 100*time.Millisecond)
	setDur(&cfg.AttackDeadline, 2*time.Second)
	setDur(&cfg.DedupWindow, 10*time.Second)
	setInt(&cfg.MaxAppliedHits, 128)
	setDur(&cfg.LeaseTimeout, 3*time.Second)
	setDur(&cfg.WritebackEvery, 5*time.Second)
	if cfg.WakeRadius == 0 {
		cfg.WakeRadius = cfg.ViewRadius
	}
	setDur(&cfg.IdleAfter, 2*time.Second)
	setDur(&cfg.WatchPeriod, 100*time.Millisecond)
	setDur(&cfg.SilenceTimeout, time.Second)
	setDur(&cfg.LostGrace, 500*time.Millisecond)
	setDur(&cfg.LeasePeriod, 500*time.Millisecond)
	setDur(&cfg.PanelDeadline, 2*time.Second)
	setDur(&cfg.GroupEvery, time.Second)
	setDur(&cfg.GroupHold, 3*cfg.GroupEvery)
	setDur(&cfg.GroupResyncEvery, 10*time.Second)
	setInt(&cfg.OutboxSize, 65536)
	setInt(&cfg.ReplicaCacheSize, 131072)
	setInt(&cfg.MaxDeathReceipts, 20000)
	setDur(&cfg.DeathRetry, 500*time.Millisecond)
	setDur(&cfg.DeathScanPeriod, 100*time.Millisecond)
	setInt(&cfg.WitnessBatch, 16)
	setDur(&cfg.ReplicaRefresh, 250*time.Millisecond)
	setInt(&cfg.MaxWireBytes, 512*1024)
	setInt(&cfg.ControlQueue, 4096)
	setInt(&cfg.DestroyWorkers, 4)
	setInt(&cfg.DestroyQueue, 1024)
	setInt(&cfg.PublishAttempts, 3)
	setDur(&cfg.PublishRetry, 20*time.Millisecond)
	setInt(&cfg.ReportBatch, 512)
	setInt(&cfg.OwnershipWorkers, 8)
	setInt(&cfg.OwnershipQueue, 1024)
	setDur(&cfg.OwnershipTimeout, 2*time.Second)
	return cfg
}

func (cfg NodeConfig) validate() error {
	if cfg.MaxDeathReceipts < 1 || cfg.DeathRetry <= 0 || cfg.DeathScanPeriod <= 0 {
		return fmt.Errorf("%w: invalid death settlement limits", ErrNodeConfig)
	}
	if cfg.ReplicaCacheSize < cfg.MaxViewEntities || cfg.WitnessBatch < 1 || cfg.WitnessBatch > 256 || cfg.ReplicaRefresh <= 0 || cfg.MaxWireBytes < 1024 {
		return fmt.Errorf("%w: invalid replication bounds", ErrNodeConfig)
	}
	if cfg.ControlQueue < 1 || cfg.DestroyWorkers < 1 || cfg.DestroyQueue < 1 || cfg.PublishAttempts < 1 || cfg.PublishAttempts > 16 || cfg.PublishRetry <= 0 {
		return fmt.Errorf("%w: invalid outbound limits", ErrNodeConfig)
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrNodeConfig}, args...)...)
	}
	switch {
	case !validWorld(cfg.World):
		return bad("world %q", cfg.World)
	case cfg.SID == 0 || cfg.Grid == nil || cfg.Transport == nil || cfg.Nest == nil || cfg.Destroy == nil:
		return bad("sid, grid, transport, nest and destroy are required")
	case cfg.Cell == nil && cfg.PlayerEntity == nil:
		return bad("node hosts neither a cell nor players")
	case cfg.OutboxSize < 0 || cfg.ReportBatch < 0:
		return bad("negative queue size")
	}
	for space, handler := range cfg.Ext {
		if !validWorld(space) || handler == nil {
			return bad("ext space %q needs a name of letters, digits, '-' or '_' and a handler", space)
		}
	}
	if cfg.Cell != nil {
		switch {
		case !cfg.Grid.Contains(*cfg.Cell) || cfg.Grid.SID(*cfg.Cell) != cfg.SID:
			return bad("cell %v is not mapped to sid %d", *cfg.Cell, cfg.SID)
		case cfg.Owners == nil || cfg.Incarnation == "" || cfg.CheckLease == nil:
			return bad("a cell needs shared ownership, process incarnation and local lease guard")
		case cfg.OwnershipWorkers < 1 || cfg.OwnershipQueue < 1 || cfg.OwnershipTimeout <= 0:
			return bad("invalid ownership I/O limits")
		case cfg.Scene == 0 || cfg.CreateActor == nil:
			return bad("a cell needs its scene entity and CreateActor")
		case !slices.Contains(heartbeat.Periods[:], cfg.Period):
			return bad("period %v is not a heartbeat period", cfg.Period)
		case cfg.ViewRadius <= 0 || cfg.ViewRadius+cfg.Grid.Hysteresis() > cfg.Grid.GhostBand():
			// Real 最多越出本格 H 才交接，半径 R 内的邻区实体离本格至多 R + H，要都有 Ghost 就需要 R + H ≤ G。
			return bad("view radius %d plus hysteresis %d must be in (0, ghost band %d]", cfg.ViewRadius, cfg.Grid.Hysteresis(), cfg.Grid.GhostBand())
		case cfg.AttackRange < 0 || cfg.AttackRange > cfg.ViewRadius:
			return bad("attack range %d must be in [0, view radius %d]", cfg.AttackRange, cfg.ViewRadius)
		case cfg.IndexBlock <= 0 || cfg.MaxViewEntities < 0 || cfg.MaxReals < 0 || cfg.MaxSpeed < 0 || cfg.GhostChunk <= 0 || cfg.MaxAppliedHits < 0:
			return bad("negative size")
		case cfg.OfferRetry <= 0 || cfg.HandoffAbort <= cfg.OfferRetry:
			return bad("handoff abort %v must exceed offer retry %v", cfg.HandoffAbort, cfg.OfferRetry)
		case cfg.SeenTTL <= cfg.HandoffAbort:
			return bad("seen ttl %v must exceed handoff abort %v", cfg.SeenTTL, cfg.HandoffAbort)
		case cfg.AttackRetry <= 0 || cfg.AttackDeadline <= cfg.AttackRetry:
			return bad("attack deadline %v must exceed attack retry %v", cfg.AttackDeadline, cfg.AttackRetry)
		case cfg.DedupWindow <= cfg.AttackDeadline:
			// 去重窗口短于重试期限时，过期的去重记录之后到达的重试会被再结算一次。
			return bad("dedup window %v must exceed attack deadline %v", cfg.DedupWindow, cfg.AttackDeadline)
		case cfg.GhostTTL <= cfg.Period || cfg.RejectBackoff <= 0:
			return bad("ghost ttl %v must exceed period %v", cfg.GhostTTL, cfg.Period)
		case cfg.LeaseTimeout <= cfg.LeasePeriod:
			return bad("lease timeout %v must exceed lease period %v", cfg.LeaseTimeout, cfg.LeasePeriod)
		case cfg.IdlePeriod != 0 && (!slices.Contains(heartbeat.Periods[:], cfg.IdlePeriod) || cfg.IdlePeriod <= cfg.Period || cfg.IdlePeriod%cfg.Period != 0):
			// 慢档按 Period 分步积分（advance），所以必须是 Period 的整数倍。
			return bad("idle period %v must be a heartbeat period that is a multiple of period %v", cfg.IdlePeriod, cfg.Period)
		case cfg.WakeRadius < cfg.ViewRadius || cfg.WakeRadius+cfg.Grid.Hysteresis() > cfg.Grid.GhostBand():
			return bad("wake radius %d must be in [view radius %d, ghost band %d - hysteresis %d]", cfg.WakeRadius, cfg.ViewRadius, cfg.Grid.GhostBand(), cfg.Grid.Hysteresis())
		case cfg.IdleAfter <= 0:
			return bad("idle after %v must be positive", cfg.IdleAfter)
		}
	}
	if cfg.PlayerEntity != nil {
		if _, isCell := cfg.Grid.CellOf(cfg.SID); isCell && cfg.Cell == nil {
			return bad("sid %d is a cell sid but the node hosts no cell", cfg.SID)
		}
		switch {
		case !slices.Contains(heartbeat.Periods[:], cfg.WatchPeriod):
			return bad("watch period %v is not a heartbeat period", cfg.WatchPeriod)
		case cfg.SilenceTimeout <= cfg.WatchPeriod || cfg.LostGrace <= 0 || cfg.LeasePeriod <= 0:
			return bad("silence timeout must exceed watch period")
		case cfg.PanelDeadline <= cfg.LeasePeriod:
			return bad("panel deadline %v must exceed lease period %v", cfg.PanelDeadline, cfg.LeasePeriod)
		case cfg.Mirrors != nil && (cfg.Interest == nil || cfg.Session == nil || cfg.MirrorKind == nil):
			return bad("mirrors need interest, session and mirror kind")
		case cfg.GroupView != nil && cfg.Mirrors == nil:
			return bad("the team panel (group view) needs mirrors")
		case cfg.GroupView != nil && (cfg.GroupEvery < cfg.WatchPeriod || cfg.GroupHold <= cfg.GroupEvery || cfg.GroupResyncEvery <= cfg.GroupEvery):
			return bad("group every %v must be at least watch period %v, and group hold %v and group resync every %v must exceed it",
				cfg.GroupEvery, cfg.WatchPeriod, cfg.GroupHold, cfg.GroupResyncEvery)
		}
	}
	return nil
}

// Node 连接 Cell / Base 的 Nest 事务、传输和收尾。玩法状态在 Scene 的 Region、
// Actor / Player 的 DAO 上；Node 仅持有有界队列与生命周期记录。
// 控制消息、完整状态、本地 Nest 报告、销毁和所有权 I/O 各自执行，提交回调只做准入。
type Node struct {
	cfg             NodeConfig
	cell            CellID
	hostsCell       bool
	rect            spatial.Rect
	indexBounds     spatial.Rect
	out             chan job
	network         networkQueue
	sentReplica     replicaCache
	receivedReplica replicaCache
	controls        chan queuedSend
	destroyQueue    chan job
	destroyMu       sync.Mutex
	destroyBusy     map[fentity.IThreadSafeEntity]bool
	outputPending   atomic.Int64
	lifecycle       struct {
		sync.Mutex
		stopping     bool
		transactions map[*nest.RollbackTx]bool
	}
	deathAcks    chan receivedDeathAck
	deathPending atomic.Int64
	ownerQueue   chan ownerWork
	ownerMu      sync.Mutex
	ownerBusy    map[int64]bool
	ready        chan struct{}
	running      atomic.Bool
	lastExpire   atomic.Int64
	// ingress 是收件的准入门（CloseIngress）：closed 之后回调直接丢弃；busy 是正在执行的回调数，closed 且归零时关 idle。
	ingress struct {
		sync.Mutex
		closed      bool
		busy        int
		idle        chan struct{}
		unsubscribe func() error
	}
	stats    nodeCounters
	handlers *Handlers
}

// NewNode 校验配置并构造 Node。
func NewNode(cfg NodeConfig) (*Node, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	n := &Node{cfg: cfg, out: make(chan job, cfg.OutboxSize), ready: make(chan struct{}), handlers: cfg.Handlers}
	n.network.init()
	n.sentReplica.init()
	n.receivedReplica.init()
	n.controls = make(chan queuedSend, cfg.ControlQueue)
	n.destroyQueue = make(chan job, cfg.DestroyQueue)
	n.destroyBusy = make(map[fentity.IThreadSafeEntity]bool)
	if n.handlers == nil {
		n.handlers = NewHandlers()
	}
	if err := n.handlers.bind(n); err != nil {
		return nil, err
	}
	if cfg.Cell != nil {
		n.ownerQueue = make(chan ownerWork, cfg.OwnershipQueue)
		n.deathAcks = make(chan receivedDeathAck, cfg.OwnershipQueue)
		n.ownerBusy = make(map[int64]bool)
		n.cell, n.hostsCell = *cfg.Cell, true
		n.rect = cfg.Grid.Rect(n.cell)
		n.indexBounds = expand(n.rect, cfg.Grid.GhostBand())
	}
	return n, nil
}

// Config 返回补过缺省值的配置。
func (n *Node) Config() NodeConfig { return n.cfg }

// Ready 在 Run 完成订阅后关闭。
func (n *Node) Ready() <-chan struct{} { return n.ready }

// Run 订阅本进程收件主题、运行出站 goroutine，直到 ctx 结束；返回前退订。只能 Run 一次。
func (n *Node) Run(ctx context.Context) error {
	if !n.running.CompareAndSwap(false, true) {
		return errors.New("cellworld: node already ran")
	}
	n.ingress.Lock()
	if !n.ingress.closed {
		unsubscribe, err := n.cfg.Transport.Subscribe(Subject(n.cfg.World, n.cfg.SID), n.receive)
		if err != nil {
			n.ingress.Unlock()
			return fmt.Errorf("cellworld: subscribe sid %d: %w", n.cfg.SID, err)
		}
		var once sync.Once
		n.ingress.unsubscribe = func() error {
			var err error
			once.Do(func() { err = unsubscribe() })
			return err
		}
	}
	stopReceiving := n.ingress.unsubscribe
	n.ingress.Unlock()
	if stopReceiving != nil {
		defer func() { _ = stopReceiving() }()
	}
	var workers sync.WaitGroup
	if n.hostsCell {
		workers.Go(func() { n.runDeaths(ctx) })
		for range n.cfg.OwnershipWorkers {
			workers.Go(func() { n.runOwners(ctx) })
		}
	}
	workers.Go(func() { n.runControls(ctx) })
	workers.Go(func() { n.runStates(ctx) })
	for range n.cfg.DestroyWorkers {
		workers.Go(func() { n.runDestroys(ctx) })
	}
	close(n.ready)
	n.pump(ctx)
	workers.Wait()
	return nil
}

// ---- 出站 ----

type jobKind uint8

const (
	jobSend jobKind = iota + 1
	jobReport
	jobDispatch
	jobDestroy
	jobCall
)

type job struct {
	kind    jobKind
	send    outgoing
	report  ActorReport
	handler nest.HandlerName
	ids     []int64
	params  nest.Params
	entity  fentity.IThreadSafeEntity
	call    func(context.Context) // jobCall：在出站 goroutine 上执行（只做准入的投递，例如 GroupResync）
}

type outgoing struct {
	sid int32
	msg *wireMsg
}

func (n *Node) enqueue(j job) bool {
	if j.kind == jobSend {
		return n.enqueueSend(j.send)
	}
	if j.kind == jobDestroy {
		return n.enqueueDestroy(j)
	}
	n.outputPending.Add(1)
	select {
	case n.out <- j:
		return true
	default:
		n.outputPending.Add(-1)
		n.stats.outboxDrops.Add(1)
		return false
	}
}

// pump 只处理本地报告、Nest 准入与快速回调；网络和实体销毁各自运行，不能阻塞这里。
// 队列空或攒满 ReportBatch 就把报告投给 Scene，不等待固定节拍。
func (n *Node) pump(ctx context.Context) {
	var reports []ActorReport
	flush := func() {
		if len(reports) == 0 {
			return
		}
		batch := reports
		reports = nil
		defer n.outputPending.Add(-int64(len(batch)))
		if err := n.cfg.Nest.Dispatch(ctx, hReports, n.cfg.Scene, nest.Params{batch}); err != nil {
			n.stats.dispatchErrors.Add(1)
		}
	}
	for {
		var j job
		select {
		case <-ctx.Done():
			return
		case j = <-n.out:
		}
		for {
			switch j.kind {
			case jobReport:
				reports = append(reports, j.report)
				if len(reports) >= n.cfg.ReportBatch {
					flush()
				}
			case jobDispatch:
				if err := n.dispatch(ctx, j.handler, j.ids, j.params); err != nil {
					n.stats.dispatchErrors.Add(1)
				}
			case jobCall:
				j.call(ctx)
			}
			if j.kind != jobReport {
				n.outputPending.Add(-1)
			}
			select {
			case j = <-n.out:
				continue
			default:
			}
			break
		}
		flush()
	}
}

func (n *Node) publish(o outgoing) error {
	if err := n.checkLease(); err != nil {
		return err
	}
	o.msg.From = n.cfg.SID
	if o.msg.SentAt == 0 {
		o.msg.SentAt = time.Now().UnixNano()
	}
	encoded := n.encodeReplica(o.sid, o.msg, time.Now())
	data, err := encodeMsg(encoded)
	if err == nil && len(data) > n.cfg.MaxWireBytes {
		n.sentReplica.clearPeer(o.sid)
		if o.msg.Kind == kindWitness && len(o.msg.Witnesses) > 0 {
			// 按完整观察者包拆分；不截断实体或不透明字节。
			views := append([]*witnessMsg{o.msg.Witness}, o.msg.Witnesses...)
			mid := len(views) / 2
			first, second := *o.msg, *o.msg
			first.Witness, first.Witnesses = views[0], views[1:mid]
			second.Witness, second.Witnesses = views[mid], views[mid+1:]
			return errors.Join(n.publish(outgoing{sid: o.sid, msg: &first}), n.publish(outgoing{sid: o.sid, msg: &second}))
		}
		err = fmt.Errorf("cellworld: message has %d bytes, limit %d", len(data), n.cfg.MaxWireBytes)
	}
	if err == nil {
		err = n.cfg.Transport.Publish(Subject(n.cfg.World, o.sid), data)
	}
	if err != nil {
		n.stats.publishErrors.Add(1)
		if o.msg.Kind == kindGhost || o.msg.Kind == kindWitness {
			n.sentReplica.clearPeer(o.sid)
		}
		return err
	}
	n.stats.msgsOut.Add(1)
	n.stats.bytesOut.Add(uint64(len(data)))
	return nil
}

// commit 在当前事务提交之后执行 fn；不在事务里时立即执行。
func commit(fn func()) {
	if !nest.AfterCommit(fn) {
		fn()
	}
}

// txEffects 收集一个 handler 里要在提交之后做的事。
type txEffects struct {
	sends    []outgoing
	destroys []fentity.IThreadSafeEntity
	follows  []job
	reports  []ActorReport
	hooks    []func()
	owners   []ownerWork
}

func (fx *txEffects) send(sid int32, m *wireMsg) {
	fx.sends = append(fx.sends, outgoing{sid: sid, msg: m})
}

// registerCommit 把 fx 交给当前事务的提交后回调：先调观察点（先于发送，跨进程观察到的顺序与因果一致），再排出站。
func (n *Node) registerCommit(fx *txEffects) {
	if err := n.guardTransaction(); err != nil {
		return
	}
	if len(fx.sends)+len(fx.destroys)+len(fx.follows)+len(fx.reports)+len(fx.hooks)+len(fx.owners) == 0 {
		return
	}
	commit(func() {
		if n.checkLease() != nil {
			return
		}
		for _, h := range fx.hooks {
			h()
		}
		for _, work := range fx.owners {
			n.enqueueOwner(work)
		}
		for _, s := range fx.sends {
			n.enqueue(job{kind: jobSend, send: s})
		}
		for _, r := range fx.reports {
			n.enqueue(job{kind: jobReport, report: r})
		}
		for _, f := range fx.follows {
			n.enqueue(f)
		}
		for _, e := range fx.destroys {
			n.enqueue(job{kind: jobDestroy, entity: e})
		}
	})
}

// ---- 入站：传输回调 ----

// receive 是传输回调：解码后投成 Nest 请求。不阻塞（Dispatch 只做准入）；准入失败计数，协议靠重传容忍。
func (n *Node) receive(data []byte) {
	if !n.admit() {
		return
	}
	defer n.admitted()
	n.stats.msgsIn.Add(1)
	n.stats.bytesIn.Add(uint64(len(data)))
	if len(data) > n.cfg.MaxWireBytes {
		n.stats.decodeErrors.Add(1)
		return
	}
	m, err := decodeMsg(data)
	if err != nil {
		n.stats.decodeErrors.Add(1)
		return
	}
	if m.Kind == kindReplicaReset {
		n.sentReplica.clearPeer(m.From)
		return
	}
	messages, err := n.decodeReplica(m)
	if err != nil {
		if errors.Is(err, errReplicaGap) {
			n.stats.replicaGaps.Add(1)
			n.requestReplicaReset(m.From, time.Now())
		} else {
			n.stats.decodeErrors.Add(1)
		}
		return
	}
	for _, decoded := range messages {
		n.receiveMessage(decoded)
	}
}

func (n *Node) receiveMessage(m *wireMsg) {
	if m.Kind == kindDeathAck {
		if n.hostsCell {
			n.deathPending.Add(1)
			select {
			case n.deathAcks <- receivedDeathAck{from: m.From, ack: *m.DeathAck}:
			default:
				n.deathPending.Add(-1)
				n.stats.deathErrors.Add(1)
			}
		}
		return
	}
	if m.Kind == kindDeath {
		if n.cfg.PlayerEntity != nil {
			if err := n.dispatch(context.Background(), hDeath, []int64{n.cfg.PlayerEntity(m.Death.Hit.Attacker)}, nest.Params{m}); err != nil {
				n.stats.dispatchErrors.Add(1)
			}
		}
		return
	}
	ctx := context.Background()
	params := nest.Params{m}
	dispatch := func(handler nest.HandlerName, ids ...int64) {
		if err := n.dispatch(ctx, handler, ids, params); err != nil {
			n.stats.dispatchErrors.Add(1)
		}
	}
	cellOnly := func() bool {
		if !n.hostsCell {
			n.stats.decodeErrors.Add(1)
		}
		return n.hostsCell
	}
	switch m.Kind {
	case kindGhost, kindDamageReply:
		if cellOnly() {
			dispatch(hRegion, n.cfg.Scene)
		}
	case kindOffer, kindOfferReply, kindDamage, kindEnter, kindLeave, kindPanel:
		if cellOnly() {
			if id := messageActor(m); id != 0 {
				dispatch(hActor, n.cfg.Scene, id)
			}
		}
	case kindInput:
		if cellOnly() && m.Input.ID != 0 {
			dispatch(hInput, m.Input.ID)
		}
	case kindAttack:
		if cellOnly() && m.Attack.Hit.Attacker != 0 && m.Attack.Target != 0 {
			if m.Attack.Target == m.Attack.Hit.Attacker {
				dispatch(hAttack, n.cfg.Scene, m.Attack.Hit.Attacker)
			} else {
				dispatch(hAttack, n.cfg.Scene, m.Attack.Hit.Attacker, m.Attack.Target)
			}
		}
	case kindGroupSnap:
		n.onGroupSnap(m.GroupSnap)
	case kindExt:
		n.onExt(m.From, m.Ext)
	case kindGroupNotice:
		if n.cfg.PlayerEntity == nil {
			n.stats.decodeErrors.Add(1)
			return
		}
		for _, player := range m.GroupNotice.To {
			if player != 0 {
				dispatch(hBase, player)
			}
		}
	case kindOwner, kindWitness, kindAttackResult, kindLost, kindEnterRefused, kindWriteback, kindPanelResult:
		if n.cfg.PlayerEntity == nil {
			n.stats.decodeErrors.Add(1)
			return
		}
		if player := n.cfg.PlayerEntity(messageActor(m)); player != 0 {
			dispatch(hBase, player)
		}
	default:
		n.stats.decodeErrors.Add(1)
	}
}

// messageActor 是消息所针对的角色 / NPC 的实体 ID。
func messageActor(m *wireMsg) int64 {
	switch m.Kind {
	case kindOffer:
		return m.Offer.State.ID
	case kindOfferReply:
		return m.OfferReply.ID
	case kindDamage:
		return m.Damage.Target
	case kindEnter:
		return m.Enter.State.ID
	case kindLeave:
		return m.Leave.ID
	case kindOwner:
		return m.Owner.ID
	case kindWitness:
		return m.Witness.Observer
	case kindAttackResult:
		return m.AttackResult.Hit.Attacker
	case kindLost:
		return m.Lost.State.ID
	case kindEnterRefused:
		return m.EnterRefused.ID
	case kindWriteback:
		return m.Writeback.State.ID
	case kindPanel:
		return m.Panel.ID
	case kindPanelResult:
		return m.PanelResult.ID
	}
	return 0
}

// ---- Nest handler ----

var (
	hReports     = nest.NewHandlerName("cellworld_reports")
	hRegion      = nest.NewHandlerName("cellworld_region")
	hActor       = nest.NewHandlerName("cellworld_actor")
	hInput       = nest.NewHandlerName("cellworld_input")
	hAttack      = nest.NewHandlerName("cellworld_attack")
	hImplied     = nest.NewHandlerName("cellworld_implied")
	hSpawn       = nest.NewHandlerName("cellworld_spawn")
	hBase        = nest.NewHandlerName("cellworld_base")
	hBaseCmd     = nest.NewHandlerName("cellworld_base_cmd")
	hBaseEnter   = nest.NewHandlerName("cellworld_base_enter")
	hInspect     = nest.NewHandlerName("cellworld_inspect")
	hOwnerResult = nest.NewHandlerName("cellworld_owner_result")
	hDeath       = nest.NewHandlerName("cellworld_death")
)

// Handlers 是 Node 在 Nest 引擎上的 handler 名单，可以在 Node 建出来之前注册：正式进程里服务经 wiring/nest 的
// NestHandlerInstaller 钩子在引擎启动前调用 Register（实例表只在 Start 之前接受注册），Node 在 Service.Init 里才建好
// （它要场景实体、场景桥的 Manager / Interest），NewNode 时经 NodeConfig.Handlers 接上。接上之前的派发答
// errHandlersUnbound（不会发生：只有 Node 自己投这些消息）。一个 Handlers 只接一个 Node，不是进程级注册表：
// 它跟着服务实例走，同一进程的两个引擎各有各的（维护者 2026-11-03 决定 E3 = B）。
type Handlers struct {
	node atomic.Pointer[Node]
}

var errHandlersUnbound = errors.New("cellworld: handlers are not bound to a node yet")

// NewHandlers 返回一份还没接上 Node 的 handler 名单。
func NewHandlers() *Handlers { return &Handlers{} }

// Register 把全部 handler 装到 Nest 引擎实例上（引擎 Start 之前）。Cell 侧与 Base 侧的 handler 都注册：注册时还不知道
// 本进程托管哪一格，没用到的那一侧永远不会被投递。全部 rollback=undo：区域表与角色 DAO 的修改随事务回滚。
func (h *Handlers) Register(engine nest.HandlerRegistrar) error {
	meta := nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync}
	for _, entry := range []struct {
		name nest.HandlerName
		run  func(*Node, []fentity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error)
	}{
		{hDeath, (*Node).handleDeath}, {hReports, (*Node).handleReports}, {hRegion, (*Node).handleRegion}, {hActor, (*Node).handleActor},
		{hInput, (*Node).handleInput}, {hAttack, (*Node).handleAttack}, {hImplied, (*Node).handleImplied},
		{hOwnerResult, (*Node).handleOwnerResult}, {hSpawn, (*Node).handleSpawn}, {hInspect, (*Node).handleInspect}, {hTier, (*Node).handleTier},
		{hBase, (*Node).handleBase}, {hBaseCmd, (*Node).handleBaseCmd}, {hBaseEnter, (*Node).handleBaseCmd},
	} {
		run := entry.run
		handler := func(es []fentity.IThreadSafeEntity, params []any, opts ...nest.HandlerOption) (any, error) {
			n := h.node.Load()
			if n == nil {
				return nil, errHandlersUnbound
			}
			if err := n.guardTransaction(); err != nil {
				return nil, err
			}
			return run(n, es, params, opts...)
		}
		entryMeta := meta
		if entry.name == hBaseEnter || entry.name == hDeath {
			entryMeta.Durability = nest.DurabilityStrict
		}
		if err := engine.RegisterHandlerWithMeta(entry.name, handler, entryMeta); err != nil {
			return fmt.Errorf("cellworld: register %s: %w", entry.name.String(), err)
		}
	}
	return nil
}

// bind 把名单接到 n；已经接了别的 Node 时报错。
func (h *Handlers) bind(n *Node) error {
	if !h.node.CompareAndSwap(nil, n) && h.node.Load() != n {
		return errors.New("cellworld: handlers are already bound to another node")
	}
	return nil
}

// RegisterHandlers 把本 Node 的 handler 装到 Nest 引擎上（引擎 Start 之前；Node 先于引擎启动建好时用，例如测试）。
// 与 NodeConfig.Handlers 是同一份名单。
func (n *Node) RegisterHandlers(engine nest.HandlerRegistrar) error {
	return n.handlers.Register(engine)
}

func regionOf(es []fentity.IThreadSafeEntity) (*Region, error) {
	if len(es) == 0 || es[0] == nil {
		return nil, errors.New("cellworld: scene entity is not loaded")
	}
	host, ok := es[0].(RegionHost)
	if !ok || host.CellRegion() == nil {
		return nil, fmt.Errorf("cellworld: entity %T holds no region", es[0])
	}
	return host.CellRegion(), nil
}

// dispatch 把消息投给 handler：ids[0] 是 Scene（多实体请求）或唯一的角色。名单里不在本进程内存的角色 / NPC（还没建出、
// 已交出或已销毁）由 Nest 按不存在处理：它们是不持久的种类，getter 内存未命中即答 nil（framework/entity
// kindHasNoAuthority，维护者 2026-11-03 决定 E1 = B），多实体请求里 handler 拿到 nil、按实体 ID 取角色（actorOf），
// 只锁角色的单实体请求（输入、换档）由 Nest 以 ErrEntityNotFound 丢弃（Base 从新 owner 的 witness 里看到输入序号
// 落后会重发；区域按下一份报告重新判断档位）。新建实体的请求都锁 Scene，所以“检查时不在、handler 里已被另一个 Scene
// handler 建出”时新建失败、事务回滚，由对方重传。
func (n *Node) dispatch(ctx context.Context, handler nest.HandlerName, ids []int64, params nest.Params) error {
	if len(ids) == 1 {
		return n.cfg.Nest.Dispatch(ctx, handler, ids[0], params)
	}
	return n.cfg.Nest.DispatchMulti(ctx, handler, ids, params)
}

// actorOf 在 handler 拿到的实体里找 ID 为 id 的角色；不在本进程（getter 答 nil）时为 nil。
func actorOf(es []fentity.IThreadSafeEntity, id int64) Actor {
	if id == 0 {
		return nil
	}
	for _, e := range es {
		if e != nil && e.ID() == id {
			a, _ := e.(Actor)
			return a
		}
	}
	return nil
}

func (n *Node) handleReports(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	reports, _ := params[0].([]ActorReport)
	fx := &txEffects{}
	n.applyReports(r, reports, time.Now(), &fx.sends)
	n.registerCommit(fx)
	return nil, nil
}

func (n *Node) handleRegion(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	now := time.Now()
	fx := &txEffects{}
	switch m.Kind {
	case kindGhost:
		for _, done := range n.applyGhostBatch(r, m.From, m.Ghost, now) {
			// 隐式确认：交给角色的 handler 收尾（这里不能锁没声明的角色）。
			reply := &wireMsg{Kind: kindOfferReply, From: done.from, OfferReply: &handoffReply{ID: done.id, Epoch: done.epoch, Accepted: true}}
			fx.follows = append(fx.follows, job{kind: jobDispatch, handler: hImplied, ids: []int64{n.cfg.Scene, done.id}, params: nest.Params{reply}})
		}
		if hook := n.cfg.Hooks.GhostApplied; hook != nil && m.SentAt != 0 {
			latency, entries := now.Sub(time.Unix(0, m.SentAt)), len(m.Ghost.Updates)+len(m.Ghost.Removes)
			fx.hooks = append(fx.hooks, func() { hook(n.cell, latency, entries) })
		}
	case kindDamageReply:
		n.onDamageReply(r, m.DamageReply, now, fx)
	}
	n.registerCommit(fx)
	return nil, nil
}

func (n *Node) handleActor(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	a := actorOf(es, messageActor(m))
	now := time.Now()
	fx := &txEffects{}
	switch m.Kind {
	case kindOffer:
		err = n.onOffer(r, a, m.From, m.Offer, now, fx)
	case kindOfferReply:
		n.onOfferReply(r, a, m.From, m.OfferReply, now, fx)
	case kindDamage:
		n.onDamage(r, a, m.From, m.Damage, now, fx)
	case kindEnter:
		err = n.onEnter(r, a, m.From, m.Enter, now, fx)
	case kindLeave:
		if a != nil && m.From == a.CellState().Base {
			n.onLeave(r, a, m.Leave, now, fx)
		}
	case kindPanel:
		n.onPanel(r, a, m.From, m.Panel, now, fx)
	}
	if err != nil {
		return nil, err // 回滚：区域表、新建的实体一起撤销，不回复（对方重传）
	}
	n.registerCommit(fx)
	return nil, nil
}

// handleImplied 是隐式确认的收尾：与收到“已接受”回复相同。
func (n *Node) handleImplied(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	fx := &txEffects{}
	n.onOfferReply(r, actorOf(es, m.OfferReply.ID), m.From, m.OfferReply, time.Now(), fx)
	n.registerCommit(fx)
	return nil, nil
}

// handleInput 只锁角色：移动意图与租约续期。
func (n *Node) handleInput(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	m := params[0].(*wireMsg)
	a := actorOf(es, m.Input.ID)
	if a == nil {
		n.stats.inputsDropped.Add(1)
		return nil, nil
	}
	st := a.CellState()
	if st.Role != RoleReal || m.From != st.Base || m.Input.Entry != st.Entry {
		// 非 Real 时丢弃：Base 从新 owner 的 witness 里看到输入序号落后会重发最新意图。
		n.stats.inputsDropped.Add(1)
		return nil, nil
	}
	st.LeaseUntil = time.Now().Add(n.cfg.LeaseTimeout).UnixNano()
	if m.Input.Seq > st.LastInput {
		clampSpeed := func(v int64) int64 { return max(-n.cfg.MaxSpeed, min(n.cfg.MaxSpeed, v)) }
		vx, vy := clampSpeed(m.Input.VX), clampSpeed(m.Input.VY)
		if st.VX != vx || st.VY != vy {
			st.VX, st.VY = vx, vy
			st.Ver.Seq++
		}
		st.LastInput = m.Input.Seq
	}
	a.SetCellState(st)
	return nil, nil
}

// ---- 交接 ----

// onOffer 只在 Nest 中预留 Pending 实例。接受授权在锁外做 CAS，完成后回到 Nest 激活；
// 预留事务失败就回滚、不发授权请求，源端按原交接身份重传。
func (n *Node) onOffer(r *Region, a Actor, from int32, o *handoffOffer, now time.Time, fx *txEffects) error {
	if err := o.Recovery.Validate(); err != nil {
		return err
	}
	n.stats.offersReceived.Add(1)
	st := o.State
	if st.ID == 0 || o.Grant.Process.SID != from || o.Transfer.Target != n.cfg.SID || o.Transfer.ID == "" || o.Transfer.Epoch != st.Ver.Epoch || o.Transfer.Epoch <= o.Grant.Epoch {
		return nil
	}
	if a != nil {
		cs := a.CellState()
		if cs.Role == RolePending && cs.Pending != nil && cs.Pending.Transfer == o.Transfer {
			fx.owners = append(fx.owners, n.ownerWork(cs))
			return nil
		}
		if cs.Role == RoleGone {
			return nil
		}
		if cs.Ver.Epoch >= st.Ver.Epoch {
			fx.send(from, &wireMsg{Kind: kindOfferReply, OfferReply: &handoffReply{ID: st.ID, Epoch: st.Ver.Epoch, Accepted: true}})
			return nil
		}
		// 旧实例先完成自己的退役，新的交接由源端重传；不在旧 I/O 未收尾时复用同 ID。
		return nil
	}
	target, inside := n.cfg.Grid.CellAt(st.Pos())
	if !inside || target != n.cell || r.activeCount() >= n.cfg.MaxReals {
		fx.owners = append(fx.owners, ownerWork{kind: ownerRejectWork, state: ActorState{EntityState: st, Grant: o.Grant}, transfer: o.Transfer})
		return nil
	}
	var err error
	a, err = n.cfg.CreateActor(st.ID, st.Kind)
	if err != nil {
		return err
	}
	next := ActorState{EntityState: st, Role: RolePending, Grant: o.Grant, Pending: &PendingOwner{Token: o.Transfer.ID, Transfer: o.Transfer},
		SpawnGeneration: o.SpawnGeneration, Recovery: o.Recovery.Clone().At(now), LastInput: o.LastInput, Applied: restoreApplied(o.Applied, now.UnixNano(), n.cfg.DedupWindow), Entry: o.Entry, FracX: validFrac(o.FracX), FracY: validFrac(o.FracY)}
	n.setState(a, a.CellState(), next)
	r.locals.set(st.ID, localRec{St: st, Role: RolePending})
	fx.owners = append(fx.owners, n.ownerWork(next))
	return a.StartCell()
}

// install 把 a 写成本格的 Real（epoch = st.Ver.Epoch），登记 seen 与区域索引；玩家的话通知 Base（提交后）。档位按
// st.Idle（交接带来的；新建的实体还没启动心跳时 SetCellPeriod 只记下档位，StartCell 按它启动）。
func (n *Node) install(r *Region, a Actor, st ActorState, now time.Time, fx *txEffects) {
	nowNano := now.UnixNano()
	st.Role, st.Handoff, st.RetryAfter, st.Pending = RoleReal, nil, 0, nil
	st.capHP() // Enter 的快照 HP 可能高于面板上限（交接带来的状态本来就夹过）
	if st.Kind == KindPlayer {
		st.LeaseUntil = nowNano + int64(n.cfg.LeaseTimeout)
		st.WritebackAt = nowNano + int64(n.cfg.WritebackEvery)
		st.Idle = false
	}
	n.setState(a, a.CellState(), st)
	r.seen.set(st.ID, seenEpoch{Epoch: st.Ver.Epoch, Accepted: true, At: nowNano})
	r.locals.set(st.ID, localRec{St: st.EntityState, Role: RoleReal, GhostedTo: n.cfg.Grid.Neighbors(n.cell)})
	r.ghosts.del(st.ID)
	r.tombs.del(st.ID)
	n.realChanged(st.ID, st.Ver.Epoch, true, fx)
	if st.Kind == KindPlayer && st.Base != 0 {
		fx.send(st.Base, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: st.Ver.Epoch, LastInput: st.LastInput, PanelVer: st.Panel.Ver}})
	}
}

func (n *Node) realChanged(id int64, epoch uint64, acquired bool, fx *txEffects) {
	if hook := n.cfg.Hooks.RealChanged; hook != nil {
		cell := n.cell
		fx.hooks = append(fx.hooks, func() { hook(cell, id, epoch, acquired) })
	}
}

// onOfferReply 只处理与本格当前冻结 epoch、目标都一致的回复；其余（重复、迟到、过期）丢弃。
func (n *Node) onOfferReply(_ *Region, a Actor, from int32, rep *handoffReply, _ time.Time, fx *txEffects) {
	if a == nil {
		return
	}
	st := a.CellState()
	if st.Role != RoleFrozen || st.Handoff == nil || st.Handoff.Epoch != rep.Epoch || n.cfg.Grid.SID(st.Handoff.Target) != from {
		n.stats.staleDrops.Add(1)
		return
	}
	// 回复与 Ghost 只是催查信号。不能根据目标的进程内 seen 表恢复写入。
	fx.owners = append(fx.owners, n.ownerWork(st))
}

func (n *Node) handoffConcluded(cs ActorState, accepted bool, now time.Time, fx *txEffects) {
	if accepted {
		n.stats.handoffsAccepted.Add(1)
	}
	if hook := n.cfg.Hooks.HandoffDone; hook != nil && cs.Handoff != nil {
		cell, id, epoch, elapsed := n.cell, cs.ID, cs.Handoff.Epoch, now.Sub(time.Unix(0, cs.Handoff.StartedAt))
		fx.hooks = append(fx.hooks, func() { hook(cell, id, epoch, accepted, elapsed) })
	}
}

// ---- 进入 / 离开 / 生成 ----

// onEnter：Base 让玩家进入。本格已是 Real 时幂等返回 owner；本格知道 Real 在别处（Frozen 或存活 Ghost）时拒绝并提示
// owner，避免造出第二个 Real。接受时按 Base 带来的 Player 快照（位置、HP）新建 SceneActor。
func (n *Node) onEnter(r *Region, a Actor, from int32, req *enterReq, now time.Time, fx *txEffects) error {
	if err := req.Recovery.Validate(); err != nil {
		return err
	}
	st := req.State
	refuse := func(hint int32) {
		fx.send(from, &wireMsg{Kind: kindEnterRefused, EnterRefused: &enterRefused{ID: st.ID, Hint: hint}})
	}
	if st.ID == 0 || req.Entry == 0 || from <= 0 {
		return nil
	}
	if a != nil {
		cs := a.CellState()
		switch cs.Role {
		case RoleReal:
			if from != cs.Base || req.Entry < cs.Entry {
				n.stats.staleDrops.Add(1)
				return nil
			}
			// 幂等重进（Base 重启、看门狗误判）：Real 留着，Base 带来的面板更新就应用（与面板命令同一条规则）。
			// 入场序号只前进：Base 重启后的新一次入场接管这个 Real（之后它的 Leave 能删），迟到的旧 Enter 不把序号改回去
			// （否则上一次入场迟到的 Leave 又能删它，RV-20261106-03）。
			if req.Entry > cs.Entry {
				// 新入场先提高共享入场高水位，再接管现有状态；期间暂停写入，不从旧快照覆盖位置 / HP。
				prev := cs
				cs.Entry = req.Entry
				// CloseEntry 属于上一次入场（死亡玩家的 Leave 只打标记、等收尾）；新入场接管后不能沿用，
				// 否则认领成功会立刻按新 Entry 退役并关闭这次入场。
				cs.CloseEntry = false
				cs.Role = RolePending
				cs.Pending = &PendingOwner{Token: rand.Text()}
				n.applyPanel(&cs, st.Panel, now, fx)
				n.setState(a, prev, cs)
				r.locals.set(cs.ID, localRec{St: cs.EntityState, Role: RolePending})
				n.realChanged(cs.ID, cs.Ver.Epoch, false, fx)
				fx.owners = append(fx.owners, n.ownerWork(cs))
				return nil
			}
			cs.LeaseUntil = now.Add(n.cfg.LeaseTimeout).UnixNano()
			n.applyPanel(&cs, st.Panel, now, fx)
			a.SetCellState(cs)
			fx.send(from, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: cs.Ver.Epoch, LastInput: cs.LastInput, PanelVer: cs.Panel.Ver}})
			return nil
		case RolePending:
			fx.owners = append(fx.owners, n.ownerWork(cs))
			return nil
		case RoleFrozen:
			refuse(n.cfg.Grid.SID(cs.Handoff.Target))
			return nil
		default:
			refuse(0) // 旧实例正等销毁：Base 的看门狗稍后重发
			return nil
		}
	}
	if g, ok := r.ghosts.get(st.ID); ok && now.UnixNano()-g.HeardAt <= int64(n.cfg.GhostTTL) {
		refuse(n.cfg.Grid.SID(g.Owner))
		return nil
	}
	if !n.rect.Contains(st.Pos()) {
		target, _ := n.cfg.Grid.CellAt(st.Pos())
		refuse(n.cfg.Grid.SID(target))
		return nil
	}
	if r.activeCount() >= n.cfg.MaxReals {
		refuse(0)
		return nil
	}
	created, err := n.cfg.CreateActor(st.ID, KindPlayer)
	if err != nil {
		return fmt.Errorf("cellworld: enter %d: %w", st.ID, err)
	}
	st.Kind, st.Base = KindPlayer, from
	st.VX, st.VY = 0, 0
	st.Ver = Version{Epoch: req.EpochHint, Seq: 1}
	next := ActorState{EntityState: st, Role: RolePending, Grant: OwnerGrant{ID: rand.Text(), Process: n.ownerProcess()}, Pending: &PendingOwner{}, LastInput: req.LastInput, Entry: req.Entry, Recovery: req.Recovery.Clone().At(now)}
	next.Pending.Token = next.Grant.ID
	created.SetCellState(next)
	r.locals.set(st.ID, localRec{St: st, Role: RolePending})
	fx.owners = append(fx.owners, n.ownerWork(next))
	return created.StartCell()
}

// onLeave：Base 让玩家离场。只删 Entry 与请求相同的 Real（RV-20261106-03）：上一次入场重复 / 迟到的 Leave 碰到同 ID
// 重新入场的角色时丢弃。非 Real（冻结、等销毁）也不处理：交接中的离场由新 owner 的租约兜底。
func (n *Node) onLeave(r *Region, a Actor, req *leaveReq, now time.Time, fx *txEffects) {
	if a == nil {
		return
	}
	cs := a.CellState()
	if cs.Death != nil && cs.Entry == req.Entry {
		cs.CloseEntry = true
		a.SetCellState(cs)
		return
	}
	if cs.Role != RoleReal && cs.Role != RolePending {
		return
	}
	if cs.Entry != req.Entry {
		n.stats.staleDrops.Add(1)
		return
	}
	if cs.Role == RolePending {
		// 已准入的授权 I/O 不能按取消猜测未执行。保留待决身份，确认后只退役，不激活。
		cs.CloseEntry = true
		a.SetCellState(cs)
		fx.owners = append(fx.owners, n.ownerWork(cs))
		return
	}
	var afx actorEffects
	cs.CloseEntry = true
	n.leave(&cs, &afx, now.UnixNano())
	a.SetCellState(cs)
	n.realChanged(cs.ID, cs.Ver.Epoch, false, fx)
	fx.sends = append(fx.sends, afx.sends...)
	fx.owners = append(fx.owners, afx.owners...)
	n.applyReports(r, []ActorReport{*afx.report}, now, &fx.sends)
}

// Spawn 在本格预留一个 NPC Pending：位置须在本格、ID 非零且尚未占用。返回 nil 只表示预留已提交，
// 授权确认后通过 Hooks.RealChanged 通知激活，epoch 由共享记录分配。
func (n *Node) Spawn(ctx context.Context, st EntityState) error {
	if !n.hostsCell {
		return errors.New("cellworld: node hosts no cell")
	}
	if st.ID == 0 || !n.rect.Contains(st.Pos()) {
		return fmt.Errorf("cellworld: spawn %d at %v outside cell %v", st.ID, st.Pos(), n.cell)
	}
	// 已在本进程时 handler 看到它并拒绝；不在时 handler 拿到 nil、在事务里新建。
	_, err := n.cfg.Nest.RequestMulti(ctx, hSpawn, []int64{n.cfg.Scene, st.ID}, nest.Params{spawnRequest{State: st}})
	return err
}

func (n *Node) handleSpawn(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	request := params[0].(spawnRequest)
	st := request.State
	if a := actorOf(es, st.ID); a != nil {
		if request.Generation > 0 && a.CellState().SpawnGeneration == request.Generation {
			return nil, nil
		}
		return nil, errors.New("cellworld: spawn: already exists")
	}
	a, err := n.cfg.CreateActor(st.ID, KindNPC)
	if err != nil {
		return nil, err
	}
	st.Kind, st.Base, st.Idle = KindNPC, 0, false // 新生成的 NPC 先快档，区域按附近有没有玩家再降
	if r.activeCount() >= n.cfg.MaxReals {
		return nil, errors.New("cellworld: spawn capacity exceeded")
	}
	st.Ver = Version{Seq: 1}
	next := ActorState{SpawnGeneration: request.Generation, EntityState: st, Role: RolePending, Grant: OwnerGrant{ID: rand.Text(), Process: n.ownerProcess()}, Pending: &PendingOwner{}}
	next.Pending.Token = next.Grant.ID
	a.SetCellState(next)
	r.locals.set(st.ID, localRec{St: st, Role: RolePending})
	fx := &txEffects{owners: []ownerWork{n.ownerWork(next)}}
	if err := a.StartCell(); err != nil {
		return nil, err
	}
	n.registerCommit(fx)
	return nil, nil
}

// ---- 心跳 ----

// ActorBeat 是角色 / NPC 的格内心跳回调：只锁角色本身，结果在提交后报给区域、发出 Offer / 回写 / Lost、销毁实体。
// elapsed 是距上一拍的实际经过时间（heartbeat.Beat.Elapsed：首拍从启动起算，已按上限夹过）：位置按它积分，不按回调
// 次数——驱动变慢、跳拍时角色照样按设定速度走完这段时间（docs/framework/impl/10-time.md“不以回调次数推算经过时间”）。
func (n *Node) ActorBeat(a Actor, now time.Time, elapsed time.Duration) error {
	if err := n.guardTransaction(); err != nil {
		return err
	}

	n.stats.actorBeats.Add(1)
	st := a.CellState()
	if st.Role == RoleNone {
		return nil
	}
	before := st.Role
	prev := st
	afx := n.stepActor(&st, now, elapsed)
	n.setState(a, prev, st)
	fx := &txEffects{sends: afx.sends, owners: afx.owners, hooks: afx.hooks}
	if afx.report != nil {
		fx.reports = append(fx.reports, *afx.report)
	}
	if before == RoleReal && st.Role != RoleReal {
		n.realChanged(st.ID, before.epochOf(st), false, fx)
	}
	n.registerCommit(fx)
	return nil
}

// epochOf 是失去 Real 身份前的 epoch：冻结时 epoch 已经 +1。
func (r Role) epochOf(st ActorState) uint64 {
	if st.Role == RoleFrozen {
		return st.Ver.Epoch - 1
	}
	return st.Ver.Epoch
}

// RegionBeat 是 Scene 的 scene.cell 心跳回调：过期、伤害重试、NPC 档位、witness，结果在提交后发出。
func (n *Node) RegionBeat(host RegionHost, now time.Time) error {
	if err := n.guardTransaction(); err != nil {
		return err
	}
	start := time.Now()
	r := host.CellRegion()
	if r == nil {
		return nil
	}
	fx := &txEffects{}
	n.regionBeat(r, now, fx)
	n.stats.ticks.Add(1)
	if hook := n.cfg.Hooks.TickDone; hook != nil {
		elapsed := time.Since(start)
		reals, ghosts := int(n.stats.reals.Load()), int(n.stats.ghosts.Load())
		fx.hooks = append(fx.hooks, func() { hook(n.cell, elapsed, reals, ghosts) })
	}
	n.registerCommit(fx)
	return nil
}

// ---- 诊断 ----

// EntitySnapshot 是 Inspect 返回的一条记录。
type EntitySnapshot struct {
	State EntityState
	Role  string // real / frozen / ghost
	Owner CellID // Ghost 的来源格 / Frozen 的交接目标
}

// Inspect 返回本格区域表的快照（测试与诊断用；一次 Scene 上的 Nest 请求）。
func (n *Node) Inspect(ctx context.Context) ([]EntitySnapshot, error) {
	if !n.hostsCell {
		return nil, errors.New("cellworld: node hosts no cell")
	}
	out, err := n.cfg.Nest.Request(ctx, hInspect, n.cfg.Scene, nil)
	if err != nil {
		return nil, err
	}
	snapshot, _ := out.([]EntitySnapshot)
	return snapshot, nil
}

func (n *Node) handleInspect(es []fentity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	var out []EntitySnapshot
	for _, l := range r.locals.m {
		s := EntitySnapshot{State: l.St, Role: l.Role.String()}
		if l.Handoff != nil {
			s.Owner = l.Handoff.Target
		}
		out = append(out, s)
	}
	for id, g := range r.ghosts.m {
		if _, local := r.locals.get(id); !local {
			out = append(out, EntitySnapshot{State: g.St, Role: "ghost", Owner: g.Owner})
		}
	}
	slices.SortFunc(out, func(a, b EntitySnapshot) int { return int(a.State.ID - b.State.ID) })
	return out, nil
}

// CloseIngress 关闭本进程的 cellworld 收件，实现 app.IngressCloser（App 停机第一步：别的进程经传输发来的 Enter / 攻击 /
// 交接 / witness / 队伍快照与成员通知都是外部输入，要在 Service.Shutdown 与 Nest 停止之前关掉）。
//
// 关准入（幂等）：之后进来的回调直接丢弃（不解码、不计 MsgsIn、不投 Nest、不喂 mirror）；在 ctx 内等调用时正在执行的
// 回调返回（它们只做 Nest 准入或 mirror 的短锁更新），然后退订收件主题。ctx 先到期返回 ctx 错误，用新 ctx 再调继续等。
// 只停“接收”：出站 goroutine、Base / Cell 的心跳与已经准入 Nest 的请求照常，直到 Run 的 ctx 结束（Service.Shutdown）。
// 丢掉的消息由协议容忍（重传、租约、看门狗），与进程退出时一样。
func (n *Node) CloseIngress(ctx context.Context) error {
	n.ingress.Lock()
	if !n.ingress.closed {
		n.ingress.closed = true
		n.ingress.idle = make(chan struct{})
		if n.ingress.busy == 0 {
			close(n.ingress.idle)
		}
	}
	idle := n.ingress.idle
	n.ingress.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		return ctx.Err()
	}
	// 准入门已经挡住新回调、在途的都返回了，再退订：有的传输（进程内替身）退订时要等投递 goroutine 的回调结束。
	n.ingress.Lock()
	unsubscribe := n.ingress.unsubscribe
	n.ingress.unsubscribe = nil
	n.ingress.Unlock()
	if unsubscribe != nil {
		_ = unsubscribe()
	}
	return nil
}

// admit 在传输回调开始时取准入：关闭之后答 false；成功时调用方处理完必须调 admitted。
func (n *Node) admit() bool {
	n.ingress.Lock()
	defer n.ingress.Unlock()
	if n.ingress.closed {
		return false
	}
	n.ingress.busy++
	return true
}

func (n *Node) admitted() {
	n.ingress.Lock()
	defer n.ingress.Unlock()
	n.ingress.busy--
	if n.ingress.closed && n.ingress.busy == 0 {
		close(n.ingress.idle)
	}
}
