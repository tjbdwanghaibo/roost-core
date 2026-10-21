package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
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
	d := NewDriver(Config{Name: "nest-test", Logger: discard, Delivery: testDelivery})
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

// stepUntilDelivered 逐格推进，直到驱动累计投出 n 拍（至多一个最长档的格数）。
func (f *nestFixture) stepUntilDelivered(n int64) {
	f.t.Helper()
	for range 6000 {
		if f.driver.Stats().Delivered >= n {
			return
		}
		f.step(1)
	}
	f.t.Fatalf("no beat delivered: %+v", f.driver.Stats())
}

func (f *nestFixture) step(n int) {
	for range n {
		f.driver.step(context.Background())
		f.clock.Advance(Resolution)
	}
}

// 回调在实体自己的 Nest worker 上、实体锁与事务内执行，与该实体的其他消息串行：业务消息持锁期间到期的拍排在它后面。
func TestBeatRunsUnderTheEntityLockInsideATransaction(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
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
	})
}

// 慢池实体的拍投到慢池，快池实体的投到快池（Nest 按实体种类分流，心跳不另选池）。
func TestBeatRunsInTheEntitysPool(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
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
	})
}

// 事务内的启停提交后生效、回滚作废；回调里的启停随本拍事务。回调返回错误时本拍回滚，回调侧记录也回滚
// （下一拍 Elapsed 覆盖回滚的那一拍）。
func TestStartStopFollowTheTransaction(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
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
	})
}

// 卸载 / 重载：旧实例投出、排在队里的拍在同 ID 的新实例上执行时按过期丢弃，旧回调与新回调都不跑；
// 新实例自己登记后照常跳。
func TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
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
	})
}

// 冒烟：真实 Nest 引擎、5 万实体、真实驱动循环；每个实体都跳过、没有过期 / 停止丢弃。不做性能测量。
func TestFiftyThousandEntitiesSmoke(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
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
	})
}

// D22-1：Beat.Now 是这一拍消息冻结的业务时间（fctx.Now），回调里再读 fctx.Now 得到同一个值。驱动用缺省时钟
// （不 SetClock），进程业务时钟的来源每读一次走 1ms：现读第二次就会不同。
func TestBeatNowIsTheMessagesFrozenBusinessTime(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f := newNestFixture(t, nil)
		var mu sync.Mutex
		tick := time.Unix(1_800_000_000, 0)
		clock.SetSource(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			tick = tick.Add(time.Millisecond)
			return tick
		})
		t.Cleanup(func() { clock.SetSource(nil) })
		d := NewDriver(Config{Name: "frozen", Logger: discard, Delivery: testDelivery})
		if err := d.Bind(f.engine, func(id int64) entity.IThreadSafeEntity { return f.manager.Get(id) }); err != nil {
			t.Fatal(err)
		}
		e := newHBEntity(t, d, 40, kindShort)
		f.manager.Add(e)
		type reading struct{ beat, message time.Time }
		got := make(chan reading, 4)
		r, _ := e.host.Register("x", 50*time.Millisecond, func(b Beat) error {
			got <- reading{beat: b.Now, message: fctx.Now()}
			return nil
		})
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		for range 5 {
			d.step(context.Background())
		}
		f.settle(r)
		select {
		case g := <-got:
			if !g.beat.Equal(g.message) {
				t.Fatalf("Beat.Now = %v, but the beat's message froze %v: the beat read the clock a second time", g.beat, g.message)
			}
		default:
			t.Fatal("no beat ran")
		}
	})
}

// tdFields 扮演场景实体上时间域的 DAO 字段（测试里不在事务中写，不登记回滚）。
type tdFields struct{ state timedomain.State }

func (f *tdFields) TimeDomain() timedomain.State     { return f.state }
func (f *tdFields) SetTimeDomain(s timedomain.State) { f.state = s }

