package cellworld

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 测试用实体：Scene（持有 Region，scene.cell 心跳）、SceneActor（玩家在格里的部分）、NPC、Player（Base 侧）。
// 它们是 game 生成实体的手写替身：状态放在手写 DAO 里，写时向当前 Nest 事务登记逆操作（与生成 setter 同形）。
// kind 号只在本测试二进制内唯一（本包测试没有别的 kind）。
const (
	kindTScene  fentity.EntityKind = 211
	kindTActor  fentity.EntityKind = 212
	kindTNPC    fentity.EntityKind = 213
	kindTPlayer fentity.EntityKind = 214
)

func init() {
	fentity.MustRegisterEntityKindDefs(
		fentity.EntityKindDef{Kind: kindTScene, Category: fentity.EntityCategoryWorld, BusinessPool: fentity.BusinessPoolShort},
		fentity.EntityKindDef{Kind: kindTPlayer, Category: fentity.EntityCategoryPlayer, BusinessPool: fentity.BusinessPoolShort},
	)
	for _, kind := range []fentity.EntityKind{kindTActor, kindTNPC} {
		fentity.RegisterEntityBuilder(&fentity.EntityBuilderParam{
			Category: fentity.EntityCategoryOther, Kind: kind, NoPersist: true, BusinessPool: fentity.BusinessPoolShort,
			Builder: buildTestActor,
		})
	}
}

// ---- 真实时钟下的等待（2026-11-10 统一整治，见 docs/maintenance/evidence/cellworld-clock-cleanup-20261110）----
//
// 本包的集成用例跑在真实时钟上：心跳驱动按系统时间醒来、迟到的拍不补跑，消息经真实 goroutine 投递。所以用例里
//   - 等异步结论一律“等结论成立、有期限、超时打诊断”（eventually / waitFor / waitView / waitSettled），不写“sleep 固定
//     时长后立即断言”；
//   - 需要“过几拍”的窗口（看一件事在这段时间里没有发生），以格实际执行的心跳拍数计（world.waitBeats），不以墙钟计；
//   - 断言间隔 / 频率的，以实际执行的心跳拍数为尺子（checkContinuity 的视野间隔、升档后的拍数），不以墙钟毫秒为尺子：
//     负载下心跳本身被推迟时墙钟间隔变长、拍数不变；实现真的漏发、迟发时拍数变大。
//
// 期限集中在下面两个常量；个别用例自己的配置期限更长时另取，写明理由。
const (
	// settleWithin：等一个异步结论（交接收尾、伤害 / 面板结论、角色 / Ghost 出现或消失、客户端帧到达）的统一期限。长于
	// 缺省 HandoffAbort（3s）、LeaseTimeout（3s）、AttackDeadline（2s）、PanelDeadline（2s）：协议在这些期限内一定给出
	// 结论（成功或放弃），过了 settleWithin 还没有就是真的卡住，按诊断失败，不是“负载下慢了一点”。
	settleWithin = 5 * time.Second
	// travelWithin：等玩家走到某处（最长一段约 2600 单位、1000/s 约 2.6s，途中交接）的期限。
	travelWithin = 10 * time.Second
)

// eventually 轮询 check 直到成立；超过 within 按最后一次检查的 diag 失败。只等结论，不断言耗时。
func eventually(t testing.TB, within time.Duration, what string, check func() (ok bool, diag string)) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok, diag := check()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s: %s", within, what, diag)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

