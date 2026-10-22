package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 对比三（DESIGN-TODO D20 §7 后续微基准，第 4 期补）：上面两组对比用的是原型结构与原型驱动，这里用正式的
// gameplay/heartbeat.Driver。§7 列的六项与本包的对应：
//
//	BenchmarkWheelAdvance（只测出入环）        → BenchmarkSched op=step / add / remove / settier（原型 TierRing 与正式环同一写法）
//	时间轮与最小堆对照                          → BenchmarkSched impl=wheel10 / heap10 / tier10
//	Dispatch 与 DispatchBroadcast 对照           → TestDeliveryRun HB_MODE=x / z
//	BenchmarkDriverDispatch（假客户端）          → 本文件 BenchmarkDriverDispatch
//	真实 Nest + 空 handler 的开环投递            → 本文件 TestDriverOpenLoop（HB_OPENLOOP=1；不设时跑一个几百毫秒的小档断言）
//	快池被业务占满时的跳拍                       → 本文件 TestBusyShortPoolSkipsBeatsAndResumes（go test 里常跑）
//
// 命令与待补测见 docs/performance/HB-DRIVEN-LOAD-20261021.md。

const driverKind entity.EntityKind = 203

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: driverKind, Category: 1, BusinessPool: entity.BusinessPoolShort})
	nest.MustRegisterMemoryHandler(gateHandler, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		// 只在测试里模拟“快池被业务占满”：占住 worker 直到测试放开（正式业务不得在快池里阻塞）。
		gate := params[0].(*busyGate)
		close(gate.started)
		<-gate.release
		return nil, nil
	})
}

// busyGate 让测试按事件控制快池被占住的区间：消息真正开始执行时关 started，测试关 release 才返回。
type busyGate struct {
	started chan struct{}
	release chan struct{}
}

var gateHandler = nest.NewHandlerName("hb_bench_gate")

// driverEntity 只有实体基座：回调不碰业务状态，量的是驱动与投递本身。
type driverEntity struct{ *entity.EntityBase }

func (e *driverEntity) Base() *entity.EntityBase { return e.EntityBase }

func newDriverEntities(t testing.TB, first, n int) (map[int64]*driverEntity, []*driverEntity) {
	t.Helper()
	byID := make(map[int64]*driverEntity, n)
	list := make([]*driverEntity, n)
	for i := range n {
		id, err := entity.BuildEntityID(int64(first+i), driverKind)
		if err != nil {
			t.Fatal(err)
		}
		e := &driverEntity{EntityBase: entity.NewEntityBase(id, 1, true, driverKind)}
		byID[id], list[i] = e, e
	}
	return byID, list
}

type driverGetter map[int64]*driverEntity

func (g driverGetter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if e := g[id]; e != nil {
		return e, nil
	}
	return nil, fmt.Errorf("entity %d missing", id)
}

