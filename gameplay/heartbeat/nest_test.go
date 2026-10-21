package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

var errBusiness = errors.New("business refused")

// nestFixture 是一个真实 Nest 引擎 + 实体管理器 + 未启动循环的驱动（测试逐格 step，等 Nest 执行完再断言）。
type nestFixture struct {
	t       *testing.T
	engine  *nest.NestMgr
	manager *entity.EntityManager
	driver  *Driver
	clock   *fakeClock
}

func newNestFixture(t *testing.T, register func(*nest.NestMgr), opts ...nest.NestOption) *nestFixture {
	t.Helper()
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	base := []nest.NestOption{
		nest.NestOptionWithGetter(access),
		nest.NestOptionWithTransactionCommitter(nullCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 1 << 17}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}),
	}
	engine := nest.NewEngine(append(base, opts...)...)
	if register != nil {
		register(engine)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	d := NewDriver(Config{Name: "nest-test", Logger: discard})
	clk := newFakeClock()
	d.SetClock(clk.Now)
	if err := d.Bind(engine, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		t.Fatal(err)
	}
	return &nestFixture{t: t, engine: engine, manager: manager, driver: d, clock: clk}
}

// waitFor 轮询条件（只用于等异步执行完成，不断言耗时）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settle 等全部已投递的拍执行完（在途标志由 handler 返回时清掉）。
func (f *nestFixture) settle(regs ...*Reg) {
	f.t.Helper()
	waitFor(f.t, "delivered beats to run", func() bool {
		for _, r := range regs {
			if r.inflight.Load() {
				return false
			}
		}
		return true
	})
}

// idle 等引擎排空（没有运行、就绪或等前驱的作业）：Remote 准入被拒的拍不会清在途，不能按在途标志等。
func (f *nestFixture) idle() {
	f.t.Helper()
	waitFor(f.t, "the engine to drain", func() bool {
		st := f.engine.Stats()
		q := st.Queue
		for _, lane := range []nest.DispatchLaneStats{q.Short, q.IO, q.Long} {
			if lane.Running+lane.Ready+lane.BlockedOnPredecessor+lane.WaitingForWorker != 0 {
				return false
			}
		}
		return q.LongContinuations == 0 && q.ContinuationRunning == 0 && st.ShortContinuations == 0
	})
}

func (f *nestFixture) step(n int) {
	for range n {
		f.driver.step(context.Background())
		f.clock.Advance(Resolution)
	}
}

// 回调在实体自己的 Nest worker 上、实体锁与事务内执行，与该实体的其他消息串行：业务消息持锁期间到期的拍排在它后面。
func TestBeatRunsUnderTheEntityLockInsideATransaction(t *testing.T) {
	block := nest.NewHandlerName("hb_test_block")
	entered, release := make(chan struct{}), make(chan struct{})
	var events []string
	var mu sync.Mutex
	logEvent := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(block, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
			logEvent("biz-start")
			close(entered)
			<-release
			logEvent("biz-end")
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	})
	e := newHBEntity(t, f.driver, 1, kindShort)
	f.manager.Add(e)
	var inTx, lockedElsewhere, onShort atomic.Bool
	r, _ := e.host.Register("x", 50*time.Millisecond, func(Beat) error {
		inTx.Store(nest.CurrentRollbackTx() != nil)
		onShort.Store(fctx.InShortWorker())
		got := make(chan bool)
		go func() { // 另一个 goroutine 取不到锁，说明回调期间锁被持有
			ok := e.GetMutex().TryLock()
			if ok {
				e.GetMutex().Unlock()
			}
			got <- ok
		}()
		lockedElsewhere.Store(!<-got)
		logEvent("beat")
		return nil
	})
	r.Start()
	if err := f.engine.Dispatch(context.Background(), block, e.ID(), nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.step(5)
	if f.driver.Stats().Delivered != 1 {
		t.Fatalf("stats %+v", f.driver.Stats())
	}
	close(release)
	f.settle(r)
	if fmt.Sprint(events) != "[biz-start biz-end beat]" {
		t.Fatalf("events %v: the beat must wait for the entity's running message", events)
	}
	if !inTx.Load() || !lockedElsewhere.Load() || !onShort.Load() {
		t.Fatalf("beat ran in tx=%v locked=%v shortWorker=%v", inTx.Load(), lockedElsewhere.Load(), onShort.Load())
	}
}

