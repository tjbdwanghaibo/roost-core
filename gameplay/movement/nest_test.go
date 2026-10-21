package movement

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 真实 Nest 引擎 + 实体同步 Manager + Interest（ApplySync）+ 启动的心跳驱动：业务操作经一个通用 handler 在实体锁与
// 事务里执行（rollback=undo、async），心跳回调由驱动投递。断言只看事件顺序、计数与最终状态，等待只用于等异步执行完。

// runHandler 在目标实体的锁与事务里执行参数里的函数；函数返回错误时事务回滚。
var runHandler = nest.NewHandlerName("movement_test.run")

func init() {
	nest.MustRegisterHandlerWithMeta(runHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func(*mover) error)(es[0].(*mover))
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
}

// frameLimits：冒烟里一个观察者同时看见上百个实体，超过缺省的每会话 100 个对象。
var frameLimits = frame.Limits{MaxObjects: 4096}

type nestFixture struct {
	t        *testing.T
	engine   *nest.NestMgr
	manager  *entity.EntityManager
	sync     *entitysync.Manager
	interest *policy.Interest
	driver   *heartbeat.Driver
	registry *actionflow.Registry
	sink     *frameSink
}

func newNestFixture(t *testing.T, bounds int64) *nestFixture {
	t.Helper()
	sink := &frameSink{}
	syncManager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: sink, Limits: frameLimits, SessionLost: func(id entitysync.SessionID, cause error) {
		sink.lost(id, cause)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syncManager.Close(context.Background()) })
	interest, err := policy.NewInterest(policy.InterestConfig{
		Manager: syncManager, ApplyMode: policy.ApplySync,
		AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: bounds, Y: bounds}}, BlockSize: 150, EnterRadius: 120, LeaveRadius: 150},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(interest.Close)
	manager := entity.NewEntityManager()
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(entity.NewManagerAccess(manager)),
		nest.NestOptionWithTransactionCommitter(nullCommitter{}),
		nest.NestOptionWithEntitySync(syncManager),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 1 << 16}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "movement-nest", Logger: discard})
	if err := driver.Bind(engine, manager.Get); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(); err != nil {
		t.Fatal(err)
	}
	// 先停驱动再停引擎（Cleanup 逆序）。
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	return &nestFixture{t: t, engine: engine, manager: manager, sync: syncManager, interest: interest, driver: driver, registry: newRegistry(t), sink: sink}
}

// add 建一个带同步 subject 的实体，登记进实体管理器与同步 Manager。
func (f *nestFixture) add(unique int64, o moverOptions) *mover {
	f.t.Helper()
	if o.locate == nil {
		o.locate = LocateIn(f.manager.Get)
	}
	o.fields = &MemFields{}
	o.sync = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: mustID(f.t, unique), Namespace: "movement", Packer: positionPacker{o.fields}})
	e := newMover(f.t, f.driver, unique, f.registry, o)
	f.manager.Add(e)
	if err := f.sync.Register(o.sync); err != nil {
		f.t.Fatal(err)
	}
	return e
}

// run 在 e 的实体锁与事务里执行 fn。
func (f *nestFixture) run(e *mover, fn func(*mover) error) error {
	_, err := f.engine.Request(context.Background(), runHandler, e.ID(), nest.NewParams(fn))
	return err
}

func (f *nestFixture) mustRun(e *mover, fn func(*mover) error) {
	f.t.Helper()
	if err := f.run(e, fn); err != nil {
		f.t.Fatal(err)
	}
}

// read 在 e 的实体锁内读状态（经同一个 handler，不改任何东西）。
func read[T any](f *nestFixture, e *mover, fn func(*mover) T) T {
	f.t.Helper()
	var out T
	f.mustRun(e, func(m *mover) error { out = fn(m); return nil })
	return out
}

