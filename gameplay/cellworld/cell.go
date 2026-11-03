package cellworld

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// ErrCellConfig 表示 Cell 配置不合法。
var ErrCellConfig = errors.New("cellworld: invalid cell config")

// ErrCellStopped 表示 Cell 的事件循环没有在运行（尚未 Run 或已退出），控制请求无法执行。
var ErrCellStopped = errors.New("cellworld: cell not running")

// CellConfig 配置一个 Cell。零值字段取 withDefaults 里的缺省值。
type CellConfig struct {
	// World 是主题命名空间（同一世界的所有进程一致），只允许字母、数字、'-'、'_'。
	World string
	// Grid 是静态分区，所有进程必须一致。
	Grid *Grid
	// Cell 是本 Cell 负责的格；sid 由 Grid 映射得到。
	Cell CellID
	// Transport 是消息通道。
	Transport Transport
	// Period 是心跳周期，必须是 heartbeat.Periods 的档位；零值 50ms（20Hz，现行最短档）。
	Period time.Duration
	// ViewRadius 是玩家视野半径 R，必须为正且不大于 Grid 的 GhostBand（保证视野内的邻区实体都有 Ghost）。
	ViewRadius int64
	// AttackRange 是攻击距离；零值等于 ViewRadius，不能超过 ViewRadius。
	AttackRange int64
	// IndexBlock 是块索引的块边长；零值等于 ViewRadius。
	IndexBlock int64
	// MaxViewEntities 是一次视野推送的实体上限，超出按距离取最近的并标记 Truncated；零值 256。
	MaxViewEntities int
	// MaxReals 是本格 Real 数上限，超出时拒绝交接与进入；零值 50000。
	MaxReals int
	// MaxSpeed 是玩家输入速度每个分量的上限（单位 / 秒）；零值 2000。
	MaxSpeed int64
	// InboxSize 是收件箱容量（条消息）；满时丢弃并计数。零值 16384。
	InboxSize int
	// GhostChunk 是一条 Ghost 消息最多携带的更新 + 删除条数；零值 256。
	GhostChunk int
	// OfferRetry 是交接 Offer 的重传间隔；零值 100ms。
	OfferRetry time.Duration
	// HandoffAbort 是交接等结论的上限，超过即删除本地记录并通知 Base；零值 3s。
	HandoffAbort time.Duration
	// RejectBackoff 是交接被拒后再次尝试前的等待；零值 500ms。
	RejectBackoff time.Duration
	// GhostTTL：Ghost 超过它没收到更新就删除；零值 1s。
	GhostTTL time.Duration
	// SeenTTL 是交接 epoch 记录的保留时间，必须长于 HandoffAbort；零值 30s。
	SeenTTL time.Duration
	// AttackRetry 是跨格伤害请求的重发间隔；零值 100ms。
	AttackRetry time.Duration
	// AttackDeadline 是跨格伤害等结论的上限，过期向 Base 报 OutcomeUnknown；零值 2s。
	AttackDeadline time.Duration
	// DedupWindow 是目标上伤害去重记录的保留时间，必须长于 AttackDeadline；零值 10s。
	DedupWindow time.Duration
	// MaxAppliedHits 是单个目标去重窗口内最多记录的伤害数，满了拒绝新伤害（ReasonBusy）；零值 128。
	MaxAppliedHits int
	// Hooks 是测试与测量用的观察点，可为零值。
	Hooks Hooks
}

// Hooks 都在 Cell 的事件循环 goroutine 上同步调用，必须快速返回、不能阻塞、不能回调本 Cell 的控制方法。
type Hooks struct {
	// RealChanged 在本格取得（acquired=true）或失去某实体 Real 身份时调用；epoch 是取得时 / 失去前的 epoch。
	RealChanged func(cell CellID, id int64, epoch uint64, acquired bool)
	// HandoffDone 在旧格得到交接结论时调用：accepted 为 false 表示被拒后恢复；elapsed 从冻结起算。
	HandoffDone func(cell CellID, id int64, epoch uint64, accepted bool, elapsed time.Duration)
	// HandoffAborted 在旧格放弃交接（超时，删除本地记录）时调用。
	HandoffAborted func(cell CellID, id int64, epoch uint64)
	// GhostApplied 在应用一条 Ghost 消息后调用，latency 是发送方打时间戳到这里的系统时间差。
	GhostApplied func(cell CellID, latency time.Duration, entries int)
	// DamageApplied 在目标 Real 上真正扣血（不含重复请求）时调用。
	DamageApplied func(cell CellID, target int64, hit HitID, hp int32)
	// TickDone 在每拍处理结束时调用。
	TickDone func(cell CellID, elapsed time.Duration, reals, ghosts int)
}

