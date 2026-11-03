package cellworld

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	// ---- Base 侧（PlayerEntity 为 nil 时本进程不托管玩家）----

	// PlayerEntity 把角色（SceneActor）ID 换成本进程 Player 实体 ID。
	PlayerEntity func(actorID int64) int64
	// WatchPeriod：Base 心跳（看门狗 + 续租）周期，必须是 heartbeat 档位；零值 100ms。
	WatchPeriod time.Duration
	// SilenceTimeout：玩家多久没收到 witness 就按最后位置重新进入；零值 1s。LostGrace：收到交接放弃后等多久再重进；零值 500ms。
	SilenceTimeout, LostGrace time.Duration
	// LeasePeriod：Base 向 owner 格续租（重发最新输入）的间隔；零值 500ms。
	LeasePeriod time.Duration
	// Mirrors / Interest / Session / MirrorKind：把 witness 喂给客户端（Sync mirror）。Mirrors 为 nil 时只记 owner，
	// 不推客户端（测试与不需要客户端的进程）。Session 把角色 ID 换成 Interest 的玩家（会话）ID；MirrorKind 给出对象
	// 在帧上的 namespace 与 kind。
	Mirrors    *policy.Mirrors[EntityState]
	Interest   *policy.Interest
	Session    func(actorID int64) int64
	MirrorKind func(EntityState) (string, uint32)
	// View 是玩家在 Interest 里的主视野（零值取缺省）。
	View policy.ObserverView

	// OutboxSize：出站队列容量（提交后的发送、报告、销毁）；满了丢弃并计数。零值 65536。
	OutboxSize int
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
	setInt(&cfg.OutboxSize, 65536)
	setInt(&cfg.ReportBatch, 512)
	return cfg
}

func (cfg NodeConfig) validate() error {
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
	if cfg.Cell != nil {
		switch {
		case !cfg.Grid.Contains(*cfg.Cell) || cfg.Grid.SID(*cfg.Cell) != cfg.SID:
			return bad("cell %v is not mapped to sid %d", *cfg.Cell, cfg.SID)
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
		case cfg.Mirrors != nil && (cfg.Interest == nil || cfg.Session == nil || cfg.MirrorKind == nil):
			return bad("mirrors need interest, session and mirror kind")
		}
	}
	return nil
}

// Node 是一个进程的 cellworld 端点：把传输上的消息投成 Nest 请求，把 Nest 事务提交后要做的事（发送、给区域的报告、
// 销毁实体）排进出站队列，由一个出站 goroutine 执行。Node 自己没有锁、不持有任何格内状态：区域表在 Scene 实体上
// （Region），角色状态在 SceneActor / Monster 的 DAO 上，玩家的路由状态在 Player 上（BaseState）。
//
// 出站队列是唯一的进程级结构：有界 channel，单消费者；Nest handler 的提交后回调只做非阻塞投递（满了丢弃并计数：
// 协议靠重传与下一拍的全量容忍丢失）。提交后回调里不能再 Dispatch（Nest 拒绝 handler 上下文里的异步投递），
// 所以“提交后再投一个 Nest 请求”（给区域的报告、隐式完成交接）也经它转发。
type Node struct {
	cfg         NodeConfig
	cell        CellID
	hostsCell   bool
	rect        spatial.Rect
	indexBounds spatial.Rect
	out         chan job
	ready       chan struct{}
	running     atomic.Bool
	lastExpire  atomic.Int64
	stats       nodeCounters
	handlers    *Handlers
}