// settle 让 e 上已经开始执行的消息（包括心跳拍）全部收尾：同实体的消息按准入顺序串行，一条消息要等前驱的作业整个
// 结束（含提交后回调：MapBasic 的发布、心跳登记的启停生效）才开始，所以这条空消息返回时，在它之前准入的拍的提交后
// 状态都已可见。用在“从拍的事务里观察到一个事件（动作结束）之后，要断言提交后的状态（Published、Registered）”的
// 地方：动作结束发生在拍的事务里，提交后回调在实体锁释放之后才执行，直接读会读到上一次提交的值。
func (f *nestFixture) settle(e *mover) {
	f.t.Helper()
	f.mustRun(e, func(*mover) error { return nil })
}

func (f *nestFixture) flush() {
	f.t.Helper()
	if err := f.sync.Flush(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// watch 让一个只看的玩家在 at 观察（Interest.Watch），返回它的玩家 ID。
func (f *nestFixture) watch(playerID int64, at spatial.Point) int64 {
	f.t.Helper()
	if err := f.sync.OpenSession(entitysync.SessionID(playerID)); err != nil {
		f.t.Fatal(err)
	}
	if err := f.interest.Watch(playerID, at, policy.ObserverView{}); err != nil {
		f.t.Fatal(err)
	}
	return playerID
}

// 位置变化经 QueueMove 进 AOI：提交后的 Flush 里生效（观察者的可见集合跟着变）；回滚的移动不进 AOI，DAO 字段与
// 发布的位置也都回到原处。进场景在提交之后 Show，观察者看见它。
func TestPositionChangesReachTheAOIOnlyWhenCommitted(t *testing.T) {
	f := newNestFixture(t, 1000)
	watcher := f.watch(9001, spatial.Point{X: 100, Y: 100})
	e := f.add(1, moverOptions{})
	space := InterestSpace{Interest: f.interest}
	f.mustRun(e, func(m *mover) error { return m.mb.Enter(1, space, spatial.Point{X: 110, Y: 100}) })
	f.flush()
	if !slices.Contains(f.interest.Visible(watcher), e.ID()) {
		t.Fatalf("watcher does not see the entity that entered next to it: %v", f.interest.Visible(watcher))
	}

	if err := f.run(e, func(m *mover) error {
		if err := m.mb.SetPos(spatial.Point{X: 900, Y: 900}); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatalf("rolled-back move: %v", err)
	}
	f.flush()
	if !slices.Contains(f.interest.Visible(watcher), e.ID()) {
		t.Fatal("a rolled-back move took the entity out of the watcher's view")
	}
	if pos := read(f, e, func(m *mover) spatial.Point { return m.mb.Pos() }); pos != (spatial.Point{X: 110, Y: 100}) || e.mb.Published().Pos != pos {
		t.Fatalf("after rollback: fields %+v published %+v", pos, e.mb.Published())
	}

	f.mustRun(e, func(m *mover) error { return m.mb.SetPos(spatial.Point{X: 900, Y: 900}) })
	f.flush()
	if slices.Contains(f.interest.Visible(watcher), e.ID()) {
		t.Fatal("a committed move far away did not take the entity out of the watcher's view")
	}
	if e.mb.Published().Pos != (spatial.Point{X: 900, Y: 900}) {
		t.Fatalf("published %+v", e.mb.Published())
	}
}

// 进出场景随事务：回滚的 Enter / Leave 不调用 Space、组件状态回到原处；提交后才调用，Enter 用本事务结束时的位置
// （同一事务里进场景后又移动不另入队）；同一事务里先进后出互相抵消。
func TestEnterAndLeaveFollowTheTransaction(t *testing.T) {
	f := newNestFixture(t, 1000)
	e := f.add(2, moverOptions{})
	space := &spaceLog{}
	if err := f.run(e, func(m *mover) error {
		if err := m.mb.Enter(5, space, spatial.Point{X: 1, Y: 1}); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatal(err)
	}
	inScene := read(f, e, func(m *mover) bool { return m.mb.InScene() })
	if ops := space.take(); len(ops) != 0 || inScene || e.mb.Published().SceneID != 0 {
		t.Fatalf("rolled-back enter: ops %v in scene %v published %+v", ops, inScene, e.mb.Published())
	}
	f.mustRun(e, func(m *mover) error {
		if err := m.mb.Enter(5, space, spatial.Point{X: 1, Y: 1}); err != nil {
			return err
		}
		return m.mb.SetPos(spatial.Point{X: 2, Y: 2})
	})
	f.mustRun(e, func(m *mover) error { return m.mb.SetPos(spatial.Point{X: 3, Y: 3}) })
	if err := f.run(e, func(m *mover) error {
		if err := m.mb.Leave(); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatal(err)
	}
	if !read(f, e, func(m *mover) bool { return m.mb.InScene() }) {
		t.Fatal("rolled-back leave took the entity out of the scene")
	}
	f.mustRun(e, func(m *mover) error { return m.mb.Leave() })
	f.mustRun(e, func(m *mover) error {
		if err := m.mb.Enter(6, space, spatial.Point{X: 4, Y: 4}); err != nil {
			return err
		}
		return m.mb.Leave()
	})
	want := []string{"enter 2,2", "move 3,3", "leave"}
	if ops := space.take(); !slices.Equal(ops, want) {
		t.Fatalf("space saw %v, want %v", ops, want)
	}
	if snap := e.mb.Published(); snap.SceneID != 0 || snap.Pos != (spatial.Point{X: 4, Y: 4}) {
		t.Fatalf("published %+v", snap)
	}
}

// 回滚的 Move.MoveTo 不生效：移动状态回到原处（Move 的推进状态随事务回滚）。
func TestRolledBackMoveToLeavesNothingRunning(t *testing.T) {
	f := newNestFixture(t, 1000)
	e := f.add(3, moverOptions{})
	f.mustRun(e, func(m *mover) error { return m.mb.Enter(1, &spaceLog{}, spatial.Point{}) })
	if err := f.run(e, func(m *mover) error {
		if _, err := m.move.MoveTo([]spatial.Point{{X: 50}}, 10); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatal(err)
	}
	if moving := read(f, e, func(m *mover) bool { return m.move.Moving() }); moving || f.driver.Registered() != 0 {
		t.Fatalf("rolled-back move: moving %v registered %d", moving, f.driver.Registered())
	}
}

// runner 由心跳 tick：组的当前动作需要周期 tick 时这个组挂心跳，每拍在实体自己的 worker、事务里 tick，带本拍
// Elapsed；动作结束后心跳停，空闲不占调度。
func TestRunnerIsTickedByTheHeartbeatAndStopsWhenIdle(t *testing.T) {
	f := newNestFixture(t, 1000)
	e := f.add(4, moverOptions{})
	action := &tickAction{limit: 3}
	f.mustRun(e, func(m *mover) error {
		_, err := m.beat.Runner().Start(actTick, action, 0, time.Time{})
		return err
	})
	waitFor(t, "the tick action to end", func() bool { return len(e.endedActions()) == 1 })
	f.settle(e)
	ticks := action.seen()
	if len(ticks) != 3 {
		t.Fatalf("ticks %+v, want 3", ticks)
	}
	for _, tick := range ticks {
		if !tick.inTx || tick.elapsed <= 0 {
			t.Fatalf("tick outside a heartbeat beat: %+v", ticks)
		}
	}
	if active := read(f, e, func(m *mover) bool { return m.beat.Reg(groupWork).Active() }); active || f.driver.Registered() != 0 {
		t.Fatalf("idle runner keeps its heartbeat: active %v registered %d", active, f.driver.Registered())
	}
	if got := e.endedActions()[0]; got.status != actionflow.ActionStatusSuccess {
		t.Fatalf("ended %+v", got)
	}
}

// MoveTo 动作由 runner 的心跳驱动（参考 ssr：action 挂心跳、在心跳里调 move）：移动组挂一个心跳，移动中实体只有
// 这一个登记（每拍一条消息），每拍动作调 Move.Advance，到达时动作收尾为成功；结束后什么都不跳。新的 MoveTo 取代
// 旧的：旧的以取消结束、新的从当前位置走到新终点。
func TestMoveToActionIsDrivenByTheRunnerHeartbeatAndReplacedByTheNextOne(t *testing.T) {
	f := newNestFixture(t, 1000)
	e := f.add(5, moverOptions{})
	f.mustRun(e, func(m *mover) error {
		if err := m.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
			return err
		}
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 3}, {X: 3, Y: 3}}, Speed: 40}, 0, time.Time{})
		return err
	})
	if active := read(f, e, func(m *mover) bool { return m.beat.Reg(groupMove).Active() }); !active || f.driver.Registered() != 1 {
		t.Fatalf("moving entity: move group heartbeat active %v, registrations %d (want exactly the runner's one)", active, f.driver.Registered())
	}
	waitFor(t, "the move to arrive", func() bool { return len(e.endedActions()) == 1 })
	f.settle(e)
	if got := e.endedActions()[0]; got.kind != actMoveTo || got.status != actionflow.ActionStatusSuccess || e.mb.Published().Pos != (spatial.Point{X: 3, Y: 3}) {
		t.Fatalf("ended %+v at %+v", got, e.mb.Published())
	}
	if f.driver.Registered() != 0 {
		t.Fatalf("registered %d after arriving", f.driver.Registered())
	}

	f.mustRun(e, func(m *mover) error {
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 3, Y: 900}}, Speed: 20}, 0, time.Time{})
		return err
	})
	waitFor(t, "the long move to get going", func() bool { return e.mb.Published().Pos.Y >= 5 })
	f.mustRun(e, func(m *mover) error {
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 0, Y: 0}}, Speed: 200}, 0, time.Time{})
		return err
	})
	waitFor(t, "the replacing move to arrive", func() bool { return len(e.endedActions()) == 3 })
	f.settle(e)
	ended := e.endedActions()
	if ended[1].status != actionflow.ActionStatusCanceled || ended[2].status != actionflow.ActionStatusSuccess {
		t.Fatalf("ended %+v, want the long move canceled and the replacing one done", ended)
	}
	if e.mb.Published().Pos != (spatial.Point{}) || f.driver.Registered() != 0 {
		t.Fatalf("at %+v registered %d", e.mb.Published(), f.driver.Registered())
	}
}

