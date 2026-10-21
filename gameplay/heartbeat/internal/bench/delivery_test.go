package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 对比二：投递方式（经真实 Nest 引擎）。一次进程只跑一组参数，由脚本循环调用并交替顺序：
//
//	HB_DELIVERY=1 HB_MODE=x|y|z|c|none HB_ACTIVE=0.2 HB_RES=10ms HB_SHARDS=1 HB_OUT=<jsonl> \
//	  go test -run TestDeliveryRun -count=1 -timeout 0 ./gameplay/heartbeat/internal/bench
//
// 模式：
//
//	x     每拍每个到期登记一条单目标 nest.Dispatch（正式链路：准入、同 ID 顺序、实体锁、事务收尾）
//	y     ssr 式：每条消息带一批（HB_BATCH，缺省 64）登记，投给“承载实体”（HB_CARRIERS 个，轮转），
//	      handler 在快池 worker 上逐个对目标实体 TryLock、执行回调、解锁；TryLock 失败的登记交回驱动，
//	      下一格重投（handler 内不能 Dispatch）。**绕过 Nest 对目标实体的准入顺序、Guard 与事务**，见报告风险一节
//	z     nest.DispatchBroadcast：本格到期的全部实体一次调用，Nest 按锁组与 worker 散列拆成至多 32 个一批，
//	      每批一个作业（等批内全部前驱），worker 上逐个加锁、每个目标各自一个事务
//	c     D20 选项 C：nest.DispatchTryBatch，每 HB_BATCH（缺省 64）个到期登记一个 Nest 批量作业，worker 逐目标在
//	      目标自己的 Guard 作用域与事务里执行；同 ID 冲突在 Nest 里当场转成单目标作业（计 c_converted_per_s），
//	      交回的（转换被拒等）清在途、计 hb_queue_full_total
//	none  不挂心跳，只有业务消息（基线）
//
// 负载：HB_ENTITIES（缺省 5 万）个实体，其中 HB_ACTIVE 比例各有一个 100ms 登记（移动）；另有一路业务消息
// HB_BIZ_RATE 条/秒（缺省 5000）随机打到全部实体（单目标 Dispatch），测它从发出到 handler 开始执行的延迟。
// 回调只做少量内存计算（按经过时间推进位置并在边界反弹）。

const (
	hbKind      entity.EntityKind = 201
	carrierKind entity.EntityKind = 202
)

var (
	hbHandler      = nest.NewHandlerName("hb_bench_beat")
	carrierHandler = nest.NewHandlerName("hb_bench_carrier")
	bizHandler     = nest.NewHandlerName("hb_bench_biz")
	run            *deliveryRun // 一个进程只跑一组；handler 经它找到计数器
)

func init() {
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: hbKind, Category: 1, BusinessPool: entity.BusinessPoolShort},
		entity.EntityKindDef{Kind: carrierKind, Category: 1, BusinessPool: entity.BusinessPoolShort},
	)
	nest.MustRegisterMemoryHandler(hbHandler, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		e := es[0].(*hbEntity)
		run.beat(e, e.reg)
		return nil, nil
	})
	nest.MustRegisterMemoryHandler(carrierHandler, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		run.carry(params[0].(*carrierBatch))
		return nil, nil
	})
	nest.MustRegisterMemoryHandler(bizHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		run.biz(es[0].(*hbEntity), params[0].(int64))
		return nil, nil
	})
}

type hbEntity struct {
	*entity.EntityBase
	x, y, vx, vy float64
	last         int64 // 上次回调的时刻（相对 origin 的纳秒），只在实体锁内读写
	work         uint64
	reg          *hbReg
}

func (e *hbEntity) Base() *entity.EntityBase { return e.EntityBase }

type hbReg struct {
	ent      *hbEntity
	id       int64 // 实体 ID
	index    int
	inflight atomic.Bool
	planned  int64 // 本拍计划时刻（相对 origin 的纳秒）；驱动写、回调读，经 Nest 准入建立先后
	params   nest.Params
}