func (cfg CellConfig) withDefaults() CellConfig {
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
	setInt(&cfg.InboxSize, 16384)
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
	return cfg
}

func (cfg CellConfig) validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrCellConfig}, args...)...)
	}
	switch {
	case !validWorld(cfg.World):
		return bad("world %q", cfg.World)
	case cfg.Grid == nil || !cfg.Grid.Contains(cfg.Cell):
		return bad("cell %v not in grid", cfg.Cell)
	case cfg.Transport == nil:
		return bad("nil transport")
	case !slices.Contains(heartbeat.Periods[:], cfg.Period):
		return bad("period %v is not a heartbeat period", cfg.Period)
	case cfg.ViewRadius <= 0 || cfg.ViewRadius > cfg.Grid.GhostBand():
		return bad("view radius %d must be in (0, ghost band %d]", cfg.ViewRadius, cfg.Grid.GhostBand())
	case cfg.AttackRange < 0 || cfg.AttackRange > cfg.ViewRadius:
		return bad("attack range %d must be in [0, view radius %d]", cfg.AttackRange, cfg.ViewRadius)
	case cfg.IndexBlock <= 0 || cfg.MaxViewEntities < 0 || cfg.MaxReals < 0 || cfg.MaxSpeed < 0 ||
		cfg.InboxSize < 0 || cfg.GhostChunk < 0 || cfg.MaxAppliedHits < 0:
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
	}
	return nil
}

// role 是实体在本格的身份。
type role uint8

const (
	roleReal   role = 1 // 唯一写权威
	roleFrozen role = 2 // 已发出交接、等结论；只读，不再写、不发 Ghost
	roleGhost  role = 3 // 邻格 Real 的只读镜像
)

func (r role) String() string {
	switch r {
	case roleReal:
		return "real"
	case roleFrozen:
		return "frozen"
	case roleGhost:
		return "ghost"
	}
	return "?"
}

// entity 是本格对一个实体的记录。所有字段只由事件循环 goroutine 读写。
type entity struct {
	st      EntityState
	role    role
	indexed bool
	idxPos  spatial.Point

	// Real：本格向哪些邻格发着 Ghost（上一拍）、已应用的 Base 输入序号、伤害去重表、被拒后的退避截止。
	ghostedTo  []CellID
	lastInput  uint64
	applied    []appliedHit
	retryAfter time.Time

	// Frozen：交接进行中。
	handoff *pendingHandoff

	// Ghost：Real 所在格与最近一次收到更新的时间。
	owner   CellID
	heardAt time.Time
}

type pendingHandoff struct {
	target    CellID
	epoch     uint64
	startedAt time.Time
	lastSent  time.Time
	payload   []byte // 编码好的 Offer；重传发同一份
}

// seenEpoch 记录本格见过的某实体最高交接 epoch 以及是否接受过它（含本格安装与本格冻结）。
type seenEpoch struct {
	epoch    uint64
	accepted bool
	at       time.Time
}

type tombstone struct {
	ver   Version
	until time.Time
}

type pendingAttack struct {
	base       int32
	hit        HitID
	target     int64
	amount     int32
	targetCell CellID
	deadline   time.Time
	nextSend   time.Time
}