// Follow 动作跟随目标；目标卸载后下一拍以失败（target lost）收尾。
func TestFollowActionEndsWhenTheTargetIsUnloaded(t *testing.T) {
	f := newNestFixture(t, 1000)
	target := f.add(6, moverOptions{})
	follower := f.add(7, moverOptions{})
	f.mustRun(target, func(m *mover) error { return m.mb.Enter(1, &spaceLog{}, spatial.Point{X: 40, Y: 40}) })
	f.mustRun(follower, func(m *mover) error {
		if err := m.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
			return err
		}
		_, err := m.beat.Runner().Start(actFollow, FollowParam{Target: target.ID(), Speed: 200, Keep: 2}, 0, time.Time{})
		return err
	})
	waitFor(t, "the follower to close in", func() bool {
		p := follower.mb.Published().Pos
		return spatial.WithinDistance(p, spatial.Point{X: 40, Y: 40}, 3)
	})
	if len(follower.endedActions()) != 0 {
		t.Fatalf("follow without StopWhenReached ended on reaching: %+v", follower.endedActions())
	}
	if err := f.manager.Destroy(context.Background(), target, entity.DestroyReasonMemoryUnload, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the follow to end", func() bool { return len(follower.endedActions()) == 1 })
	f.settle(follower)
	if f.driver.Registered() != 0 {
		t.Fatalf("registered %d after the follow ended", f.driver.Registered())
	}
	if got := follower.endedActions()[0]; got.status != actionflow.ActionStatusFailed || got.reason != TargetLost.String() {
		t.Fatalf("ended %+v", got)
	}
}