type carrierBatch struct {
	regs  []*hbReg
	shard *driverShard
}

type getter map[int64]entity.IThreadSafeEntity

func (g getter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if e := g[id]; e != nil {
		return e, nil
	}
	return nil, fmt.Errorf("entity %d missing", id)
}

func (g getter) GetMany(ctx context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
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

// --- 直方图：按实体分 64 片、原子累加，测量窗口外不记 ---

const (
	histShards  = 64
	histBuckets = 10_000 + 19_900 + 1 // <10ms 每 1µs 一格；10ms..2s 每 100µs 一格；其余一格
)

type histogram struct {
	shards [histShards]*[histBuckets]atomic.Uint64
}

func newHistogram() *histogram {
	h := &histogram{}
	for i := range h.shards {
		h.shards[i] = new([histBuckets]atomic.Uint64)
	}
	return h
}

func bucketOf(ns int64) int {
	us := max(ns, 0) / 1000
	switch {
	case us < 10_000:
		return int(us)
	case us < 2_000_000:
		return 10_000 + int((us-10_000)/100)
	default:
		return histBuckets - 1
	}
}

func bucketUpper(b int) float64 { // 毫秒
	switch {
	case b < 10_000:
		return float64(b+1) / 1000
	case b < histBuckets-1:
		return float64(10_000+(b-10_000+1)*100) / 1000
	default:
		return math.Inf(1)
	}
}

func (h *histogram) add(shard int, ns int64) { h.shards[shard%histShards][bucketOf(ns)].Add(1) }

func (h *histogram) quantiles(qs ...float64) (count uint64, out []float64) {
	var merged [histBuckets]uint64
	for _, s := range h.shards {
		for b := range s {
			v := s[b].Load()
			merged[b] += v
			count += v
		}
	}
	out = make([]float64, len(qs))
	for i, q := range qs {
		target := uint64(math.Ceil(q * float64(count)))
		var acc uint64
		for b, v := range merged {
			acc += v
			if acc >= target && target > 0 {
				out[i] = bucketUpper(b)
				break
			}
		}
	}
	return count, out
}

// --- 分片计数器 ---

type paddedCounter struct {
	n atomic.Uint64
	_ [56]byte
}

type counter [histShards]paddedCounter

func (c *counter) add(shard int, v uint64) { c[shard%histShards].n.Add(v) }
func (c *counter) sum() (s uint64) {
	for i := range c {
		s += c[i].n.Load()
	}
	return s
}

// --- 一次运行 ---

type deliveryConfig struct {
	Mode      string  `json:"mode"`
	Entities  int     `json:"entities"`
	Active    float64 `json:"active"`
	Res       string  `json:"res"`
	Shards    int     `json:"shards"`
	Workers   int     `json:"workers"`
	Batch     int     `json:"batch"`
	Carriers  int     `json:"carriers"`
	BizRate   int     `json:"biz_rate"`
	WarmS     float64 `json:"warm_s"`
	MeasureS  float64 `json:"measure_s"`
	Gomaxproc int     `json:"gomaxprocs"`
	Label     string  `json:"label"`
}

type deliveryRun struct {
	cfg       deliveryConfig
	res       time.Duration
	period    int64
	origin    time.Time
	measuring atomic.Bool
	mgr       *nest.NestMgr
	entities  []*hbEntity
	regs      []*hbReg
	carriers  []int64
	shards    []*driverShard

	interval, absDev, lateness, bizLatency *histogram
	beats, skipped, queueFull, admitErr    counter
	tryFail, admissions, bizSent, bizFail  counter
	handedBack                             counter
	driverBusy, genBusy                    atomic.Int64 // 纳秒，只计测量窗口
	batchPool                              sync.Pool
}

type driverShard struct {
	idx     int
	sched   *TierRing
	regs    []*hbReg // 下标 = 本片登记序号（Step 返回的 id）
	retryM  sync.Mutex
	retry   []*hbReg // y 模式 TryLock 失败、等待重投的登记
	ids     []int64
	elig    []*hbReg
	targets []nest.TryBatchTarget
	carry   int
}

func (r *deliveryRun) now() int64 { return int64(time.Since(r.origin)) }

// beat 是心跳回调（x / z 在 Nest handler 里、y 在承载 handler 持目标锁时调用）：清在途、记间隔与偏差、推进位置。
func (r *deliveryRun) beat(e *hbEntity, reg *hbReg) {
	now := r.now()
	reg.inflight.Store(false)
	shard := reg.index
	if r.measuring.Load() {
		r.beats.add(shard, 1)
		r.lateness.add(shard, now-reg.planned)
		if e.last != 0 {
			d := now - e.last
			r.interval.add(shard, d)
			r.absDev.add(shard, int64(math.Abs(float64(d-r.period))))
		}
	}
	dt := 0.1
	if e.last != 0 {
		dt = min(float64(now-e.last)/1e9, 1)
	}
	e.last = now
	e.x += e.vx * dt
	e.y += e.vy * dt
	if e.x < 0 || e.x > 1000 {
		e.vx = -e.vx
	}
	if e.y < 0 || e.y > 1000 {
		e.vy = -e.vy
	}
	x := e.work | 1
	for range 32 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	e.work = x
}

// carry 是 y 模式的承载 handler：逐个 TryLock 目标实体的锁（不经 Guard、不开事务），拿不到就交回驱动重投。
func (r *deliveryRun) carry(b *carrierBatch) {
	var failed []*hbReg
	for _, reg := range b.regs {
		mu := reg.ent.GetMutex()
		if !mu.TryLock() {
			failed = append(failed, reg)
			continue
		}
		if !reg.ent.IsRemoved() {
			r.beat(reg.ent, reg)
		}
		mu.Unlock()
	}
	if len(failed) > 0 {
		if r.measuring.Load() {
			r.tryFail.add(failed[0].index, uint64(len(failed)))
		}
		b.shard.retryM.Lock()
		b.shard.retry = append(b.shard.retry, failed...)
		b.shard.retryM.Unlock()
	}
	b.regs = b.regs[:0]
	r.batchPool.Put(b)
}

// handBack 是 c 模式的交回回调（Nest worker 上调用）：没有执行的拍清在途、计数。
func (r *deliveryRun) handBack(target nest.TryBatchTarget, _ error) {
	reg := target.Params[0].(*hbReg)
	reg.inflight.Store(false)
	if r.measuring.Load() {
		r.handedBack.add(reg.index, 1)
	}
}

func (r *deliveryRun) biz(e *hbEntity, sent int64) {
	now := r.now()
	if r.measuring.Load() {
		r.bizLatency.add(int(e.UniqueID()), now-sent)
	}
	e.work += uint64(now)
}

// deliver 把一格到期的登记按模式投出去。
func (r *deliveryRun) deliver(s *driverShard, due []int64, planned int64) {
	measuring := r.measuring.Load()
	ctx := context.Background()
	s.elig = s.elig[:0]
	if r.cfg.Mode == "y" {
		s.retryM.Lock()
		s.elig = append(s.elig, s.retry...) // 重投的登记保持在途、保持原计划时刻
		s.retry = s.retry[:0]
		s.retryM.Unlock()
	}
	for _, id := range due {
		reg := s.regs[id]
		if !reg.inflight.CompareAndSwap(false, true) {
			if measuring {
				r.skipped.add(reg.index, 1)
			}
			continue
		}
		reg.planned = planned
		s.elig = append(s.elig, reg)
	}
	switch r.cfg.Mode {
	case "x":
		for _, reg := range s.elig {
			if err := r.mgr.Dispatch(ctx, hbHandler, reg.id, reg.params); err != nil {
				reg.inflight.Store(false)
				if measuring {
					r.queueFull.add(reg.index, 1)
				}
			}
		}
		if measuring {
			r.admissions.add(s.idx, uint64(len(s.elig)))
		}
	case "z":
		if len(s.elig) == 0 {
			return
		}
		s.ids = s.ids[:0]
		for _, reg := range s.elig {
			s.ids = append(s.ids, reg.id)
		}
		if err := r.mgr.DispatchBroadcast(ctx, hbHandler, s.ids, nil); err != nil && measuring {
			// 部分批次可能已准入，原型不区分；正式实现需要逐批结果。本轮各组都应为 0。
			r.admitErr.add(s.idx, 1)
		}
		if measuring {
			r.admissions.add(s.idx, uint64((len(s.ids)+nest.MaxBroadcastIdNum-1)/nest.MaxBroadcastIdNum))
		}
	case "c":
		for off := 0; off < len(s.elig); off += r.cfg.Batch {
			end := min(off+r.cfg.Batch, len(s.elig))
			s.targets = s.targets[:0]
			for _, reg := range s.elig[off:end] {
				s.targets = append(s.targets, nest.TryBatchTarget{Handler: hbHandler, ID: reg.id, Params: reg.params})
			}
			if err := r.mgr.DispatchTryBatch(ctx, s.targets, r.handBack); err != nil {
				for _, reg := range s.elig[off:end] {
					reg.inflight.Store(false)
				}
				if measuring {
					r.admitErr.add(s.idx, 1)
				}
			} else if measuring {
				r.admissions.add(s.idx, 1)
			}
		}
	case "y":
		for off := 0; off < len(s.elig); off += r.cfg.Batch {
			end := min(off+r.cfg.Batch, len(s.elig))
			b := r.batchPool.Get().(*carrierBatch)
			b.shard = s
			b.regs = append(b.regs[:0], s.elig[off:end]...)
			carrier := r.carriers[s.carry%len(r.carriers)]
			s.carry++
			if err := r.mgr.Dispatch(ctx, carrierHandler, carrier, nest.Params{b}); err != nil {
				for _, reg := range b.regs {
					reg.inflight.Store(false)
				}
				if measuring {
					r.queueFull.add(s.idx, uint64(len(b.regs)))
				}
				b.regs = b.regs[:0]
				r.batchPool.Put(b)
			} else if measuring {
				r.admissions.add(s.idx, 1)
			}
		}
	}
}

func (r *deliveryRun) driverLoop(s *driverShard, stop <-chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	ticker := time.NewTicker(r.res)
	defer ticker.Stop()
	var cur int64
	buf := make([]int64, 0, 4096)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		start := time.Now()
		target := int64(time.Since(r.origin) / r.res)
		for cur < target {
			cur++
			buf = s.sched.Step(buf[:0])
			r.deliver(s, buf, cur*int64(r.res))
		}
		if r.measuring.Load() {
			r.driverBusy.Add(int64(time.Since(start)))
		}
	}
}