func (g driverGetter) GetMany(ctx context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		e, err := g.Get(ctx, id, 0)
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

func (g driverGetter) lookup(id int64) entity.IThreadSafeEntity {
	if e := g[id]; e != nil {
		return e
	}
	return nil
}

// inlineClient 是 BenchmarkDriverDispatch 的假客户端：Dispatch 与 DispatchTryBatch 在驱动 goroutine 上直接调用心跳
// handler（不经 Nest 的队列与 worker，不开事务），所以驱动 goroutine 的耗时 = 环推进 + 核对 + 一次 handler 调用。
// 其他方法不用。
type inlineClient struct {
	lookup func(int64) entity.IThreadSafeEntity
}

func (c inlineClient) Dispatch(_ context.Context, name nest.HandlerName, id int64, params nest.Params, _ ...nest.SendOpt) error {
	_, err := nest.GetHandler(name)([]entity.IThreadSafeEntity{c.lookup(id)}, params)
	return err
}

// DispatchTryBatch 逐个目标直接执行（批量投递方式的驱动侧开销：凭据、目标切片、整批交出）；不交回。
func (c inlineClient) DispatchTryBatch(_ context.Context, targets []nest.TryBatchTarget, _ func(nest.TryBatchTarget, error)) error {
	for _, target := range targets {
		if _, err := nest.GetHandler(target.Handler)([]entity.IThreadSafeEntity{c.lookup(target.ID)}, target.Params); err != nil {
			return err
		}
	}
	return nil
}

var errUnused = errors.New("bench: not used by the heartbeat driver")

func (inlineClient) Request(context.Context, nest.HandlerName, int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}
func (inlineClient) DispatchBroadcast(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (inlineClient) DispatchMulti(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (inlineClient) RequestMulti(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}
func (inlineClient) DispatchMultiGroup(context.Context, nest.HandlerName, [][]int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (inlineClient) RequestMultiGroup(context.Context, nest.HandlerName, [][]int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}

// registerAll 给每个实体登记一个周期 period 的心跳并启动（不在事务里，立即生效）。
func registerAll(t testing.TB, d *heartbeat.Driver, list []*driverEntity, period time.Duration, fn heartbeat.Func) {
	t.Helper()
	for _, e := range list {
		reg, err := d.NewHost(e).Register("bench", period, fn, heartbeat.WithDurability(nest.DurabilityMemory))
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Start(); err != nil {
			t.Fatal(err)
		}
	}
}

// BenchmarkDriverDispatch：正式驱动 + 假客户端（handler 在驱动 goroutine 上直接执行，回调为空），两种投递方式
// （delivery=dispatch / batch）各一组。驱动按真实时间每 10ms 推进一格，所以 ns/op 只是 1 / 目标速率，看的是附加指标：
//
//	cpu-ns/beat   进程 CPU / 拍（≈ 驱动 goroutine：环推进、核对、一次 handler 调用；含 GC）
//	driver-cores  进程 CPU / 墙钟（§7：超过约 0.25 核才考虑 heartbeat.shards）
//	beats/s       实际执行速率（低于目标说明单个驱动追不上）
//
// 命令：go test -run '^$' -bench BenchmarkDriverDispatch -benchtime 2000000x -count 10 ./gameplay/heartbeat/internal/bench
func BenchmarkDriverDispatch(b *testing.B) {
	for _, delivery := range []heartbeat.Delivery{heartbeat.DeliveryDispatch, heartbeat.DeliveryBatch} {
		for _, tc := range []struct {
			regs   int
			period time.Duration
		}{
			{50_000, 100 * time.Millisecond},  // 50 万拍/s（§7 的 5 万实体 10Hz）
			{100_000, 100 * time.Millisecond}, // 100 万拍/s
			{50_000, time.Second},             // 5 万拍/s
		} {
			benchDriverDispatch(b, delivery, tc.regs, tc.period)
		}
	}
}

func benchDriverDispatch(b *testing.B, delivery heartbeat.Delivery, regs int, period time.Duration) {
	b.Run(fmt.Sprintf("delivery=%s/regs=%d/period=%v", delivery, regs, period), func(b *testing.B) {
		byID, list := newDriverEntities(b, 1, regs)
		getter := driverGetter(byID)
		d := heartbeat.NewDriver(heartbeat.Config{Name: "bench-inline", Delivery: delivery})
		var executed atomic.Int64
		registerAll(b, d, list, period, func(heartbeat.Beat) error {
			executed.Add(1)
			return nil
		})
		if err := d.Bind(inlineClient{lookup: getter.lookup}, getter.lookup); err != nil {
			b.Fatal(err)
		}
		if err := d.Start(); err != nil {
			b.Fatal(err)
		}
		defer func() { _ = d.Stop(context.Background()) }()
		// 预热一个周期：每格切片容量进入稳态。
		for executed.Load() < int64(regs) {
			time.Sleep(time.Millisecond)
		}
		b.ResetTimer()
		base, cpu0, wall0 := executed.Load(), cpuTime(), time.Now()
		for executed.Load()-base < int64(b.N) {
			time.Sleep(time.Millisecond)
		}
		wall, cpu, beats := time.Since(wall0), cpuTime()-cpu0, executed.Load()-base
		b.StopTimer()
		b.ReportMetric(float64(cpu)/float64(beats), "cpu-ns/beat")
		b.ReportMetric(float64(cpu)/float64(wall), "driver-cores")
		b.ReportMetric(float64(beats)/wall.Seconds(), "beats/s")
		if s := d.Stats(); s.SkippedInflight+s.SkippedQueueFull+s.SkippedRejected+s.Stalled > 0 {
			b.Fatalf("inline delivery skipped beats: %+v", s)
		}
	})
}

// openLoopResult 是一次开环投递的结果（HB_OUT 时追加一行 JSON）。
type openLoopResult struct {
	Label             string          `json:"label"`
	Rate              int             `json:"target_beats_per_second"`
	Period            string          `json:"period"`
	Regs              int             `json:"registrations"`
	Workers           int             `json:"short_workers"`
	Queue             int             `json:"short_queue"`
	Seconds           float64         `json:"measured_seconds"`
	Executed          int64           `json:"executed"`
	ExecutedPerSecond float64         `json:"executed_per_second"`
	Stats             heartbeat.Stats `json:"driver"`
	// Totals 是驱动从启动到停止的累计计数（Stats 只是测量窗口内的差值）。窗口的起点在拍子在途时取，
	// 窗口前投递、窗口内执行的拍只算进窗口的 Executed，所以“投递数 ≥ 执行数”只对累计值成立。
	Totals heartbeat.Stats `json:"driver_totals"`
	// WrongPeriod 是测量窗口内 Beat.Period 不等于登记周期的拍数（应为 0）。
	WrongPeriod        int64   `json:"wrong_period_beats"`
	IntervalP50MS      float64 `json:"interval_p50_ms"`
	IntervalP99MS      float64 `json:"interval_p99_ms"`
	IntervalMaxMS      float64 `json:"interval_max_ms"`
	ShortPeakWaiting   int     `json:"short_peak_waiting"`
	ShortMaxWorkerWait float64 `json:"short_max_worker_wait_ms"`
	Cores              float64 `json:"process_cores"`
	GOMAXPROCS         int     `json:"gomaxprocs"`
}

// runOpenLoop：真实 Nest 引擎（快池 workers / queue），rate × period 个登记、回调为空（只记 Beat.Elapsed），
// 驱动开环投递 seconds 秒（先预热一个周期）。返回结果；跳拍、卡住与准入失败如实记录，由调用方判断。
func runOpenLoop(t testing.TB, label string, rate int, period time.Duration, workers, queue int, seconds float64, first int) openLoopResult {
	t.Helper()
	regs := max(1, int(float64(rate)*period.Seconds()))
	byID, list := newDriverEntities(t, first, regs)
	getter := driverGetter(byID)
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: workers, QueueCap: queue}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	d := heartbeat.NewDriver(heartbeat.Config{Name: "bench-" + label})
	var measuring atomic.Bool
	var executed atomic.Int64
	var wrongPeriod atomic.Int64 // 拍子带的 Period 不等于登记的周期（接线错误）
	var mu sync.Mutex
	intervals := make([]float64, 0, 1<<16)
	registerAll(t, d, list, period, func(b heartbeat.Beat) error {
		if !measuring.Load() {
			return nil
		}
		executed.Add(1)
		if b.Period != period {
			wrongPeriod.Add(1)
		}
		// 只记相邻两拍之间没有跳拍的间隔：跳过的拍让 Elapsed 跨两个周期，混进来就量不出“间隔接近档位”。
		if !b.First && b.Skipped == 0 {
			mu.Lock()
			if len(intervals) < cap(intervals) {
				intervals = append(intervals, float64(b.Elapsed)/float64(time.Millisecond))
			}
			mu.Unlock()
		}
		return nil
	})
	if err := d.Bind(engine, getter.lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(period)
	measuring.Store(true)
	before := d.Stats()
	cpu0, wall0 := cpuTime(), time.Now()
	time.Sleep(time.Duration(seconds * float64(time.Second)))
	measuring.Store(false)
	wall, cpu := time.Since(wall0), cpuTime()-cpu0
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := d.Stats()
	short := engine.Stats().Queue.Short
	mu.Lock()
	slices.Sort(intervals)
	q := func(p float64) float64 {
		if len(intervals) == 0 {
			return 0
		}
		return intervals[min(len(intervals)-1, int(p*float64(len(intervals))))]
	}
	r := openLoopResult{Label: label, Rate: rate, Period: period.String(), Regs: regs, Workers: workers, Queue: queue, Seconds: wall.Seconds(),
		Executed: executed.Load(), ExecutedPerSecond: float64(executed.Load()) / wall.Seconds(), Stats: after, Totals: after, WrongPeriod: wrongPeriod.Load(),
		IntervalP50MS: q(.5), IntervalP99MS: q(.99), IntervalMaxMS: q(1), ShortPeakWaiting: short.PeakWaiting,
		ShortMaxWorkerWait: float64(short.MaxWorkerWait) / float64(time.Millisecond), Cores: float64(cpu) / float64(wall), GOMAXPROCS: runtime.GOMAXPROCS(0)}
	mu.Unlock()
	r.Stats.Delivered -= before.Delivered
	r.Stats.Executed -= before.Executed
	return r
}

// TestDriverOpenLoop：真实 Nest + 空回调的开环投递（§7：10 / 25 / 50 万拍每秒，记录完成数、跳拍、快池等待峰值、
// 间隔 P99）。HB_OPENLOOP=1 时按环境变量跑一档并把结果追加到 HB_OUT（JSON 行），一个进程一档、由调用方循环并交替：
//
//	HB_OPENLOOP=1 HB_RATE=100000 HB_PERIOD=100ms HB_WORKERS=8 HB_QUEUE=65536 HB_SECONDS=20 HB_OUT=<jsonl> \
//	  go test -run '^TestDriverOpenLoop$' -count=1 -timeout 0 ./gameplay/heartbeat/internal/bench
//
// 不设 HB_OPENLOOP 时跑一个几百毫秒的小档（2000 拍/s）作接线断言：没有卡住与准入失败，累计投递数不少于执行数，
// 每拍带的周期就是登记的档位。在途跳拍（上一拍没执行完就到了下一拍）不在这里断言：它是设计内的让路行为，-race 下宿主一次
// 几十毫秒的停顿就会让整轮登记各跳一拍；跳拍本身由 TestBusyShortPoolSkipsBeatsAndResumes 覆盖，零跳拍属于
// 正式开环测量（HB_OPENLOOP=1）的结论，不是接线断言。
func TestDriverOpenLoop(t *testing.T) {
	if os.Getenv("HB_OPENLOOP") == "" {
		r := runOpenLoop(t, "small", 2000, 50*time.Millisecond, 2, 1024, 0.3, 1_000_000)
		t.Logf("%+v", r)
		s := r.Stats
		if s.SkippedQueueFull+s.SkippedRejected+s.Stalled > 0 || r.Executed == 0 || r.Totals.Delivered < r.Totals.Executed {
			t.Fatalf("small open loop: %+v", r)
		}
		// 周期的接线断言用拍子自带的 Period（确定性）。墙钟间隔只记日志不断言：-race 加宿主 load 15 时 p50 到过
		// 63～100ms（驱动本身被拖晚，不记为跳拍），量的是宿主调度，属于正式开环测量（HB_OPENLOOP=1）的结论。
		if r.WrongPeriod != 0 {
			t.Fatalf("%d beats carried a period other than the registered %v", r.WrongPeriod, 50*time.Millisecond)
		}
		return
	}
	period, err := time.ParseDuration(envOr("HB_PERIOD", "100ms"))
	if err != nil {
		t.Fatal(err)
	}
	r := runOpenLoop(t, envOr("HB_LABEL", "openloop"), envInt(t, "HB_RATE", 100_000), period, envInt(t, "HB_WORKERS", 8), envInt(t, "HB_QUEUE", 65536), envFloat(t, "HB_SECONDS", 20), 1_000_000)
	raw, _ := json.Marshal(r)
	t.Logf("%s", raw)
	if out := os.Getenv("HB_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.Write(append(raw, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestBusyShortPoolSkipsBeatsAndResumes：快池被业务占满时心跳按规则跳拍（§7 最后一项；维护者决定 ④⑤：在途跳拍、
// ErrQueueFull 跳拍，不累积、不补发，没有水位让路），业务放开后照常跳，第一拍报告跳过的拍数、Elapsed 覆盖这段
// 时间（夹在上限内）。快池 1 个 worker、队列 8 格；20 个 50ms 登记；一条业务消息占住 worker（只在测试里这样模拟，
// 正式业务不得在快池里阻塞）。
//
// 各阶段按事件推进，不按墙钟窗口划分：每个登记先跳过一拍 → 占位消息真正开始执行（busyGate.started）→ 驱动累计
// 跳拍达到每个登记两拍 → 放开 → 每个登记在放开之后再跳两拍。宿主负载只拉长各阶段，不改变断言。
func TestBusyShortPoolSkipsBeatsAndResumes(t *testing.T) {
	const period = 50 * time.Millisecond
	const resumeDeadline = 30 * time.Second // 只在驱动真的不再恢复时才会耗尽
	byID, list := newDriverEntities(t, 2_000_000, 21)
	getter := driverGetter(byID)
	blocker, beaters := list[0], list[1:]
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	d := heartbeat.NewDriver(heartbeat.Config{Name: "bench-busy"})
	var mu sync.Mutex
	beats := make(map[int64][]heartbeat.Beat)
	beaten := make(chan struct{}, 1) // 每执行一拍非阻塞地通知一次，等待方据此复查条件
	for _, e := range beaters {
		id := e.ID()
		reg, err := d.NewHost(e).Register("bench", period, func(b heartbeat.Beat) error {
			mu.Lock()
			beats[id] = append(beats[id], b)
			mu.Unlock()
			select {
			case beaten <- struct{}{}:
			default:
			}
			return nil
		}, heartbeat.WithDurability(nest.DurabilityMemory))
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Bind(engine, getter.lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	counts := func() map[int64]int {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[int64]int, len(beats))
		for id, list := range beats {
			out[id] = len(list)
		}
		return out
	}
	// everyBeater 等到每个登记的拍数都比 base 多至少 n；beaten 只是唤醒，条件每次重新数。
	everyBeater := func(what string, base map[int64]int, n int) {
		t.Helper()
		deadline := time.After(resumeDeadline)
		for {
			now, short := counts(), 0
			for _, e := range beaters {
				if now[e.ID()]-base[e.ID()] < n {
					short++
				}
			}
			if short == 0 {
				return
			}
			select {
			case <-beaten:
			case <-deadline:
				t.Fatalf("%s: %d of %d entities ran fewer than %d beats within %v (driver %+v)", what, short, len(beaters), n, resumeDeadline, d.Stats())
			}
		}
	}
	everyBeater("before the pool is busy", nil, 1)

	gate := &busyGate{started: make(chan struct{}), release: make(chan struct{})}
	if err := engine.Dispatch(context.Background(), gateHandler, blocker.ID(), nest.Params{gate}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.started:
	case <-time.After(resumeDeadline):
		t.Fatal("the blocking message never started on the short pool")
	}
	whileBusy := counts()
	// 唯一的快池 worker 被占住，期间不可能有拍执行；驱动每格对每个登记要么在途跳拍、要么队列满跳拍。
	skips := func() int {
		s := d.Stats()
		return int(s.SkippedInflight + s.SkippedQueueFull)
	}
	skipDeadline := time.After(resumeDeadline)
	for skips() < 2*len(beaters) {
		select {
		case <-skipDeadline:
			t.Fatalf("a busy short pool skipped only %d beats within %v: %+v", skips(), resumeDeadline, d.Stats())
		case <-time.After(period):
		}
	}
	released := counts()
	for id, n := range released {
		if n != whileBusy[id] {
			t.Fatalf("entity %d ran %d beats while the only short worker was busy", id, n-whileBusy[id])
		}
	}
	close(gate.release)
	everyBeater("after the pool was released", released, 2)
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := d.Stats()
	t.Logf("driver %+v", s)
	if s.Stalled != 0 || s.SkippedRejected != 0 || s.Delivered != s.Executed {
		t.Fatalf("busy short pool: stalled %d rejected %d delivered %d executed %d", s.Stalled, s.SkippedRejected, s.Delivered, s.Executed)
	}
	mu.Lock()
	defer mu.Unlock()
	resumedWithSkips := 0
	for id, list := range beats {
		for i, b := range list {
			if !b.First && b.Elapsed > heartbeat.DefaultMaxElapsed {
				t.Fatalf("entity %d beat %d: Elapsed %v over the %v cap", id, i, b.Elapsed, heartbeat.DefaultMaxElapsed)
			}
		}
		// 放开后的第一拍报告占用期间跳过的拍数；Elapsed 覆盖这些拍，被上限夹住时等于上限。
		first := list[released[id]]
		if first.Skipped > 0 {
			resumedWithSkips++
			if want := min(time.Duration(first.Skipped-1)*period, heartbeat.DefaultMaxElapsed); first.Elapsed < want {
				t.Fatalf("entity %d: first beat after %d skips covers only %v, want at least %v", id, first.Skipped, first.Elapsed, want)
			}
		}
	}
	if resumedWithSkips == 0 {
		t.Fatal("no beat after the busy period reported its skipped beats")
	}
}