// Start 即完成（D21 A7）：已经在终点的 MoveTo、找不到目标的 Follow、目标已在 Keep 以内且 StopWhenReached 的 Follow，
// 在发起它的 handler 里就结束（OnEnded 在同一事务里），不挂心跳、Move 不处于移动中。
func TestAMoveThatIsAlreadySettledEndsInStartWithoutAHeartbeat(t *testing.T) {
	f := newNestFixture(t, 1000)
	target := f.add(8, moverOptions{})
	e := f.add(9, moverOptions{})
	f.mustRun(target, func(m *mover) error { return m.mb.Enter(1, &spaceLog{}, spatial.Point{X: 4, Y: 3}) })
	var inHandler []int
	f.mustRun(e, func(m *mover) error {
		if err := m.mb.Enter(1, &spaceLog{}, spatial.Point{X: 3, Y: 3}); err != nil {
			return err
		}
		for _, start := range []struct {
			kind  actionflow.ActionKind
			param any
		}{
			{actMoveTo, MoveToParam{Path: []spatial.Point{{X: 3, Y: 3}, {X: 3, Y: 3}}, Speed: 10}},
			{actFollow, FollowParam{Target: mustID(t, 404), Speed: 10}},
			{actFollow, FollowParam{Target: target.ID(), Speed: 10, Keep: 2, StopWhenReached: true}},
		} {
			if _, err := m.beat.Runner().Start(start.kind, start.param, 0, time.Time{}); err != nil {
				return err
			}
			inHandler = append(inHandler, len(m.endedActions()))
		}
		if m.move.Moving() {
			return errors.New("a settled move left the Move component moving")
		}
		return nil
	})
	f.settle(e)
	if fmt.Sprint(inHandler) != "[1 2 3]" || f.driver.Registered() != 0 {
		t.Fatalf("ended inside the handler after each start: %v; registered %d", inHandler, f.driver.Registered())
	}
	got := e.endedActions()
	want := []endedAction{
		{id: 1, kind: actMoveTo, status: actionflow.ActionStatusSuccess, reason: Arrived.String()},
		{id: 2, kind: actFollow, status: actionflow.ActionStatusFailed, reason: TargetLost.String()},
		{id: 3, kind: actFollow, status: actionflow.ActionStatusSuccess, reason: Arrived.String()},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("ended %+v, want %+v", got, want)
	}
}