// CellStats 是计数快照；都是累计值，Reals / Ghosts 是最近一拍结束时的数量。
type CellStats struct {
	Ticks, MsgsOut, BytesOut, MsgsIn, BytesIn       uint64
	InboxDrops, DecodeErrors, PublishErrors         uint64
	StaleDrops, Conflicts                           uint64
	HandoffsStarted, HandoffsAccepted               uint64
	HandoffsRejected, HandoffsAborted               uint64
	OfferRetransmits, OffersReceived, InputsDropped uint64
	DamageForwarded, DamageRetries, DamageUnknown   uint64
	Reals, Ghosts                                   uint64
}

type cellCounters struct {
	ticks, msgsOut, bytesOut, msgsIn, bytesIn       atomic.Uint64
	inboxDrops, decodeErrors, publishErrors         atomic.Uint64
	staleDrops, conflicts                           atomic.Uint64
	handoffsStarted, handoffsAccepted               atomic.Uint64
	handoffsRejected, handoffsAborted               atomic.Uint64
	offerRetransmits, offersReceived, inputsDropped atomic.Uint64
	damageForwarded, damageRetries, damageUnknown   atomic.Uint64
	reals, ghosts                                   atomic.Uint64
}

type inbound struct {
	msg *wireMsg
}

// Cell 运行一个格的空间模拟：持有本格 Real、邻格 Ghost，按心跳推进位置、发 Ghost、做交接、算玩家视野、
// 结算伤害。
//
// 并发与所有权：全部模拟状态只由 Run 的事件循环 goroutine 读写，没有锁。传输回调只解码，再非阻塞投进有界
// 收件箱（满了丢弃并计数）；控制请求（SpawnNPC、Inspect）经 control 通道投进循环执行。计数器是原子量，任何
// goroutine 都能读 Stats。
type Cell struct {
	cfg         CellConfig
	grid        *Grid
	id          CellID
	sid         int32
	rect        spatial.Rect
	indexBounds spatial.Rect
	index       *spatial.BlockIndex

	ents    map[int64]*entity
	seen    map[int64]seenEpoch
	tombs   map[int64]tombstone
	attacks map[HitID]*pendingAttack
	tick    uint64
	batches map[CellID]*ghostBatch
	scratch []CellID

	inbox   chan inbound
	control chan func()
	ready   chan struct{}
	running atomic.Bool
	stats   cellCounters
}

// NewCell 校验配置并构造 Cell；Run 之前不收消息。
func NewCell(cfg CellConfig) (*Cell, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	c := &Cell{
		cfg: cfg, grid: cfg.Grid, id: cfg.Cell, sid: cfg.Grid.SID(cfg.Cell),
		rect:    cfg.Grid.Rect(cfg.Cell),
		ents:    make(map[int64]*entity),
		seen:    make(map[int64]seenEpoch),
		tombs:   make(map[int64]tombstone),
		attacks: make(map[HitID]*pendingAttack),
		batches: make(map[CellID]*ghostBatch),
		inbox:   make(chan inbound, cfg.InboxSize),
		control: make(chan func()),
		ready:   make(chan struct{}),
	}
	c.indexBounds = expand(c.rect, cfg.Grid.GhostBand())
	index, err := spatial.NewBlockIndex(c.indexBounds, cfg.IndexBlock)
	if err != nil {
		return nil, fmt.Errorf("%w: index: %v", ErrCellConfig, err)
	}
	c.index = index
	return c, nil
}

// ID 返回本 Cell 负责的格；SID 返回它的进程 sid。
func (c *Cell) ID() CellID { return c.id }
func (c *Cell) SID() int32 { return c.sid }

// Ready 在 Run 完成订阅后关闭。
func (c *Cell) Ready() <-chan struct{} { return c.ready }

// Run 订阅本格收件主题并运行事件循环，直到 ctx 结束；返回前退订。一个 Cell 只能 Run 一次。
// 退出后状态不再推进（非持久，进程退出即丢失）。
func (c *Cell) Run(ctx context.Context) error {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("cellworld: cell already ran")
	}
	unsubscribe, err := c.cfg.Transport.Subscribe(Subject(c.cfg.World, c.sid), c.receive)
	if err != nil {
		return fmt.Errorf("cellworld: subscribe cell %v: %w", c.id, err)
	}
	defer func() { _ = unsubscribe() }()
	close(c.ready)
	ticker := time.NewTicker(c.cfg.Period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			c.step(now)
		case in := <-c.inbox:
			c.handle(in.msg, time.Now())
		case fn := <-c.control:
			fn()
		}
	}
}