// D22 时间域：挂了时间域的登记，调度仍按真实时间（档位不缩放），Beat.Now 是这一拍消息的域时间、Elapsed 是域时间
// 的经过；2 倍速时 Elapsed 是业务经过的两倍，时停时为 0、Now 不动；Elapsed 上限乘倍率。
func TestBeatsOnATimeDomainFollowItsRate(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f := newNestFixture(t, nil)
		clock.SetSource(f.clock.Now) // fctx 冻结的业务时间与驱动用同一个可控时钟
		t.Cleanup(func() { clock.SetSource(nil) })
		domain, err := timedomain.New(&tdFields{}, timedomain.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := domain.SetRate(2); err != nil { // 不在事务里：立即生效
			t.Fatal(err)
		}
		e := newHBEntity(t, f.driver, 50, kindShort)
		f.manager.Add(e)
		type reading struct {
			beat    Beat
			message time.Time
		}
		got := make(chan reading, 8)
		r, err := e.host.Register("scene", 100*time.Millisecond, func(b Beat) error {
			got <- reading{beat: b, message: domain.Now()}
			return nil
		}, WithTimeDomain(domain))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
		beat := func() reading {
			t.Helper()
			before := f.driver.Stats().Delivered
			for f.driver.Stats().Delivered == before {
				f.step(1)
			}
			f.settle(r)
			select {
			case g := <-got:
				if !g.beat.Now.Equal(g.message) {
					t.Fatalf("Beat.Now = %v, but the beat's message reads the domain at %v", g.beat.Now, g.message)
				}
				return g
			default:
				t.Fatal("a delivered beat did not run")
				return reading{}
			}
		}

		first := beat()
		if !first.beat.First || first.beat.Elapsed <= 0 || first.beat.Elapsed%2 != 0 {
			t.Fatalf("first beat %+v: want a positive Elapsed in 2x domain time", first.beat)
		}
		second := beat() // 下一拍：业务时间过了一个档位 100ms，域时间过了 200ms
		if second.beat.Elapsed != 200*time.Millisecond || second.beat.Now.Sub(first.beat.Now) != 200*time.Millisecond {
			t.Fatalf("at 2x one 100ms period moved the domain %v (Elapsed %v), want 200ms", second.beat.Now.Sub(first.beat.Now), second.beat.Elapsed)
		}

		if err := domain.SetRate(0); err != nil {
			t.Fatal(err)
		}
		stopped := beat()
		after := beat()
		if after.beat.Elapsed != 0 || !after.beat.Now.Equal(stopped.beat.Now) {
			t.Fatalf("a stopped domain beat with Elapsed %v, Now %v -> %v; want 0 and no movement", after.beat.Elapsed, stopped.beat.Now, after.beat.Now)
		}

		// 10 倍速、业务时间一下过了 2 秒（停顿）：Elapsed 的上限是 max(1s, 100ms) × 10 = 10s，不是 1s。
		if err := domain.SetRate(10); err != nil {
			t.Fatal(err)
		}
		beat()
		f.clock.Advance(2 * time.Second)
		if long := beat(); long.beat.Elapsed != 10*time.Second {
			t.Fatalf("a 2s stall at 10x gave Elapsed %v, want the 10s cap (max(1s, period) x rate)", long.beat.Elapsed)
		}
	})
}