// 冻结移动组（D21 A5，经真实心跳）：暂停时这个组的心跳停掉（驱动上没有登记）、位置不再变；解冻后重新挂上并走到
// 终点；打断模式取消在途移动（取消状态、原因 action group frozen），Move 停下、心跳停掉。
func TestFreezingTheMoveGroupPausesOrInterruptsTheMove(t *testing.T) {
	f := newNestFixture(t, 1000)
	e := f.add(10, moverOptions{})
	f.mustRun(e, func(m *mover) error {
		if err := m.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
			return err
		}
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 0, Y: 60}}, Speed: 40}, 0, time.Time{})
		return err
	})
	waitFor(t, "the move to get going", func() bool { return e.mb.Published().Pos.Y >= 2 })
	f.mustRun(e, func(m *mover) error { return m.beat.Runner().Freeze(groupMove, actionflow.FreezePause) })
	f.settle(e)
	paused := e.mb.Published().Pos
	if f.driver.Registered() != 0 || len(e.endedActions()) != 0 {
		t.Fatalf("paused move: registered %d ended %+v", f.driver.Registered(), e.endedActions())
	}
	// 心跳已停（上面断言了驱动上没有登记），再经一条消息读一次：位置没有变。
	if at := read(f, e, func(m *mover) spatial.Point { return m.mb.Pos() }); at != paused || !e.move.Moving() {
		t.Fatalf("paused at %+v, then %+v moving %v", paused, at, e.move.Moving())
	}
	f.mustRun(e, func(m *mover) error { return m.beat.Runner().Recover(groupMove, time.Time{}) })
	waitFor(t, "the resumed move to arrive", func() bool { return len(e.endedActions()) == 1 })
	f.settle(e)
	if got := e.endedActions()[0]; got.status != actionflow.ActionStatusSuccess || e.mb.Published().Pos != (spatial.Point{X: 0, Y: 60}) || f.driver.Registered() != 0 {
		t.Fatalf("resumed move ended %+v at %+v registered %d", got, e.mb.Published(), f.driver.Registered())
	}

	f.mustRun(e, func(m *mover) error {
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 0, Y: 0}}, Speed: 40}, 0, time.Time{})
		return err
	})
	waitFor(t, "the second move to get going", func() bool { return e.mb.Published().Pos.Y <= 58 })
	f.mustRun(e, func(m *mover) error { return m.beat.Runner().Freeze(groupMove, actionflow.FreezeCancel) })
	f.settle(e)
	ended := e.endedActions()
	if len(ended) != 2 || ended[1].status != actionflow.ActionStatusCanceled || ended[1].reason != "action group frozen" || e.move.Moving() || f.driver.Registered() != 0 {
		t.Fatalf("interrupted move: ended %+v moving %v registered %d", ended, e.move.Moving(), f.driver.Registered())
	}
	if err := f.run(e, func(m *mover) error {
		_, err := m.beat.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{{X: 0, Y: 0}}, Speed: 40}, 0, time.Time{})
		return err
	}); !errors.Is(err, actionflow.ErrActionGroupFrozen) {
		t.Fatalf("move while frozen: %v", err)
	}
}