// receive 是传输回调：解码后非阻塞投进收件箱。收件箱永不关闭，退订后迟到的回调也安全。
func (c *Cell) receive(data []byte) {
	c.stats.msgsIn.Add(1)
	c.stats.bytesIn.Add(uint64(len(data)))
	m, err := decodeMsg(data)
	if err != nil {
		c.stats.decodeErrors.Add(1)
		return
	}
	select {
	case c.inbox <- inbound{msg: m}:
	default:
		c.stats.inboxDrops.Add(1)
	}
}

// do 把 fn 投进事件循环执行并等它完成。只能从循环之外的 goroutine 调用。
func (c *Cell) do(ctx context.Context, fn func()) error {
	done := make(chan struct{})
	select {
	case c.control <- func() { fn(); close(done) }:
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrCellStopped, ctx.Err())
	}
	<-done
	return nil
}

// SpawnNPC 在本格创建一个 NPC Real（epoch 取本格所知的下一个）。位置必须在本格内、ID 非零且本格不认识它。
func (c *Cell) SpawnNPC(ctx context.Context, st EntityState) error {
	var err error
	if doErr := c.do(ctx, func() { err = c.spawn(st, time.Now()) }); doErr != nil {
		return doErr
	}
	return err
}

func (c *Cell) spawn(st EntityState, now time.Time) error {
	if st.ID == 0 || !c.rect.Contains(st.Pos()) {
		return fmt.Errorf("cellworld: spawn %d at %v outside cell %v", st.ID, st.Pos(), c.id)
	}
	if _, exists := c.ents[st.ID]; exists {
		return fmt.Errorf("cellworld: spawn %d: already known", st.ID)
	}
	st.Kind = KindNPC
	st.Base = 0
	st.Ver = Version{Epoch: c.nextEpoch(st.ID, 0), Seq: 1}
	c.installReal(st, now, 0, nil, nil)
	return nil
}

// EntitySnapshot 是 Inspect 返回的一条记录。
type EntitySnapshot struct {
	State EntityState
	Role  string
	Owner CellID // Ghost 的来源格 / Frozen 的交接目标
}

// Inspect 返回本格全部实体的快照（测试与诊断用）。
func (c *Cell) Inspect(ctx context.Context) ([]EntitySnapshot, error) {
	var out []EntitySnapshot
	err := c.do(ctx, func() { out = c.snapshot() })
	return out, err
}

func (c *Cell) snapshot() []EntitySnapshot {
	out := make([]EntitySnapshot, 0, len(c.ents))
	for _, e := range c.ents {
		s := EntitySnapshot{State: e.st, Role: e.role.String(), Owner: e.owner}
		if e.handoff != nil {
			s.Owner = e.handoff.target
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].State.ID < out[j].State.ID })
	return out
}

// Stats 返回计数快照，任何 goroutine 可调用。
func (c *Cell) Stats() CellStats {
	s := &c.stats
	return CellStats{
		Ticks: s.ticks.Load(), MsgsOut: s.msgsOut.Load(), BytesOut: s.bytesOut.Load(), MsgsIn: s.msgsIn.Load(), BytesIn: s.bytesIn.Load(),
		InboxDrops: s.inboxDrops.Load(), DecodeErrors: s.decodeErrors.Load(), PublishErrors: s.publishErrors.Load(),
		StaleDrops: s.staleDrops.Load(), Conflicts: s.conflicts.Load(),
		HandoffsStarted: s.handoffsStarted.Load(), HandoffsAccepted: s.handoffsAccepted.Load(),
		HandoffsRejected: s.handoffsRejected.Load(), HandoffsAborted: s.handoffsAborted.Load(),
		OfferRetransmits: s.offerRetransmits.Load(), OffersReceived: s.offersReceived.Load(), InputsDropped: s.inputsDropped.Load(),
		DamageForwarded: s.damageForwarded.Load(), DamageRetries: s.damageRetries.Load(), DamageUnknown: s.damageUnknown.Load(),
		Reals: s.reals.Load(), Ghosts: s.ghosts.Load(),
	}
}