// 慢池实体的拍投到慢池，快池实体的投到快池（Nest 按实体种类分流，心跳不另选池）。
func TestBeatRunsInTheEntitysPool(t *testing.T) {
	f := newNestFixture(t, nil)
	var long, short atomic.Int32
	var wrong atomic.Int32
	for i, kind := range []entity.EntityKind{kindShort, kindLong} {
		e := newHBEntity(t, f.driver, int64(10+i), kind)
		f.manager.Add(e)
		wantLong := kind == kindLong
		r, _ := e.host.Register("x", 50*time.Millisecond, func(Beat) error {
			if fctx.InLongWorker() != wantLong || fctx.InShortWorker() == wantLong {
				wrong.Add(1)
			}
			if wantLong {
				long.Add(1)
			} else {
				short.Add(1)
			}
			return nil
		})
		r.Start()
		defer func() { f.settle(r) }()
	}
	f.step(5)
	waitFor(t, "both beats", func() bool { return long.Load() == 1 && short.Load() == 1 })
	if wrong.Load() != 0 {
		t.Fatalf("%d beats ran in the wrong pool", wrong.Load())
	}
}

// 事务内的启停提交后生效、回滚作废；回调里的启停随本拍事务。回调返回错误时本拍回滚，回调侧记录也回滚
// （下一拍 Elapsed 覆盖回滚的那一拍）。
func TestStartStopFollowTheTransaction(t *testing.T) {
	control := nest.NewHandlerName("hb_test_control")
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(control, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			r := params[0].(*Reg)
			switch params[1].(string) {
			case "start":
				r.Start()
			case "stop":
				r.Stop()
			case "period":
				_ = r.SetPeriod(time.Second)
			}
			if !r.Active() == (params[1].(string) == "start") {
				return nil, errors.New("business view did not change inside the transaction")
			}
			if params[2].(bool) {
				return nil, errBusiness
			}
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	})
	e := newHBEntity(t, f.driver, 20, kindShort)
	f.manager.Add(e)
	var beats []Beat
	failNext := false
	r, _ := e.host.Register("x", 100*time.Millisecond, func(b Beat) error {
		beats = append(beats, b)
		if failNext {
			failNext = false
			return errBusiness
		}
		return nil
	})
	req := func(op string, fail bool) error {
		_, err := f.engine.Request(context.Background(), control, e.ID(), nest.NewParams(r, op, fail))
		return err
	}
	if err := req("start", true); !errors.Is(err, errBusiness) {
		t.Fatalf("failing start = %v", err)
	}
	if r.Active() || f.driver.Registered() != 0 {
		t.Fatalf("rolled-back start took effect: active %v registered %d", r.Active(), f.driver.Registered())
	}
	if err := req("start", false); err != nil {
		t.Fatal(err)
	}
	if !r.Active() || f.driver.Registered() != 1 {
		t.Fatalf("committed start: active %v registered %d", r.Active(), f.driver.Registered())
	}
	if err := req("stop", true); !errors.Is(err, errBusiness) {
		t.Fatalf("failing stop = %v", err)
	}
	if !r.Active() || f.driver.Registered() != 1 {
		t.Fatalf("rolled-back stop took effect: active %v registered %d", r.Active(), f.driver.Registered())
	}

	// 回调返回错误：本拍回滚，下一拍不是 First、Elapsed 覆盖两拍。
	f.step(10)
	f.settle(r)
	failNext = true
	f.step(10)
	f.settle(r)
	f.step(10)
	f.settle(r)
	if len(beats) != 3 || !beats[0].First || beats[2].First || beats[2].Elapsed != 200*time.Millisecond {
		t.Fatalf("beats around a failing callback: %+v", beats)
	}

	if err := req("stop", false); err != nil {
		t.Fatal(err)
	}
	if r.Active() || f.driver.Registered() != 0 {
		t.Fatalf("committed stop: active %v registered %d", r.Active(), f.driver.Registered())
	}
}