// frameSink 解开推给会话的帧，记下每个会话看到的每个实体的最新位置（positionPacker 的载荷）。
type frameSink struct {
	mu   sync.Mutex
	seen map[entitysync.SessionID]map[int64]spatial.Point
	// updates 是收到的位置载荷总数。
	updates int
	// err 汇总解帧失败与 Manager 丢掉的会话。
	err error
}

func (s *frameSink) Push(_ context.Context, session entitysync.SessionID, data []byte) error {
	decoded, err := entitysync.DecodeFrame(data, frameLimits)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.err = errors.Join(s.err, err)
		return nil
	}
	if s.seen == nil {
		s.seen = make(map[entitysync.SessionID]map[int64]spatial.Point)
	}
	if s.seen[session] == nil {
		s.seen[session] = make(map[int64]spatial.Point)
	}
	for _, object := range decoded.Objects {
		for _, component := range object.Components {
			update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
			if err != nil {
				s.err = errors.Join(s.err, err)
				continue
			}
			raw := update.Payload.AppendTo(nil)
			if len(raw) != 16 {
				continue
			}
			s.seen[session][update.SubjectID] = spatial.Point{X: int64(binary.BigEndian.Uint64(raw)), Y: int64(binary.BigEndian.Uint64(raw[8:]))}
			s.updates++
		}
	}
	return nil
}

func (s *frameSink) lost(id entitysync.SessionID, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = errors.Join(s.err, fmt.Errorf("session %d lost: %w", id, cause))
}

// lastSeen 返回会话最后收到的实体位置。
func (s *frameSink) lastSeen(session entitysync.SessionID, subject int64) (spatial.Point, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.seen[session][subject]
	return p, ok
}

// positionPacker 把位置打成 16 字节（X、Y 各 8 字节大端），快照与增量相同；在实体锁内执行。
type positionPacker struct{ fields *MemFields }

func (p positionPacker) PackSubjectSnapshot(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return p.pack(), nil
}

func (p positionPacker) PackSubjectDelta(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
	return p.pack(), nil
}

func (p positionPacker) pack() entity.FrozenSyncPayload {
	pos := p.fields.Pos()
	raw := make([]byte, 16)
	binary.BigEndian.PutUint64(raw, uint64(pos.X))
	binary.BigEndian.PutUint64(raw[8:], uint64(pos.Y))
	return entity.TakeFrozenSyncPayload(1, raw)
}