// handle 分发一条消息。只在事件循环上调用（单元测试直接调用它驱动状态机）。
func (c *Cell) handle(m *wireMsg, now time.Time) {
	switch m.Kind {
	case kindGhost:
		c.onGhostBatch(m.From, m.SentAt, m.Ghost, now)
	case kindOffer:
		c.onOffer(m.From, m.Offer, now)
	case kindOfferReply:
		c.onOfferReply(m.From, m.OfferReply, now)
	case kindDamage:
		c.onDamage(m.From, m.Damage, now)
	case kindDamageReply:
		c.onDamageReply(m.From, m.DamageReply, now)
	case kindEnter:
		c.onEnter(m.From, m.Enter, now)
	case kindLeave:
		c.onLeave(m.Leave)
	case kindInput:
		c.onInput(m.Input)
	case kindAttack:
		c.onAttack(m.From, m.Attack, now)
	default:
		c.stats.decodeErrors.Add(1) // 发给 Base 的消息类型不该出现在 Cell
	}
}

// send 编码并发布到 sid 的收件主题。发布失败只计数：协议本来就容忍丢失。
func (c *Cell) send(sid int32, m *wireMsg) {
	m.From = c.sid
	data, err := encodeMsg(m)
	if err != nil {
		c.stats.publishErrors.Add(1)
		return
	}
	c.sendRaw(sid, data)
}

func (c *Cell) sendRaw(sid int32, data []byte) {
	if err := c.cfg.Transport.Publish(Subject(c.cfg.World, sid), data); err != nil {
		c.stats.publishErrors.Add(1)
		return
	}
	c.stats.msgsOut.Add(1)
	c.stats.bytesOut.Add(uint64(len(data)))
}

// step 推进一拍：积分位置 → 发起交接 → 交接重传 / 放弃 → 发 Ghost → 过期清理 → 跨格伤害重试 → 玩家视野。
func (c *Cell) step(now time.Time) {
	start := time.Now()
	c.tick++
	c.stats.ticks.Add(1)
	dt := c.cfg.Period
	var reals, ghosts int
	for _, e := range c.ents {
		switch e.role {
		case roleReal:
			reals++
			c.integrate(e, dt)
			if target, ok := c.grid.ShouldHandoff(c.id, e.st.Pos()); ok && !now.Before(e.retryAfter) {
				c.beginHandoff(e, target, now)
				continue
			}
			e.st.Ver.Seq++
			c.publishGhost(e)
		case roleFrozen:
			c.retransmitOrAbort(e, now)
		case roleGhost:
			ghosts++
			if now.Sub(e.heardAt) > c.cfg.GhostTTL {
				c.removeEntity(e)
				ghosts--
			}
		}
	}
	c.flushGhosts(now)
	c.expire(now)
	c.retryAttacks(now)
	c.publishViews()
	c.stats.reals.Store(uint64(reals))
	c.stats.ghosts.Store(uint64(ghosts))
	if c.cfg.Hooks.TickDone != nil {
		c.cfg.Hooks.TickDone(c.id, time.Since(start), reals, ghosts)
	}
}

// integrate 按固定步长积分位置，夹在世界内；NPC 碰到世界边缘反弹，玩家停在边缘。
func (c *Cell) integrate(e *entity, dt time.Duration) {
	if e.st.VX == 0 && e.st.VY == 0 {
		return
	}
	bounds := c.grid.Bounds()
	nx := e.st.X + e.st.VX*int64(dt)/int64(time.Second)
	ny := e.st.Y + e.st.VY*int64(dt)/int64(time.Second)
	clamp := func(v, lo, hi int64, vel *int64) int64 {
		switch {
		case v < lo:
			v = lo
		case v > hi:
			v = hi
		default:
			return v
		}
		if e.st.Kind == KindNPC {
			*vel = -*vel
		} else {
			*vel = 0
		}
		return v
	}
	nx = clamp(nx, bounds.Min.X, bounds.Max.X-1, &e.st.VX)
	ny = clamp(ny, bounds.Min.Y, bounds.Max.Y-1, &e.st.VY)
	e.st.X, e.st.Y = nx, ny
	c.reindex(e)
}