func mustID(t testing.TB, unique int64, kind fentity.EntityKind) int64 {
	t.Helper()
	id, err := fentity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func actorID(unique int64) int64 {
	id, _ := fentity.BuildEntityID(unique, kindTActor)
	return id
}

func npcID(unique int64) int64 {
	id, _ := fentity.BuildEntityID(unique, kindTNPC)
	return id
}

func playerEntityID(unique int64) int64 {
	id, _ := fentity.BuildEntityID(unique, kindTPlayer)
	return id
}

// undoCell 是只有一个字段的手写 DAO：set 在 rollback=undo 的事务里登记“还原成事务开始时的值”。
type undoCell[T any] struct{ v T }

func (d *undoCell[T]) set(v T) {
	old := d.v
	nest.RecordUndo(d, 1, func() error { d.v = old; return nil })
	d.v = v
}

type testActor struct {
	*fentity.EntityBase
	dao  *undoCell[ActorState]
	host *heartbeat.Host
	reg  *heartbeat.Reg
}

func (a *testActor) Base() *fentity.EntityBase             { return a.EntityBase }
func (a *testActor) CellState() ActorState                 { return a.dao.v }
func (a *testActor) SetCellState(st ActorState)            { a.dao.set(st) }
func (a *testActor) StartCell() error                      { return a.reg.Start() }
func (a *testActor) SetCellPeriod(d time.Duration) error   { return a.reg.SetPeriod(d) }
func (a *testActor) OnDestroy(fentity.EntityDestroyReason) { a.host.OnDestroy() }

// buildTestActor 是 SceneActor / NPC 的构造函数（EntityManager.Create 调用；handler 内即 CreateInScope）。Param 是它所在的“进程”。
func buildTestActor(p *fentity.EntityCreateParam) (fentity.IThreadSafeEntity, error) {
	proc, ok := p.Param.(*testProc)
	if !ok {
		return nil, fmt.Errorf("test actor %d: no process", p.Id)
	}
	a := &testActor{EntityBase: fentity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind), dao: &undoCell[ActorState]{}}
	a.host = proc.driver.NewHost(a)
	reg, err := a.host.Register("cell.actor", proc.node.Config().Period, func(beat heartbeat.Beat) error {
		return proc.node.ActorBeat(a, time.Now(), beat.Elapsed)
	})
	if err != nil {
		return nil, err
	}
	a.reg = reg
	return a, nil
}

type testScene struct {
	*fentity.EntityBase
	region *Region
	host   *heartbeat.Host
}

func (s *testScene) Base() *fentity.EntityBase             { return s.EntityBase }
func (s *testScene) CellRegion() *Region                   { return s.region }
func (s *testScene) OnDestroy(fentity.EntityDestroyReason) { s.host.OnDestroy() }

type testPlayer struct {
	*fentity.EntityBase
	dao   *undoCell[BaseState]
	group *undoCell[Group] // 队伍面板：Player 上缓存的成员表（game 里是 PlayerDao 的字段）
	host  *heartbeat.Host
	reg   *heartbeat.Reg
	// panel 是“养成输入推出的面板”的替身（game 里是属性的 Final 层）：测试 goroutine 写、Player 的 handler 里读。
	panelMu sync.Mutex
	panel   Panel
}

// setCellPanel 改替身的面板来源（不发任何东西：之后 Enter 带它，或 RefreshPanel 让 Base 读它）。
func (p *testPlayer) setCellPanel(panel Panel) {
	p.panelMu.Lock()
	p.panel = panel
	p.panelMu.Unlock()
}

func (p *testPlayer) CellPanel() Panel {
	p.panelMu.Lock()
	defer p.panelMu.Unlock()
	return p.panel
}

func (p *testPlayer) Base() *fentity.EntityBase             { return p.EntityBase }
func (p *testPlayer) BaseState() BaseState                  { return p.dao.v }
func (p *testPlayer) SetBaseState(st BaseState)             { p.dao.set(st) }
func (p *testPlayer) StartBase() error                      { return p.reg.Start() }
func (p *testPlayer) CellGroup() Group                      { return p.group.v }
func (p *testPlayer) SetCellGroup(g Group)                  { p.group.set(g) }
func (p *testPlayer) OnDestroy(fentity.EntityDestroyReason) { p.host.OnDestroy() }

var hTestGroup = nest.NewHandlerName("cellworld_test_group")

// testGroupCmd：set 时把玩家的成员表写成 group；notice 非空时同一笔事务提交后把它通知给它的成员与 former 所在的 Base
// （game 里离队的 handler 是 set 空表 + 通知新表）。都不给时只读成员表。
type testGroupCmd struct {
	set    bool
	group  Group
	notice *Group
	former []GroupMember
}

// testProc 是一个“进程”：自己的 EntityManager、Nest 引擎、心跳驱动与 Node。Cell 进程有一个 Scene；Base 进程有
// entitysync Manager + Interest + Mirrors（客户端是 clientModel）。
type testProc struct {
	t       testing.TB
	sid     int32
	manager *fentity.EntityManager
	access  *fentity.ManagerAccess
	engine  *nest.NestMgr
	driver  *heartbeat.Driver
	node    *Node
	loader  *dataEngineShapedLoader // strict 时的加载器（ManagerAccess 不该为不持久种类问它）
	scene   *testScene

	sync     *entitysync.Manager
	interest *policy.Interest
	mirrors  *policy.Mirrors[EntityState]
	client   *clientModel

	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
	// baseBeats：本进程执行过的 cell.base 心跳数（各玩家合计）：Base 侧“过几拍”的尺子。
	baseBeats atomic.Uint64
}