// 卸载 / 重载：旧实例投出、排在队里的拍在同 ID 的新实例上执行时按过期丢弃，旧回调与新回调都不跑；
// 新实例自己登记后照常跳。
func TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement(t *testing.T) {
	block := nest.NewHandlerName("hb_test_block_other")
	entered, release := make(chan struct{}), make(chan struct{})
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(block, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			close(entered)
			<-release
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	}, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	other := newHBEntity(t, f.driver, 30, kindShort)
	f.manager.Add(other)
	oldE := newHBEntity(t, f.driver, 31, kindShort)
	f.manager.Add(oldE)
	var oldRuns, newRuns atomic.Int32
	oldReg, _ := oldE.host.Register("x", 50*time.Millisecond, func(Beat) error { oldRuns.Add(1); return nil })
	oldReg.Start()
	// 唯一的快池 worker 被另一个实体的消息占住：旧实例的拍准入后排队。
	if err := f.engine.Dispatch(context.Background(), block, other.ID(), nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.step(5)
	if f.driver.Stats().Delivered != 1 {
		t.Fatalf("stats %+v", f.driver.Stats())
	}
	if err := f.manager.Destroy(context.Background(), oldE, entity.DestroyReasonMemoryUnload, false); err != nil {
		t.Fatal(err)
	}
	newE := newHBEntity(t, f.driver, 31, kindShort)
	newReg, _ := newE.host.Register("x", 50*time.Millisecond, func(Beat) error { newRuns.Add(1); return nil })
	newReg.Start()
	f.manager.Add(newE)
	close(release)
	f.settle(oldReg)
	if oldRuns.Load() != 0 || newRuns.Load() != 0 || f.driver.Stats().DroppedStale != 1 {
		t.Fatalf("old %d new %d stats %+v", oldRuns.Load(), newRuns.Load(), f.driver.Stats())
	}
	f.step(5)
	f.settle(newReg)
	if newRuns.Load() != 1 || oldRuns.Load() != 0 || f.driver.Registered() != 1 {
		t.Fatalf("after reload old %d new %d registered %d", oldRuns.Load(), newRuns.Load(), f.driver.Registered())
	}
}

// 冒烟：真实 Nest 引擎、5 万实体、真实驱动循环；每个实体都跳过、没有过期 / 停止丢弃。不做性能测量。
func TestFiftyThousandEntitiesSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test")
	}
	n := 50_000 // 1s 档，5 万次/秒
	if raceEnabled {
		// -race 下本机 4 个快池 worker 约 2700 次/秒（2026-10-21 实测：5000 次/秒时排队 15s），
		// 降到 2000 实体，只验证功能；5 万实体由非 -race 运行覆盖。
		n = 2_000
	}
	f := newNestFixture(t, nil)
	f.driver.SetClock(time.Now)
	const period = time.Second
	ran := make([]atomic.Int32, n)
	regs := make([]*Reg, n)
	for i := range n {
		e := newHBEntity(t, f.driver, int64(100_000+i), kindShort)
		f.manager.Add(e)
		r, err := e.host.Register("smoke", period, func(b Beat) error {
			ran[i].Add(1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		r.Start()
		regs[i] = r
	}
	if err := f.driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.driver.Stop(context.Background()) })
	next := 0
	waitFor(t, "every entity to beat twice", func() bool {
		for ; next < n; next++ {
			if ran[next].Load() < 2 {
				return false
			}
		}
		return true
	})
	if err := f.driver.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.settle(regs...)
	st := f.driver.Stats()
	t.Logf("smoke (%d entities, %v): %+v", n, period, st)
	if st.DroppedStale+st.DroppedStopped+st.DroppedRemoved+st.DroppedNotLoaded+st.DroppedDuplicate != 0 || st.Executed < int64(2*n) || st.Registered != n {
		t.Fatalf("unexpected stats %+v", st)
	}
}