// reindex 让块索引跟上实体位置；索引范围是本格外扩 G，范围外的实体不进索引（看不见）。
func (c *Cell) reindex(e *entity) {
	p := e.st.Pos()
	in := c.indexBounds.Contains(p)
	switch {
	case e.indexed && in:
		if p != e.idxPos {
			c.index.Move(e.st.ID, e.idxPos, p)
			e.idxPos = p
		}
	case e.indexed:
		c.index.Remove(e.st.ID, e.idxPos)
		e.indexed = false
	case in:
		c.index.Add(e.st.ID, p)
		e.indexed, e.idxPos = true, p
	}
}

func (c *Cell) removeEntity(e *entity) {
	if e.indexed {
		c.index.Remove(e.st.ID, e.idxPos)
	}
	delete(c.ents, e.st.ID)
}

// nextEpoch 返回本格为新 Real（进入 / 生成）取的 epoch：比本格见过的一切都大，且不小于 hint。
func (c *Cell) nextEpoch(id int64, hint uint64) uint64 {
	next := max(hint, 1)
	if s, ok := c.seen[id]; ok {
		next = max(next, s.epoch+1)
	}
	if e, ok := c.ents[id]; ok {
		next = max(next, e.st.Ver.Epoch+1)
	}
	if t, ok := c.tombs[id]; ok {
		next = max(next, t.ver.Epoch+1)
	}
	return next
}

// installReal 让本格成为 st.ID 的 Real（epoch = st.Ver.Epoch），替换本地 Ghost（如有），登记 seen、通知玩家的 Base。
func (c *Cell) installReal(st EntityState, now time.Time, lastInput uint64, applied []appliedHit, ghostedTo []CellID) {
	e := c.ents[st.ID]
	if e == nil {
		e = &entity{}
		c.ents[st.ID] = e
	}
	*e = entity{st: st, role: roleReal, indexed: e.indexed, idxPos: e.idxPos, lastInput: lastInput, applied: applied, ghostedTo: ghostedTo}
	c.reindex(e)
	c.seen[st.ID] = seenEpoch{epoch: st.Ver.Epoch, accepted: true, at: now}
	delete(c.tombs, st.ID)
	c.realChanged(e, true)
	if st.Kind == KindPlayer && st.Base != 0 {
		c.send(st.Base, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: st.Ver.Epoch, LastInput: lastInput}})
	}
}

func (c *Cell) realChanged(e *entity, acquired bool) {
	if c.cfg.Hooks.RealChanged != nil {
		c.cfg.Hooks.RealChanged(c.id, e.st.ID, e.st.Ver.Epoch, acquired)
	}
}

// expire 清理过期的墓碑与 seen 记录（都有上限：只保留 TTL 内碰到过的实体）。
func (c *Cell) expire(now time.Time) {
	for id, t := range c.tombs {
		if now.After(t.until) {
			delete(c.tombs, id)
		}
	}
	for id, s := range c.seen {
		if now.Sub(s.at) > c.cfg.SeenTTL {
			if e, ok := c.ents[id]; ok && e.role != roleGhost {
				continue // 本格仍是 Real / Frozen 时保留
			}
			delete(c.seen, id)
		}
	}
}