// procSpec 描述要起的进程。
type procSpec struct {
	sid       int32
	cell      *CellID
	base      bool
	grid      *Grid
	transport Transport
	mod       func(*NodeConfig)
	// strict：getter 是装了加载器的 ManagerAccess（正式进程的形状）；加载器像 DataEngine 的 Repository 一样对不持久的
	// 种类答“聚合不存在”的错误。框架约定 ManagerAccess 对不持久种类不问加载器（E1），所以加载器一次都不该被调用。
	strict bool
}

// dataEngineShapedLoader 是 DataEngine Repository 的替身：harness 的实体种类都是 noPersist，它对它们只会报错。
type dataEngineShapedLoader struct{ asked atomic.Int64 }

var errNoAggregate = errors.New("test loader: entity aggregate not found")

func (l *dataEngineShapedLoader) LoadEntity(_ context.Context, id int64, kind fentity.EntityKind) (fentity.IThreadSafeEntity, error) {
	l.asked.Add(1)
	return nil, fmt.Errorf("%w: entity %d kind %d has no persistent DAO", errNoAggregate, id, kind)
}

func startProc(t testing.TB, spec procSpec) *testProc {
	t.Helper()
	p := &testProc{t: t, sid: spec.sid, manager: fentity.NewEntityManager()}
	p.access = fentity.NewManagerAccess(p.manager)
	if spec.strict {
		p.loader = &dataEngineShapedLoader{}
		if _, err := p.access.ConfigureLoader(p.loader); err != nil {
			t.Fatal(err)
		}
	}
	p.engine = nest.NewEngine(nest.NestOptionWithGetter(p.access), nest.NestOptionWithTransactionCommitter(nullCommitter{}))
	cfg := NodeConfig{
		World: "w", SID: spec.sid, Grid: spec.grid, Transport: spec.transport, Nest: p.engine,
		Destroy: func(ctx context.Context, e fentity.IThreadSafeEntity) error {
			return p.manager.Destroy(ctx, e, fentity.DestroyReasonCommon, false)
		},
		ViewRadius: worldViewRadius,
	}
	if spec.cell != nil {
		cfg.Cell = spec.cell
		cfg.Scene = mustID(t, int64(spec.sid), kindTScene)
		cfg.CreateActor = func(id int64, _ EntityKind) (Actor, error) {
			kind := fentity.GetEntityKindFromID(id)
			e, err := p.access.Create(&fentity.EntityCreateParam{IsCreate: true, Id: id, Kind: kind, Category: fentity.EntityCategoryOther, Param: p})
			if err != nil {
				return nil, err
			}
			return e.(*testActor), nil
		}
	}
	if spec.base {
		p.startSync(t, spec.grid)
		cfg.PlayerEntity = func(actor int64) int64 { return playerEntityID(fentity.GetUniqueIDFromEntityID(actor)) }
		cfg.Mirrors, cfg.Interest = p.mirrors, p.interest
		cfg.Session = func(actor int64) int64 { return fentity.GetUniqueIDFromEntityID(actor) }
		cfg.MirrorKind = func(s EntityState) (string, uint32) {
			if s.Kind == KindNPC {
				return "npc", uint32(kindTNPC)
			}
			return "actor", uint32(kindTActor)
		}
	}
	if spec.mod != nil {
		spec.mod(&cfg)
	}
	// 正式进程的顺序（E3）：handler 名单在引擎启动前注册到引擎实例，Node 在引擎启动之后才建、经 NodeConfig.Handlers 接上。
	handlers := NewHandlers()
	if err := handlers.Register(p.engine); err != nil {
		t.Fatal(err)
	}
	// 测试用的“业务 handler”：在 Player 的事务里改成员表（game 里是 [Team, Player] 的组队 handler）。
	if err := p.engine.RegisterHandlerWithMeta(hTestGroup, func(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		pl, ok := es[0].(*testPlayer)
		if !ok || pl == nil {
			return nil, errors.New("test group: no player")
		}
		cmd := params[0].(testGroupCmd)
		if cmd.set {
			p.node.SetGroup(pl, cmd.group)
		}
		if cmd.notice != nil {
			p.node.NotifyGroup(*cmd.notice, cmd.former)
		}
		return pl.CellGroup(), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync}); err != nil {
		t.Fatal(err)
	}
	if err := p.engine.Start(); err != nil {
		t.Fatal(err)
	}
	cfg.Handlers = handlers
	node, err := NewNode(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.node = node
	p.driver = heartbeat.NewDriver(heartbeat.Config{Name: fmt.Sprintf("cell-%d", spec.sid)})
	if err := p.driver.Bind(p.engine, func(id int64) fentity.IThreadSafeEntity { return p.manager.Get(id) }); err != nil {
		t.Fatal(err)
	}
	if spec.cell != nil {
		s := &testScene{EntityBase: fentity.NewEntityBase(cfg.Scene, fentity.EntityCategoryWorld, true, kindTScene), region: NewRegion()}
		s.host = p.driver.NewHost(s)
		reg, err := s.host.Register("scene.cell", node.Config().Period, func(heartbeat.Beat) error { return node.RegionBeat(s, time.Now()) })
		if err != nil {
			t.Fatal(err)
		}
		p.manager.Add(s)
		if err := reg.Start(); err != nil {
			t.Fatal(err)
		}
		p.scene = s
	}
	if err := p.driver.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() { defer close(p.done); _ = node.Run(ctx) }()
	<-node.Ready()
	return p
}

// startSync 建 Base 进程的 Sync：Manager（客户端是 clientModel）、Interest（AOI 半径小于 witness 半径）、Mirrors。
func (p *testProc) startSync(t testing.TB, grid *Grid) {
	p.client = newClientModel()
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: p.client, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	in, err := policy.NewInterest(policy.InterestConfig{Manager: m, AOI: policy.AOIConfig{
		Bounds: grid.Bounds(), BlockSize: baseBlock, EnterRadius: baseEnterRadius, LeaveRadius: baseLeaveRadius,
	}})
	if err != nil {
		t.Fatal(err)
	}
	mirrors, err := policy.NewMirrors(policy.MirrorConfig[EntityState]{
		Manager: m, Interest: in, TTL: time.Second,
		Position: func(s EntityState) spatial.Point { return s.Pos() },
		Snapshot: func(s EntityState, _ fentity.SyncProfile) (fentity.FrozenSyncPayload, error) {
			data, err := msgpack.Marshal(s)
			return fentity.TakeFrozenSyncPayload(9, data), err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.sync, p.interest, p.mirrors = m, in, mirrors
}

// stop 模拟进程停止（崩溃）：出站与订阅停下、心跳与引擎关闭，内存状态全部丢掉。
func (p *testProc) stop() {
	if p.stopped {
		return
	}
	p.stopped = true
	p.cancel()
	<-p.done
	_ = p.driver.Stop(context.Background())
	_ = p.engine.Shutdown(context.Background())
	if p.sync != nil {
		p.mirrors.Close()
		p.interest.Close()
		_ = p.sync.Close(context.Background())
	}
}

// waitBaseBeats 等本进程再执行 n 拍 cell.base 心跳（各玩家合计）：Base 侧“过几拍再看”的窗口，以拍数计。
func (p *testProc) waitBaseBeats(n int) {
	p.t.Helper()
	target := p.baseBeats.Load() + uint64(n)
	eventually(p.t, settleWithin+time.Duration(n)*p.node.Config().WatchPeriod, fmt.Sprintf("%d base beats on sid %d", n, p.sid), func() (bool, string) {
		got := p.baseBeats.Load()
		return got >= target, fmt.Sprintf("%d beats short", target-min(got, target))
	})
}

// player 返回（必要时新建）Base 进程上的 Player 实体，并打开它的同步会话。
func (p *testProc) player(unique int64) *testPlayer {
	id := playerEntityID(unique)
	if e := p.manager.Get(id); e != nil {
		return e.(*testPlayer)
	}
	pl := &testPlayer{EntityBase: fentity.NewEntityBase(id, fentity.EntityCategoryPlayer, true, kindTPlayer), dao: &undoCell[BaseState]{}, group: &undoCell[Group]{}}
	pl.host = p.driver.NewHost(pl)
	reg, err := pl.host.Register("cell.base", p.node.Config().WatchPeriod, func(heartbeat.Beat) error {
		p.baseBeats.Add(1)
		return p.node.BaseBeat(pl, time.Now())
	})
	if err != nil {
		p.t.Fatal(err)
	}
	pl.reg = reg
	p.manager.Add(pl)
	if err := p.sync.OpenSession(entitysync.SessionID(unique)); err != nil {
		p.t.Fatal(err)
	}
	return pl
}

// clientModel 是 Base 进程的客户端替身：按会话解帧，记 ref → subject、当前持有的对象，以及每个对象被 create / remove 的次数。
type clientModel struct {
	mu      sync.Mutex
	refs    map[entitysync.SessionID]map[frame.ObjectRef]int64
	holds   map[entitysync.SessionID]map[int64]EntityState
	creates map[entitysync.SessionID]map[int64]int
	removes map[entitysync.SessionID][]clientRemove
	frames  int
}

type clientRemove struct {
	id int64
	at time.Time
}

func newClientModel() *clientModel {
	return &clientModel{
		refs: map[entitysync.SessionID]map[frame.ObjectRef]int64{}, holds: map[entitysync.SessionID]map[int64]EntityState{},
		creates: map[entitysync.SessionID]map[int64]int{}, removes: map[entitysync.SessionID][]clientRemove{},
	}
}

func (c *clientModel) Push(_ context.Context, session entitysync.SessionID, data []byte) error {
	decoded, err := entitysync.DecodeFrame(data, frame.DefaultLimits())
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames++
	if c.refs[session] == nil {
		c.refs[session], c.holds[session], c.creates[session] = map[frame.ObjectRef]int64{}, map[int64]EntityState{}, map[int64]int{}
	}
	for _, object := range decoded.Objects {
		subject := c.refs[session][object.Ref]
		var state EntityState
		for _, component := range object.Components {
			update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
			if err != nil {
				return err
			}
			subject = update.SubjectID
			if err := msgpack.Unmarshal(update.Payload.BytesCopy(), &state); err != nil {
				return err
			}
		}
		switch object.Operation {
		case frame.ObjectCreate:
			c.refs[session][object.Ref] = subject
			c.holds[session][subject] = state
			c.creates[session][subject]++
		case frame.ObjectUpdate:
			c.holds[session][subject] = state
		case frame.ObjectRemove:
			delete(c.refs[session], object.Ref)
			delete(c.holds[session], subject)
			c.removes[session] = append(c.removes[session], clientRemove{id: subject, at: time.Now()})
		}
	}
	return nil
}

func (c *clientModel) held(session int64) map[int64]EntityState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[int64]EntityState{}
	for id, s := range c.holds[entitysync.SessionID(session)] {
		out[id] = s
	}
	return out
}

func (c *clientModel) removed(session int64) []clientRemove {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clientRemove(nil), c.removes[entitysync.SessionID(session)]...)
}

func (c *clientModel) createCount(session, id int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates[entitysync.SessionID(session)][id]
}

// realChecker 在每次 RealChanged（提交后）时检查：同一实体同一时刻至多一个 Real，同一 (实体, epoch) 只被一个格取得，
// 每个实体被取得的 epoch 严格递增。
type realChecker struct {
	t        testing.TB
	mu       sync.Mutex
	holders  map[int64]map[CellID]uint64
	acquired map[[2]uint64]CellID
	lastEp   map[int64]uint64
}

func newRealChecker(t testing.TB) *realChecker {
	return &realChecker{t: t, holders: map[int64]map[CellID]uint64{}, acquired: map[[2]uint64]CellID{}, lastEp: map[int64]uint64{}}
}

func (rc *realChecker) hook(cell CellID, id int64, epoch uint64, acquired bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	h := rc.holders[id]
	if h == nil {
		h = map[CellID]uint64{}
		rc.holders[id] = h
	}
	if !acquired {
		delete(h, cell)
		return
	}
	if len(h) > 0 {
		rc.t.Errorf("entity %d: cell %v acquired Real at epoch %d while %v still Real", id, cell, epoch, h)
	}
	key := [2]uint64{uint64(id), epoch}
	if prev, dup := rc.acquired[key]; dup {
		rc.t.Errorf("entity %d epoch %d acquired twice: %v then %v", id, epoch, prev, cell)
	}
	rc.acquired[key] = cell
	if epoch <= rc.lastEp[id] {
		rc.t.Errorf("entity %d: epoch went %d -> %d", id, rc.lastEp[id], epoch)
	}
	rc.lastEp[id] = epoch
	h[cell] = epoch
}

// forget 模拟崩溃：该格持有的 Real 一起消失。
func (rc *realChecker) forget(cell CellID) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, h := range rc.holders {
		delete(h, cell)
	}
}