// NewNode 校验配置并构造 Node。
func NewNode(cfg NodeConfig) (*Node, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	n := &Node{cfg: cfg, out: make(chan job, cfg.OutboxSize), ready: make(chan struct{}), handlers: cfg.Handlers}
	if n.handlers == nil {
		n.handlers = NewHandlers()
	}
	if err := n.handlers.bind(n); err != nil {
		return nil, err
	}
	if cfg.Cell != nil {
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
	unsubscribe, err := n.cfg.Transport.Subscribe(Subject(n.cfg.World, n.cfg.SID), n.receive)
	if err != nil {
		return fmt.Errorf("cellworld: subscribe sid %d: %w", n.cfg.SID, err)
	}
	defer func() { _ = unsubscribe() }()
	close(n.ready)
	n.pump(ctx)
	return nil
}

// ---- 出站 ----

type jobKind uint8

const (
	jobSend jobKind = iota + 1
	jobReport
	jobDispatch
	jobDestroy
)

type job struct {
	kind    jobKind
	send    outgoing
	report  ActorReport
	handler nest.HandlerName
	ids     []int64
	params  nest.Params
	entity  fentity.IThreadSafeEntity
}

type outgoing struct {
	sid int32
	msg *wireMsg
}

func (n *Node) enqueue(j job) {
	select {
	case n.out <- j:
	default:
		n.stats.outboxDrops.Add(1)
	}
}

// pump 是出站 goroutine：发送、转发报告（攒批投给 Scene）、跟进投递、销毁。队列空或攒满一批时把报告投出去，
// 所以报告延迟是“队列里排在它前面的工作”，不是固定的节拍。
func (n *Node) pump(ctx context.Context) {
	var reports []ActorReport
	flush := func() {
		if len(reports) == 0 {
			return
		}
		batch := reports
		reports = nil
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
			case jobSend:
				n.publish(j.send)
			case jobReport:
				reports = append(reports, j.report)
				if len(reports) >= n.cfg.ReportBatch {
					flush()
				}
			case jobDispatch:
				if err := n.dispatch(ctx, j.handler, j.ids, j.params); err != nil {
					n.stats.dispatchErrors.Add(1)
				}
			case jobDestroy:
				if err := n.cfg.Destroy(ctx, j.entity); err != nil {
					n.stats.destroyErrors.Add(1)
				}
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

func (n *Node) publish(o outgoing) {
	o.msg.From = n.cfg.SID
	if o.msg.SentAt == 0 {
		o.msg.SentAt = time.Now().UnixNano()
	}
	data, err := encodeMsg(o.msg)
	if err == nil {
		err = n.cfg.Transport.Publish(Subject(n.cfg.World, o.sid), data)
	}
	if err != nil {
		n.stats.publishErrors.Add(1)
		return
	}
	n.stats.msgsOut.Add(1)
	n.stats.bytesOut.Add(uint64(len(data)))
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
}

func (fx *txEffects) send(sid int32, m *wireMsg) {
	fx.sends = append(fx.sends, outgoing{sid: sid, msg: m})
}

// registerCommit 把 fx 交给当前事务的提交后回调：先调观察点（先于发送，跨进程观察到的顺序与因果一致），再排出站。
func (n *Node) registerCommit(fx *txEffects) {
	if len(fx.sends)+len(fx.destroys)+len(fx.follows)+len(fx.reports)+len(fx.hooks) == 0 {
		return
	}
	commit(func() {
		for _, h := range fx.hooks {
			h()
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
	n.stats.msgsIn.Add(1)
	n.stats.bytesIn.Add(uint64(len(data)))
	m, err := decodeMsg(data)
	if err != nil {
		n.stats.decodeErrors.Add(1)
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
	case kindOffer, kindOfferReply, kindDamage, kindEnter, kindLeave:
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
	case kindOwner, kindWitness, kindAttackResult, kindLost, kindEnterRefused, kindWriteback:
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
	}
	return 0
}

// ---- Nest handler ----

var (
	hReports = nest.NewHandlerName("cellworld_reports")
	hRegion  = nest.NewHandlerName("cellworld_region")
	hActor   = nest.NewHandlerName("cellworld_actor")
	hInput   = nest.NewHandlerName("cellworld_input")
	hAttack  = nest.NewHandlerName("cellworld_attack")
	hImplied = nest.NewHandlerName("cellworld_implied")
	hSpawn   = nest.NewHandlerName("cellworld_spawn")
	hBase    = nest.NewHandlerName("cellworld_base")
	hBaseCmd = nest.NewHandlerName("cellworld_base_cmd")
	hInspect = nest.NewHandlerName("cellworld_inspect")
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
		{hReports, (*Node).handleReports}, {hRegion, (*Node).handleRegion}, {hActor, (*Node).handleActor},
		{hInput, (*Node).handleInput}, {hAttack, (*Node).handleAttack}, {hImplied, (*Node).handleImplied},
		{hSpawn, (*Node).handleSpawn}, {hInspect, (*Node).handleInspect}, {hTier, (*Node).handleTier},
		{hBase, (*Node).handleBase}, {hBaseCmd, (*Node).handleBaseCmd},
	} {
		run := entry.run
		handler := func(es []fentity.IThreadSafeEntity, params []any, opts ...nest.HandlerOption) (any, error) {
			n := h.node.Load()
			if n == nil {
				return nil, errHandlersUnbound
			}
			return run(n, es, params, opts...)
		}
		if err := engine.RegisterHandlerWithMeta(entry.name, handler, meta); err != nil {
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
		n.onLeave(r, a, now, fx)
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
	if st.Role != RoleReal {
		// 非 Real 时丢弃：Base 从新 owner 的 witness 里看到输入序号落后会重发最新意图。
		n.stats.inputsDropped.Add(1)
		return nil, nil
	}
	st.LeaseUntil = time.Now().Add(n.cfg.LeaseTimeout).UnixNano()
	if m.Input.Seq > st.LastInput {
		clampSpeed := func(v int64) int64 { return max(-n.cfg.MaxSpeed, min(n.cfg.MaxSpeed, v)) }
		st.VX, st.VY = clampSpeed(m.Input.VX), clampSpeed(m.Input.VY)
		st.LastInput = m.Input.Seq
	}
	a.SetCellState(st)
	return nil, nil
}

// ---- 交接 ----

// onOffer 是目标格的决定，见 §5.3 / §7.4。接受时在本事务里新建（或复用本格仍冻结着的旧实例）角色实体并写成 Real，
// seen 也在本事务里登记；提交之后才回“已接受”。返回错误即回滚、不回复（旧格重传）。
func (n *Node) onOffer(r *Region, a Actor, from int32, o *handoffOffer, now time.Time, fx *txEffects) error {
	n.stats.offersReceived.Add(1)
	st := o.State
	epoch := st.Ver.Epoch
	nowNano := now.UnixNano()
	reply := func(accepted bool) {
		fx.send(from, &wireMsg{Kind: kindOfferReply, OfferReply: &handoffReply{ID: st.ID, Epoch: epoch, Accepted: accepted}})
	}
	if st.ID == 0 || epoch == 0 {
		return nil
	}
	reuse := false
	if a != nil {
		cs := a.CellState()
		switch {
		case cs.Role == RoleGone:
			// 本格的旧实例已交出、正等销毁：同 ID 的新实例要等它销毁完才能建。不回复，旧格按 OfferRetry 重传。
			if cs.Ver.Epoch >= epoch {
				reply(true)
			}
			return nil
		case cs.Role == RoleFrozen && cs.Handoff != nil && epoch > cs.Handoff.Epoch:
			// 实体已经从目标格再交接回本格：只有接受过本格 epoch 的目标才能发出更大的 epoch——隐式完成后复用这个实例。
			n.handoffConcluded(cs, true, now, fx)
			reuse = true
		case (cs.Role == RoleReal || cs.Role == RoleFrozen) && cs.Ver.Epoch >= epoch:
			reply(true)
			return nil
		}
	}
	if s, ok := r.seen.get(st.ID); ok && !reuse {
		switch {
		case s.Epoch > epoch:
			reply(true)
			return nil
		case s.Epoch == epoch:
			reply(s.Accepted)
			return nil
		}
	}
	if g, ok := r.ghosts.get(st.ID); ok && g.St.Ver.Epoch >= epoch {
		// 本地只有同 epoch 的 Ghost 而没有 seen 记录：按协议不可达；保守拒绝并记下，不冒险装出第二个 Real。
		r.seen.set(st.ID, seenEpoch{Epoch: epoch, Accepted: false, At: nowNano})
		reply(false)
		return nil
	}
	target, inWorld := n.cfg.Grid.CellAt(st.Pos())
	if !inWorld || target != n.cell || (!reuse && r.activeCount() >= n.cfg.MaxReals) {
		r.seen.set(st.ID, seenEpoch{Epoch: epoch, Accepted: false, At: nowNano})
		reply(false)
		return nil
	}
	created := false
	if a == nil {
		var err error
		if a, err = n.cfg.CreateActor(st.ID, st.Kind); err != nil {
			return fmt.Errorf("cellworld: install %d at epoch %d: %w", st.ID, epoch, err)
		}
		created = true
	}
	n.install(r, a, ActorState{EntityState: st, Role: RoleReal, LastInput: o.LastInput, Applied: o.Applied}, now, fx)
	if created {
		if err := a.StartCell(); err != nil {
			return fmt.Errorf("cellworld: start %d: %w", st.ID, err)
		}
	}
	reply(true)
	return nil
}

// install 把 a 写成本格的 Real（epoch = st.Ver.Epoch），登记 seen 与区域索引；玩家的话通知 Base（提交后）。档位按
// st.Idle（交接带来的；新建的实体还没启动心跳时 SetCellPeriod 只记下档位，StartCell 按它启动）。
func (n *Node) install(r *Region, a Actor, st ActorState, now time.Time, fx *txEffects) {
	nowNano := now.UnixNano()
	st.Role, st.Handoff, st.RetryAfter = RoleReal, nil, 0
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
		fx.send(st.Base, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: st.Ver.Epoch, LastInput: st.LastInput}})
	}
}

func (n *Node) realChanged(id int64, epoch uint64, acquired bool, fx *txEffects) {
	if hook := n.cfg.Hooks.RealChanged; hook != nil {
		cell := n.cell
		fx.hooks = append(fx.hooks, func() { hook(cell, id, epoch, acquired) })
	}
}

// onOfferReply 只处理与本格当前冻结 epoch、目标都一致的回复；其余（重复、迟到、过期）丢弃。
func (n *Node) onOfferReply(r *Region, a Actor, from int32, rep *handoffReply, now time.Time, fx *txEffects) {
	if a == nil {
		n.stats.staleDrops.Add(1)
		return
	}
	cs := a.CellState()
	if cs.Role != RoleFrozen || cs.Handoff == nil || cs.Handoff.Epoch != rep.Epoch || n.cfg.Grid.SID(cs.Handoff.Target) != from {
		n.stats.staleDrops.Add(1)
		return
	}
	if rep.Accepted {
		n.handoffConcluded(cs, true, now, fx)
		p := cs.Handoff
		prev := cs
		cs.Role, cs.Handoff = RoleGone, nil
		n.setState(a, prev, cs)
		fx.destroys = append(fx.destroys, a)
		n.applyReports(r, []ActorReport{{State: cs.EntityState, Role: RoleGone, Removed: true, Handoff: p}}, now, &fx.sends)
		return
	}
	// 被拒：目标承诺以后不接受这个 epoch（seen 记录），本格以该 epoch 恢复为 Real，退避后用下一个 epoch 再试。
	n.handoffConcluded(cs, false, now, fx)
	prev := cs
	cs.Role, cs.Handoff = RoleReal, nil
	cs.RetryAfter = now.Add(n.cfg.RejectBackoff).UnixNano()
	n.setState(a, prev, cs) // 慢档的 NPC 回到慢档
	n.stats.handoffsRejected.Add(1)
	n.realChanged(cs.ID, cs.Ver.Epoch, true, fx)
	n.applyReports(r, []ActorReport{{State: cs.EntityState, Role: RoleReal}}, now, &fx.sends)
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
	st := req.State
	refuse := func(hint int32) {
		fx.send(from, &wireMsg{Kind: kindEnterRefused, EnterRefused: &enterRefused{ID: st.ID, Hint: hint}})
	}
	if st.ID == 0 {
		return nil
	}
	if a != nil {
		cs := a.CellState()
		switch cs.Role {
		case RoleReal:
			cs.Base = from
			cs.LeaseUntil = now.Add(n.cfg.LeaseTimeout).UnixNano()
			a.SetCellState(cs)
			fx.send(from, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: cs.Ver.Epoch, LastInput: cs.LastInput}})
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
	st.Ver = Version{Epoch: r.nextEpoch(st.ID, req.EpochHint), Seq: 1}
	n.install(r, created, ActorState{EntityState: st, LastInput: req.LastInput}, now, fx)
	return created.StartCell()
}

func (n *Node) onLeave(r *Region, a Actor, now time.Time, fx *txEffects) {
	if a == nil {
		return
	}
	cs := a.CellState()
	if cs.Role != RoleReal {
		return
	}
	var afx actorEffects
	n.leave(&cs, &afx, now.UnixNano())
	a.SetCellState(cs)
	n.realChanged(cs.ID, cs.Ver.Epoch, false, fx)
	fx.sends = append(fx.sends, afx.sends...)
	fx.destroys = append(fx.destroys, a)
	n.applyReports(r, []ActorReport{*afx.report}, now, &fx.sends)
}

// Spawn 在本格生成一个 NPC Real（epoch 取本格所知的下一个）：一个 Scene + NPC 的 Nest 事务，NPC 实体经 CreateActor 在
// 事务里新建。位置必须在本格内、ID 非零且本格没有这个实体。
func (n *Node) Spawn(ctx context.Context, st EntityState) error {
	if !n.hostsCell {
		return errors.New("cellworld: node hosts no cell")
	}
	if st.ID == 0 || !n.rect.Contains(st.Pos()) {
		return fmt.Errorf("cellworld: spawn %d at %v outside cell %v", st.ID, st.Pos(), n.cell)
	}
	// 已在本进程时 handler 看到它并拒绝；不在时 handler 拿到 nil、在事务里新建。
	_, err := n.cfg.Nest.RequestMulti(ctx, hSpawn, []int64{n.cfg.Scene, st.ID}, nest.Params{st})
	return err
}

func (n *Node) handleSpawn(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	st := params[0].(EntityState)
	if actorOf(es, st.ID) != nil {
		return nil, errors.New("cellworld: spawn: already exists")
	}
	a, err := n.cfg.CreateActor(st.ID, KindNPC)
	if err != nil {
		return nil, err
	}
	st.Kind, st.Base, st.Idle = KindNPC, 0, false // 新生成的 NPC 先快档，区域按附近有没有玩家再降
	st.Ver = Version{Epoch: r.nextEpoch(st.ID, 0), Seq: 1}
	fx := &txEffects{}
	now := time.Now()
	n.install(r, a, ActorState{EntityState: st}, now, fx)
	if err := a.StartCell(); err != nil {
		return nil, err
	}
	n.registerCommit(fx)
	return nil, nil
}

// ---- 心跳 ----

// ActorBeat 是角色 / NPC 的格内心跳回调：只锁角色本身，结果在提交后报给区域、发出 Offer / 回写 / Lost、销毁实体。
func (n *Node) ActorBeat(a Actor, now time.Time) error {
	n.stats.actorBeats.Add(1)
	st := a.CellState()
	if st.Role == RoleNone || st.Role == RoleGone {
		return nil
	}
	before := st.Role
	prev := st
	afx := n.stepActor(&st, now)
	n.setState(a, prev, st)
	fx := &txEffects{sends: afx.sends}
	if afx.report != nil {
		fx.reports = append(fx.reports, *afx.report)
	}
	if afx.destroy {
		fx.destroys = append(fx.destroys, a)
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