// onEnter：Base 让玩家进入（首次、Cell 崩溃后重进、交接放弃后补回）。本格已是 Real 时幂等返回 owner；
// 本格知道 Real 在别处（Frozen 或存活 Ghost）时拒绝并提示 owner，避免造出第二个 Real。
func (c *Cell) onEnter(from int32, req *enterReq, now time.Time) {
	st := req.State
	refuse := func(hint int32) {
		c.send(from, &wireMsg{Kind: kindEnterRefused, EnterRefused: &enterRefused{ID: st.ID, Hint: hint}})
	}
	if st.ID == 0 {
		return
	}
	if e, ok := c.ents[st.ID]; ok {
		switch {
		case e.role == roleReal:
			e.st.Base = from
			c.send(from, &wireMsg{Kind: kindOwner, Owner: &ownerNotice{ID: st.ID, Epoch: e.st.Ver.Epoch, LastInput: e.lastInput}})
			return
		case e.role == roleFrozen:
			refuse(c.grid.SID(e.handoff.target))
			return
		case now.Sub(e.heardAt) <= c.cfg.GhostTTL:
			refuse(c.grid.SID(e.owner))
			return
		}
	}
	if !c.rect.Contains(st.Pos()) {
		target, _ := c.grid.CellAt(st.Pos())
		refuse(c.grid.SID(target))
		return
	}
	if c.realCount() >= c.cfg.MaxReals {
		refuse(0)
		return
	}
	st.Kind, st.Base = KindPlayer, from
	st.VX, st.VY = 0, 0
	st.Ver = Version{Epoch: c.nextEpoch(st.ID, req.EpochHint), Seq: 1}
	c.installReal(st, now, req.LastInput, nil, nil)
}

func (c *Cell) onLeave(req *leaveReq) {
	e, ok := c.ents[req.ID]
	if !ok || e.role != roleReal {
		return
	}
	for _, target := range e.ghostedTo {
		c.batchFor(target).Removes = append(c.batchFor(target).Removes, ghostRemove{ID: e.st.ID, Ver: Version{Epoch: e.st.Ver.Epoch, Seq: e.st.Ver.Seq + 1}})
	}
	c.realChanged(e, false)
	c.removeEntity(e)
}

// onInput 只在本格是 Real 时应用；序号不前进的输入（重复、乱序）丢弃。非 Real 时丢弃——Base 从新 owner 的
// 视野里看到 LastInput 落后会重发最新意图。
func (c *Cell) onInput(req *inputReq) {
	e, ok := c.ents[req.ID]
	if !ok || e.role != roleReal || req.Seq <= e.lastInput {
		c.stats.inputsDropped.Add(1)
		return
	}
	clampSpeed := func(v int64) int64 { return max(-c.cfg.MaxSpeed, min(c.cfg.MaxSpeed, v)) }
	e.st.VX, e.st.VY = clampSpeed(req.VX), clampSpeed(req.VY)
	e.lastInput = req.Seq
}

func (c *Cell) realCount() int {
	n := 0
	for _, e := range c.ents {
		if e.role == roleReal || e.role == roleFrozen {
			n++
		}
	}
	return n
}

// publishViews 给本格每个玩家 Real 推一份视野：半径 R 内的实体（Real / Frozen / Ghost，含自己）。
func (c *Cell) publishViews() {
	for _, e := range c.ents {
		if e.role != roleReal || e.st.Kind != KindPlayer || e.st.Base == 0 {
			continue
		}
		center := e.st.Pos()
		var view []ViewEntity
		var dist []int64
		for _, id := range c.index.QueryRadius(center, c.cfg.ViewRadius) {
			x, ok := c.ents[id]
			if !ok || !spatial.WithinDistance(center, x.st.Pos(), c.cfg.ViewRadius) {
				continue
			}
			view = append(view, ViewEntity{ID: id, X: x.st.X, Y: x.st.Y, HP: x.st.HP, Ghost: x.role == roleGhost})
			dist = append(dist, spatial.DistanceSquared(center, x.st.Pos()))
		}
		truncated := false
		if len(view) > c.cfg.MaxViewEntities {
			order := make([]int, len(view))
			for i := range order {
				order[i] = i
			}
			sort.Slice(order, func(i, j int) bool { return dist[order[i]] < dist[order[j]] })
			kept := make([]ViewEntity, c.cfg.MaxViewEntities)
			for i := range kept {
				kept[i] = view[order[i]]
			}
			view, truncated = kept, true
		}
		c.send(e.st.Base, &wireMsg{Kind: kindView, View: &viewMsg{
			Observer: e.st.ID, Epoch: e.st.Ver.Epoch, Tick: c.tick, LastInput: e.lastInput,
			Self: e.st, Entities: view, Truncated: truncated,
		}})
	}
}