// bizLoop 每毫秒发 rate/1000 条业务消息（余数累积），目标在全部实体里伪随机。
func (r *deliveryRun) bizLoop(stop <-chan struct{}, done *sync.WaitGroup) {
	defer done.Done()
	if r.cfg.BizRate <= 0 {
		return
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	ctx := context.Background()
	x := uint64(0x2545F4914F6CDD1D)
	start := time.Now()
	var sent int64
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		t0 := time.Now()
		want := int64(time.Since(start).Seconds() * float64(r.cfg.BizRate))
		for ; sent < want; sent++ {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			e := r.entities[x%uint64(len(r.entities))]
			if err := r.mgr.Dispatch(ctx, bizHandler, e.ID(), nest.Params{r.now()}); err != nil {
				if r.measuring.Load() {
					r.bizFail.add(0, 1)
				}
			} else if r.measuring.Load() {
				r.bizSent.add(0, 1)
			}
		}
		if r.measuring.Load() {
			r.genBusy.Add(int64(time.Since(t0)))
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, key string, def int) int {
	v, err := strconv.Atoi(envOr(key, strconv.Itoa(def)))
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return v
}

func envFloat(t *testing.T, key string, def float64) float64 {
	v, err := strconv.ParseFloat(envOr(key, strconv.FormatFloat(def, 'f', -1, 64)), 64)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return v
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestDeliveryRun(t *testing.T) {
	if os.Getenv("HB_DELIVERY") == "" {
		t.Skip("set HB_DELIVERY=1 to run the delivery benchmark (see file comment)")
	}
	cfg := deliveryConfig{
		Mode:      envOr("HB_MODE", "x"),
		Entities:  envInt(t, "HB_ENTITIES", 50_000),
		Active:    envFloat(t, "HB_ACTIVE", 0.2),
		Res:       envOr("HB_RES", "10ms"),
		Shards:    envInt(t, "HB_SHARDS", 1),
		Workers:   envInt(t, "HB_WORKERS", 8),
		Batch:     envInt(t, "HB_BATCH", 64),
		Carriers:  envInt(t, "HB_CARRIERS", 64),
		BizRate:   envInt(t, "HB_BIZ_RATE", 5000),
		WarmS:     envFloat(t, "HB_WARM_S", 3),
		MeasureS:  envFloat(t, "HB_MEASURE_S", 15),
		Gomaxproc: runtime.GOMAXPROCS(0),
		Label:     os.Getenv("HB_LABEL"),
	}
	res, err := time.ParseDuration(cfg.Res)
	if err != nil {
		t.Fatal(err)
	}
	r := &deliveryRun{
		cfg: cfg, res: res, period: int64(Periods[Tier100ms]),
		interval: newHistogram(), absDev: newHistogram(), lateness: newHistogram(), bizLatency: newHistogram(),
	}
	r.batchPool.New = func() any { return &carrierBatch{regs: make([]*hbReg, 0, cfg.Batch)} }
	run = r

	g := make(getter, cfg.Entities+cfg.Carriers)
	for i := range cfg.Entities {
		id, err := entity.BuildEntityID(int64(i+1), hbKind)
		if err != nil {
			t.Fatal(err)
		}
		x := uint64(i+1) * 0x9E3779B97F4A7C15
		e := &hbEntity{
			EntityBase: entity.NewEntityBase(id, 1, true, hbKind),
			x:          float64(x%1000) + 0.5, y: float64((x>>20)%1000) + 0.5,
			vx: float64(x>>40%7) + 1, vy: float64(x>>50%5) + 1,
		}
		g[id] = e
		r.entities = append(r.entities, e)
	}
	for i := range cfg.Carriers {
		id, err := entity.BuildEntityID(int64(i+1), carrierKind)
		if err != nil {
			t.Fatal(err)
		}
		g[id] = &hbEntity{EntityBase: entity.NewEntityBase(id, 1, true, carrierKind)}
		r.carriers = append(r.carriers, id)
	}
	r.mgr = nest.NewEngine(nest.NestOptionWithGetter(g),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: cfg.Workers}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	if err := r.mgr.Start(); err != nil {
		t.Fatal(err)
	}

	r.origin = time.Now()
	for i := range cfg.Shards {
		r.shards = append(r.shards, &driverShard{idx: i, sched: NewTierRing(res)})
	}
	if cfg.Mode != "none" {
		active := int(math.Round(cfg.Active * float64(cfg.Entities)))
		for i := range active {
			// 均匀挑出 active 个实体（步长取整），片按实体序号取模，与相位散列无关。
			e := r.entities[i*cfg.Entities/active]
			s := r.shards[i%cfg.Shards]
			reg := &hbReg{ent: e, id: e.ID(), index: i}
			reg.params = nest.Params{reg}
			e.reg = reg
			s.sched.Add(int64(len(s.regs)), PhaseHash(e.ID()), Tier100ms)
			s.regs = append(s.regs, reg)
			r.regs = append(r.regs, reg)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, s := range r.shards {
		wg.Add(1)
		go r.driverLoop(s, stop, &wg)
	}
	wg.Add(1)
	go r.bizLoop(stop, &wg)

	// 队列等待数采样（1ms）：只在测量窗口内取峰值；另读 Nest 自己记的准入峰值（覆盖整个运行）。
	var samplePeak atomic.Int64
	sampleStop := make(chan struct{})
	var sampleWG sync.WaitGroup
	sampleWG.Add(1)
	go func() {
		defer sampleWG.Done()
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-sampleStop:
				return
			case <-tk.C:
			}
			if r.measuring.Load() {
				if q := int64(r.mgr.Stats().Short.QueueLen); q > samplePeak.Load() {
					samplePeak.Store(q)
				}
			}
		}
	}()

	time.Sleep(time.Duration(cfg.WarmS * float64(time.Second)))
	cpu0, wall0 := cpuTime(), time.Now()
	converted0 := r.mgr.Stats().Queue.Short.TryBatchConverted
	r.measuring.Store(true)
	time.Sleep(time.Duration(cfg.MeasureS * float64(time.Second)))
	r.measuring.Store(false)
	cpu1, wall := cpuTime(), time.Since(wall0)
	converted1 := r.mgr.Stats().Queue.Short.TryBatchConverted
	close(stop)
	wg.Wait()
	close(sampleStop)
	sampleWG.Wait()
	stats := r.mgr.Stats()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.mgr.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	sec := wall.Seconds()
	out := map[string]any{"config": cfg, "wall_s": sec}
	nIv, iv := r.interval.quantiles(0.5, 0.99, 0.999, 1)
	_, dv := r.absDev.quantiles(0.5, 0.99, 0.999)
	_, lt := r.lateness.quantiles(0.5, 0.99, 0.999)
	nBiz, bz := r.bizLatency.quantiles(0.5, 0.99, 0.999, 1)
	out["hb_beats_per_s"] = float64(r.beats.sum()) / sec
	out["hb_admissions_per_s"] = float64(r.admissions.sum()) / sec
	out["hb_skipped_per_s"] = float64(r.skipped.sum()) / sec
	out["hb_skipped_total"] = r.skipped.sum()
	out["hb_queue_full_total"] = r.queueFull.sum()
	out["hb_admit_err_total"] = r.admitErr.sum()
	out["y_trylock_fail_total"] = r.tryFail.sum()
	out["c_handed_back_total"] = r.handedBack.sum()
	out["c_converted_per_s"] = float64(converted1-converted0) / sec
	out["interval_samples"] = nIv
	out["interval_ms"] = map[string]float64{"p50": iv[0], "p99": iv[1], "p999": iv[2], "max": iv[3]}
	out["absdev_ms"] = map[string]float64{"p50": dv[0], "p99": dv[1], "p999": dv[2]}
	out["lateness_ms"] = map[string]float64{"p50": lt[0], "p99": lt[1], "p999": lt[2]}
	out["biz_samples"] = nBiz
	out["biz_sent_per_s"] = float64(r.bizSent.sum()) / sec
	out["biz_fail_total"] = r.bizFail.sum()
	out["biz_latency_ms"] = map[string]float64{"p50": bz[0], "p99": bz[1], "p999": bz[2], "max": bz[3]}
	out["cpu_cores_process"] = (cpu1 - cpu0).Seconds() / sec
	out["driver_cores"] = float64(r.driverBusy.Load()) / 1e9 / sec
	out["biz_gen_cores"] = float64(r.genBusy.Load()) / 1e9 / sec
	out["queue_peak_sampled"] = samplePeak.Load()
	out["queue_peak_admission"] = stats.Queue.Short.PeakWaiting
	out["short_rejected"] = stats.Queue.Short.Rejected
	out["short_max_worker_wait_ms"] = float64(stats.Queue.Short.MaxWorkerWait) / 1e6
	out["short_max_dependency_wait_ms"] = float64(stats.Queue.Short.MaxDependencyWait) / 1e6
	line, _ := json.Marshal(out)
	t.Log(string(line))
	if path := os.Getenv("HB_OUT"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}