// 批量投递（Nest 批量作业）下的同 ID 顺序：业务消息已准入、尚未执行（它同时声明了另一个实体，排在那个实体正在执行的
// 消息后面；本实体的锁此时没人持有）时，到期的拍在
// 这条消息之后、同一格内执行——Nest 当场把它转成单目标作业排在后面，不跳拍、不等下一格（Nest 计“冲突转单目标”1 次）。
// 转成单目标作业后这一拍仍在途：之后一个周期里到期的同一登记按 inflight 跳过，不会有第二拍在途。
// 变异核对：① Nest 冲突时直接执行而不排队（prepareTryBatchTargetLocked 对已有 tails 的目标照样执行）时拍插到业务消息
// 之前，本用例变红；② 驱动不看在途标志照投时同一登记投出两拍，本用例变红。
func TestConflictingBeatRunsRightAfterTheAdmittedMessage(t *testing.T) {
	useDelivery(t, DeliveryBatch) // 本用例断言批量作业特有的计数（冲突转单目标、交回）
	block := nest.NewHandlerName("hb_test_conflict_block")
	biz := nest.NewHandlerName("hb_test_conflict_biz")
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	var events []string
	var mu sync.Mutex
	logEvent := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(block, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			close(entered)
			<-gate
			logEvent("running")
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
		m.MustRegisterHandlerWithMeta(biz, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			logEvent("admitted")
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	})
	t.Cleanup(release) // 断言失败时也放开占住 worker 的消息，先于引擎停机执行
	e := newHBEntity(t, f.driver, 40, kindShort)
	other := newHBEntity(t, f.driver, 42, kindShort)
	f.manager.Add(e)
	f.manager.Add(other)
	r, _ := e.host.Register("x", 50*time.Millisecond, func(Beat) error { logEvent("beat"); return nil })
	r.Start()
	if err := f.engine.Dispatch(context.Background(), block, other.ID(), nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	// 业务消息声明 e 与 other：排在 other 正在执行的消息后面等前驱，占着 e 的 tails，但不持 e 的锁。
	if err := f.engine.DispatchMulti(context.Background(), biz, []int64{e.ID(), other.ID()}, nil); err != nil {
		t.Fatal(err)
	}
	f.stepUntilDelivered(1) // 首拍到期、投递
	waitFor(t, "the beat to be converted or to run", func() bool {
		mu.Lock()
		ran := slices.Contains(events, "beat")
		mu.Unlock()
		return ran || f.engine.Stats().Queue.Short.TryBatchConverted == 1
	})
	mu.Lock()
	early := slices.Clone(events)
	mu.Unlock()
	if slices.Contains(early, "beat") {
		t.Fatalf("events %v: the beat ran before the already admitted message", early)
	}
	f.step(10) // 再过一个多周期：同一登记到期，上一拍仍在途
	release()
	f.settle(r)
	f.idle()
	if fmt.Sprint(events) != "[running admitted beat]" {
		t.Fatalf("events %v: the beat must run right after the already admitted message", events)
	}
	st := f.driver.Stats()
	if st.Delivered != 1 || st.Executed != 1 || st.SkippedQueueFull+st.SkippedRejected != 0 || st.SkippedInflight == 0 {
		t.Fatalf("stats %+v: want one delivered and executed beat, no admission skips, and the next due beat skipped as in flight", st)
	}
	if c := f.engine.Stats().Queue.Short.TryBatchConverted; c != 1 {
		t.Fatalf("converted %d, want 1", c)
	}
}

// 冲突的拍转单目标作业被准入拒绝（快池等待位已满）时才算跳拍：清在途、计 skipped{queue_full}、打限频 Warn；
// 下一个周期照常投。
func TestConflictingBeatIsSkippedOnlyWhenAdmissionRefuses(t *testing.T) {
	useDelivery(t, DeliveryBatch) // 本用例断言批量作业特有的计数（冲突转单目标、交回）
	block := nest.NewHandlerName("hb_test_full_block")
	biz := nest.NewHandlerName("hb_test_full_biz")
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	logs := &countingHandler{}
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(block, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			close(entered)
			<-gate
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
		m.MustRegisterHandlerWithMeta(biz, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	}, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	t.Cleanup(release) // 断言失败时也放开占住 worker 的消息，先于引擎停机执行
	f.driver.config.Logger = slog.New(logs)
	e := newHBEntity(t, f.driver, 41, kindShort)
	f.manager.Add(e)
	var runs atomic.Int32
	r, _ := e.host.Register("x", 50*time.Millisecond, func(Beat) error { runs.Add(1); return nil })
	r.Start()
	if err := f.engine.Dispatch(context.Background(), block, e.ID(), nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := f.engine.Dispatch(context.Background(), biz, e.ID(), nil); err != nil { // 占满唯一的等待位
		t.Fatal(err)
	}
	f.stepUntilDelivered(1)
	waitFor(t, "the beat to be handed back", func() bool {
		f.driver.returnedMu.Lock()
		defer f.driver.returnedMu.Unlock()
		return len(f.driver.returned) == 1
	})
	f.step(1) // 下一格开头处理交回
	f.driver.flush(time.Now())
	st := f.driver.Stats()
	if st.SkippedQueueFull != 1 || r.inflight.Load() || logs.count() == 0 {
		t.Fatalf("stats %+v inflight=%v warns=%d: a refused conversion is a skipped beat (queue_full, Warn)", st, r.inflight.Load(), logs.count())
	}
	release()
	f.idle()
	f.step(5) // 下一个周期照常投、执行
	f.settle(r)
	if runs.Load() != 1 {
		t.Fatalf("runs %d after the next period, want 1", runs.Load())
	}
}

// 卸载窗口（批量投递）：拍准入后、执行前实例被卸载且没有重载时，批量作业不冷加载，交回“不在内存”；驱动按
// dropped{not_loaded} 丢弃、清在途，回调不执行，不算跳拍。
// 变异核对：drainReturned 把“不在内存”也按跳拍计时，DroppedNotLoaded 为 0、SkippedRejected 为 1，本用例变红。
func TestBeatOfAnUnloadedInstanceIsDroppedWithoutColdLoad(t *testing.T) {
	useDelivery(t, DeliveryBatch) // 本用例断言批量作业特有的计数（冲突转单目标、交回）
	block := nest.NewHandlerName("hb_test_unload_block")
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	f := newNestFixture(t, func(m *nest.NestMgr) {
		m.MustRegisterHandlerWithMeta(block, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			close(entered)
			<-gate
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	}, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	t.Cleanup(release)
	other := newHBEntity(t, f.driver, 50, kindShort)
	f.manager.Add(other)
	e := newHBEntity(t, f.driver, 51, kindShort)
	f.manager.Add(e)
	var runs atomic.Int32
	r, _ := e.host.Register("x", 50*time.Millisecond, func(Beat) error { runs.Add(1); return nil })
	r.Start()
	if err := f.engine.Dispatch(context.Background(), block, other.ID(), nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	f.stepUntilDelivered(1)
	if err := f.manager.Destroy(context.Background(), e, entity.DestroyReasonMemoryUnload, false); err != nil {
		t.Fatal(err)
	}
	release()
	f.idle()
	waitFor(t, "the hand-back", func() bool {
		f.driver.returnedMu.Lock()
		defer f.driver.returnedMu.Unlock()
		return len(f.driver.returned) == 1
	})
	f.step(1)
	st := f.driver.Stats()
	if runs.Load() != 0 || st.DroppedNotLoaded != 1 || st.SkippedRejected+st.SkippedQueueFull != 0 || r.inflight.Load() {
		t.Fatalf("runs %d stats %+v inflight=%v: an unloaded instance's beat is dropped as not_loaded", runs.Load(), st, r.inflight.Load())
	}
}

// D22-1 在两种投递方式下都成立：同一格里同一实体的几拍（四个同档登记，两个挂场景时间域）各自冻结一次——Beat.Now
// 等于回调里读到的 fctx.Now / 域时间，且各拍互不相同（进程业务时钟每读一次走 1ms）。批量方式下这几拍在同一个 Nest
// 批量作业里依次执行，每个目标开始前重新冻结（Context.Refreeze），不是整批共用一个时刻。
// 变异核对：批量作业逐目标不 Refreeze 时 batch 子测试里四拍的 Beat.Now 相同，本用例变红。
func TestEveryBeatFreezesItsOwnBusinessTime(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		f := newNestFixture(t, nil)
		var mu sync.Mutex
		tick := time.Unix(1_800_000_000, 0)
		clock.SetSource(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			tick = tick.Add(time.Millisecond)
			return tick
		})
		t.Cleanup(func() { clock.SetSource(nil) })
		d := NewDriver(Config{Name: "frozen-each", Logger: discard, Delivery: testDelivery})
		if err := d.Bind(f.engine, func(id int64) entity.IThreadSafeEntity { return f.manager.Get(id) }); err != nil {
			t.Fatal(err)
		}
		domain, err := timedomain.New(&tdFields{}, timedomain.Config{})
		if err != nil {
			t.Fatal(err)
		}
		e := newHBEntity(t, d, 60, kindShort)
		f.manager.Add(e)
		type reading struct {
			name          string
			beat, message time.Time
		}
		got := make(chan reading, 8)
		var regs []*Reg
		for i := range 4 {
			name := fmt.Sprint("r", i)
			var opts []RegOption
			read := fctx.Now
			if i >= 2 {
				opts = append(opts, WithTimeDomain(domain))
				read = domain.Now
			}
			r, err := e.host.Register(name, 50*time.Millisecond, func(b Beat) error {
				got <- reading{name: name, beat: b.Now, message: read()}
				return nil
			}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Start(); err != nil {
				t.Fatal(err)
			}
			regs = append(regs, r)
		}
		for d.Stats().Delivered < 4 {
			d.step(context.Background())
		}
		f.settle(regs...)
		seen := map[time.Time]string{}
		for range 4 {
			select {
			case g := <-got:
				if !g.beat.Equal(g.message) {
					t.Fatalf("%s: Beat.Now = %v, but the beat's execution froze %v", g.name, g.beat, g.message)
				}
				if other, dup := seen[g.beat]; dup {
					t.Fatalf("%s and %s share Beat.Now %v: each beat must freeze its own business time", other, g.name, g.beat)
				}
				seen[g.beat] = g.name
			default:
				t.Fatal("not every delivered beat ran")
			}
		}
	})
}

// Reg.Close（成员换场景重建行为组件）：名字立即释放、同名可以马上重新登记；旧登记已经投出、排在队里的拍到达时不执行
// 回调（按过期丢弃），新登记照常跳——按名字看旧拍与新拍可能同时在队列里，但只有新登记的执行。
func TestAClosedRegistrationFreesItsNameAndItsQueuedBeatDoesNotRun(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		block := nest.NewHandlerName("hb_test_block_close")
		entered, release := make(chan struct{}), make(chan struct{})
		f := newNestFixture(t, func(m *nest.NestMgr) {
			m.MustRegisterHandlerWithMeta(block, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
				close(entered)
				<-release
				return nil, nil
			}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
		}, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
		other := newHBEntity(t, f.driver, 50, kindShort)
		f.manager.Add(other)
		e := newHBEntity(t, f.driver, 51, kindShort)
		f.manager.Add(e)
		var oldRuns, newRuns atomic.Int32
		oldReg, _ := e.host.Register("actionflow/1", 50*time.Millisecond, func(Beat) error { oldRuns.Add(1); return nil })
		oldReg.Start()
		// 唯一的快池 worker 被另一个实体的消息占住：旧登记的拍准入后排队。
		if err := f.engine.Dispatch(context.Background(), block, other.ID(), nil); err != nil {
			t.Fatal(err)
		}
		<-entered
		f.step(5)
		if f.driver.Stats().Delivered != 1 {
			t.Fatalf("stats %+v", f.driver.Stats())
		}
		oldReg.Close()
		newReg, err := e.host.Register("actionflow/1", 50*time.Millisecond, func(Beat) error { newRuns.Add(1); return nil })
		if err != nil {
			t.Fatalf("register the same name after Close: %v", err)
		}
		newReg.Start()
		if err := oldReg.Start(); !errors.Is(err, ErrRegClosed) || f.driver.Registered() != 1 {
			t.Fatalf("start after Close = %v, registered %d", err, f.driver.Registered())
		}
		close(release)
		f.settle(oldReg)
		if oldRuns.Load() != 0 || newRuns.Load() != 0 || f.driver.Stats().DroppedStale != 1 {
			t.Fatalf("old %d new %d stats %+v", oldRuns.Load(), newRuns.Load(), f.driver.Stats())
		}
		f.stepUntilDelivered(2)
		f.settle(newReg)
		if newRuns.Load() != 1 || oldRuns.Load() != 0 || f.driver.Registered() != 1 {
			t.Fatalf("after close: old %d new %d registered %d", oldRuns.Load(), newRuns.Load(), f.driver.Registered())
		}
	})
}

// Close 在事务里立即生效、不随回滚恢复（与 Register 一样是生命周期操作）；同一事务里更早的 Start 提交后到达也不会把
// 已撤销的登记放回环上。
func TestCloseInsideATransactionIsImmediateAndIgnoresEarlierStarts(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		rebuild := nest.NewHandlerName("hb_test_rebuild")
		f := newNestFixture(t, func(m *nest.NestMgr) {
			m.MustRegisterHandlerWithMeta(rebuild, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
				e := es[0].(*hbEntity)
				old := params[0].(*Reg)
				if err := old.Start(); err != nil {
					return nil, err
				}
				old.Close()
				next, err := e.host.Register(old.Name(), 100*time.Millisecond, func(Beat) error { return nil })
				if err != nil {
					return nil, err
				}
				if err := next.Start(); err != nil {
					return nil, err
				}
				if params[1].(bool) {
					return nil, errBusiness
				}
				return next, nil
			}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
		})
		e := newHBEntity(t, f.driver, 52, kindShort)
		f.manager.Add(e)
		first, _ := e.host.Register("ai", 100*time.Millisecond, func(Beat) error { return nil })
		if _, err := f.engine.Request(context.Background(), rebuild, e.ID(), nest.NewParams(first, true)); !errors.Is(err, errBusiness) {
			t.Fatalf("failing rebuild = %v", err)
		}
		if !first.Closed() || first.Active() || f.driver.Registered() != 0 {
			t.Fatalf("after a rolled-back rebuild: closed %v active %v registered %d", first.Closed(), first.Active(), f.driver.Registered())
		}
		second, _ := e.host.Register("ai/2", 100*time.Millisecond, func(Beat) error { return nil })
		got, err := f.engine.Request(context.Background(), rebuild, e.ID(), nest.NewParams(second, false))
		if err != nil {
			t.Fatal(err)
		}
		if next := got.(*Reg); !second.Closed() || !next.Active() || f.driver.Registered() != 1 {
			t.Fatalf("after a committed rebuild: old closed %v new active %v registered %d", second.Closed(), next.Active(), f.driver.Registered())
		}
	})
}
