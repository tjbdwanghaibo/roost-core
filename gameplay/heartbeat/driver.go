// Package heartbeat 是按实体的心跳（DESIGN-TODO D20 第 1 期，维护者 2026-10-21 定方案）：所有跑在 Nest 业务池上的
// 实体周期逻辑（移动、actionflow / AI、周期效果、实体自己的逻辑帧）的底层。
//
// 三件事分在三处：
//
//   - 登记：组件在实体加载（OnInitFinish）时经 Host.Register 得到 Reg，按需要 Start / Stop / SetPeriod；
//     卸载、删档时 Host.OnDestroy 撤销。心跳本身什么都不持久，加载后由组件重新登记。
//   - 调度：Driver 一个 goroutine，按档分环、10ms 一格；档位固定（Periods），相位 = hash(实体 ID)。
//   - 执行：到期的登记各投一条单目标 Nest 消息（P1 用 nest.Client.Dispatch，不合并），回调在实体自己的 worker 上、
//     实体锁与事务内执行；快池实体在快池、慢池实体在慢池（Nest 按实体种类分流）。
//
// 每个登记至多一拍在途：上一拍没执行完就跳过这一拍，不累积、不补发，计数并限频打 Warn；准入失败（快池 ErrQueueFull、
// 引擎停止中）同样按跳拍处理。框架不追帧，回调按 Beat.Elapsed 算一次。持久、准点、离线也要算的到期动作用
// framework/nest/entitytimer，不用心跳。
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const (
	// DefaultStallAfter 是在途的拍多久没有执行就视为丢失、重新投递。正常情况下在途的拍都会执行（快池不阻塞）；丢失只来自
	// Nest 在执行 handler 之前放弃了消息，例如 Remote 托管实体的写准入被拒、目标在准入后被卸载且重载失败。重投后
	// 两份消息至多一份执行回调（在途标志在执行时核对）。
	//
	// 拍没丢、只是排得久（worker 跟不上）时重投会多压一份消息：同一登记连续卡住时阈值逐次翻倍（10s、20s、40s，
	// 上限 maxStallBackoff 次），一拍执行后复位，所以积压时每个登记多出的消息是对数级的。-race 下 5 万实体 1s 档的
	// 冒烟里，2s 阈值不翻倍时重投与积压互相放大（2026-10-21 实测，见 D20 实施记录）。
	DefaultStallAfter = 10 * time.Second
	// maxStallBackoff 是阈值翻倍的次数上限（10s × 2^3 = 80s）。
	maxStallBackoff = 3
	// DefaultWarnInterval 是跳拍 / 卡住 Warn 日志的最小间隔：间隔内的跳拍只计数，下一条 Warn 汇总。
	DefaultWarnInterval = 10 * time.Second
	// flushFrames 是驱动把内部计数刷到 metrics 注册表的间隔（100 格 = 1 秒）。
	flushFrames = int64(time.Second / Resolution)
)

const (
	// DeliveredMetric 计准入成功的心跳消息，标签 driver。
	DeliveredMetric = "heartbeat.delivered_total"
	// ExecutedMetric 计执行了回调的拍。
	ExecutedMetric = "heartbeat.executed_total"
	// SkippedMetric 计跳过的拍，标签 reason：inflight（上一拍还没执行完）、queue_full（快池准入满）、
	// rejected（其他准入失败，例如引擎停止中）。持续增长说明实体的 worker 忙不过来或回调太慢；同时打限频 Warn。
	SkippedMetric = "heartbeat.skipped_total"
	// StalledMetric 计在途超过 StallAfter 视为丢失而重投的拍；同时打限频 Warn。基线应为零。
	StalledMetric = "heartbeat.stalled_total"
	// DroppedMetric 计到期或执行时按规则丢弃的拍，标签 reason：removed（实例已卸载 / 删档）、stale（目标已被同 ID
	// 的新实例取代，或实例已 OnDestroy）、not_loaded（实例还没发布）、stopped（停止生效后到达）、not_owner
	// （Remote 托管实体本进程不是当前拥有方）、duplicate（同一次投递的另一份已执行）。不打日志。
	DroppedMetric = "heartbeat.dropped_total"
	// StoppedMetric 计框架自动停掉的登记，标签 reason：remote_shared（Remote 托管实体进入共享模式，不允许挂载心跳，
	// 见 ErrHeartbeatRemoteShared）。同时汇总打 Warn。
	StoppedMetric = "heartbeat.stopped_total"
	// RegisteredMetric 是正在跳（已启动生效）的登记数（Gauge）。
	RegisteredMetric = "heartbeat.registered"
)

// ErrNotBound 表示驱动还没有接上 Nest（Bind）就被启动。
var ErrNotBound = errors.New("heartbeat: driver is not bound to a nest client")

// Config 是 NewDriver 的输入。
type Config struct {
	// Name 用于日志与指标标签。必填。
	Name string
	// StallAfter 见 DefaultStallAfter；0 取缺省。
	StallAfter time.Duration
	// WarnInterval 见 DefaultWarnInterval；0 取缺省。
	WarnInterval time.Duration
	// Logger 是跳拍 Warn 的输出；nil 用 slog.Default()。
	Logger *slog.Logger
}

// Lookup 只读内存里已发布的实体：不加载、不等待（ManagerAccess.Manager().Get）。
type Lookup func(id int64) entity.IThreadSafeEntity

// beat 是一格到期、核对通过、准备交给 Nest 的一拍。
type beat struct {
	reg *Reg
}

// deliverer 把一格到期的一批心跳一次交出。P1 的 dispatchDeliverer 逐个 nest.Client.Dispatch；Nest 批量入队
// （DESIGN-TODO D20 方案 D）落地后只换这一层。rejected 对每个没有准入的拍调用一次（驱动按跳拍处理）。
type deliverer interface {
	deliverBatch(ctx context.Context, batch []beat, rejected func(beat, error))
}

// dispatchDeliverer 是 P1 的投递：每拍一条单目标 Dispatch，走 Nest 正式准入。
type dispatchDeliverer struct{ client nest.Client }

func (p dispatchDeliverer) deliverBatch(ctx context.Context, batch []beat, rejected func(beat, error)) {
	for i, b := range batch {
		if err := ctx.Err(); err != nil {
			// 停止中：剩下的不再投，按准入失败处理（清在途）。
			for _, rest := range batch[i:] {
				rejected(rest, err)
			}
			return
		}
		if err := p.client.Dispatch(ctx, b.reg.handler, b.reg.host.id, b.reg.params); err != nil {
			rejected(b, err)
		}
	}
}

// Driver 是一个 Nest 引擎上的心跳驱动（进程里每个引擎一个），由 wiring/heartbeat 装配。
type Driver struct {
	config     Config
	stallAfter int64 // 帧
	now        atomic.Pointer[func() time.Time]

	mu      sync.Mutex
	ring    *tierRing
	deliver deliverer
	lookup  Lookup
	cancel  context.CancelFunc
	done    chan struct{}
	// lastDone 是最近一次启动的循环的退出信号，Stop 之后保留，Start 据此拒绝与未退出的旧循环并存。
	lastDone chan struct{}

	stats driverStats
	// sharedSample 是最近一个因共享模式被停掉的登记（回调侧写、flush 读后清），Warn 的样本。
	sharedSample atomic.Pointer[Reg]

	// 以下只由驱动 goroutine（循环或测试直接调用 step / flush）读写。
	due     []*Reg
	batch   []beat
	flushed [numCounters]int64
	warn    warnState
}

// NewDriver 校验配置并返回一个未接线的驱动；配置错是接线错误，panic。
func NewDriver(config Config) *Driver {
	if config.Name == "" {
		panic("heartbeat: driver name is required")
	}
	if config.StallAfter <= 0 {
		config.StallAfter = DefaultStallAfter
	}
	if config.WarnInterval <= 0 {
		config.WarnInterval = DefaultWarnInterval
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	d := &Driver{config: config, ring: newTierRing()}
	d.stallAfter = max(int64(config.StallAfter/Resolution), 1)
	now := clock.Now
	d.now.Store(&now)
	return d
}

// Name 返回 Config.Name。
func (d *Driver) Name() string { return d.config.Name }

// SetClock 换掉业务时钟（缺省 clock.Now）。进程装配传入 app.BusinessClock，测试传入可控时钟。Beat.Now / Elapsed
// 按它计算；驱动何时醒来按系统单调时间，不受它影响。
func (d *Driver) SetClock(now func() time.Time) {
	if now != nil {
		d.now.Store(&now)
	}
}

// Now 是驱动的业务时间。
func (d *Driver) Now() time.Time { return (*d.now.Load())() }

// Bind 接上投递用的 Nest 客户端与只读实体查找。进程装配在 Nest 可用之后调用一次；可以重复调用（测试）。
func (d *Driver) Bind(client nest.Client, lookup Lookup) error {
	if client == nil || lookup == nil {
		return fmt.Errorf("heartbeat: driver %s: nest client and entity lookup are required", d.config.Name)
	}
	d.mu.Lock()
	d.deliver, d.lookup = dispatchDeliverer{client: client}, lookup
	d.mu.Unlock()
	return nil
}

// Start 启动驱动 goroutine。已经在运行时返回 nil。
func (d *Driver) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deliver == nil {
		return ErrNotBound
	}
	if d.cancel != nil {
		return nil
	}
	if d.lastDone != nil {
		select {
		case <-d.lastDone:
		default:
			// 上一次 Stop 等到超时返回、旧循环还没退出：不能再起一个（两个循环会同时推进同一个环）。
			return errors.New("heartbeat: driver " + d.config.Name + " is still stopping")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.cancel, d.done, d.lastDone = cancel, done, done
	go d.loop(ctx, done)
	return nil
}

// Stop 停止驱动并等它退出（或等到 ctx 结束，返回 ctx 的错误，驱动随后自行退出）。正在交出的一批里还没投的拍不再投、
// 清掉在途标志；Stop 返回之后驱动不再调用 Dispatch。登记保留：再次 Start 后照常跳。
func (d *Driver) Stop(ctx context.Context) error {
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.cancel, d.done = nil, nil
	d.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Driver) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(Resolution)
	defer ticker.Stop()
	origin := time.Now()
	var stepped int64
	defer func() { d.flush(time.Now()) }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// 按单调时间追到应到的格：驱动被拖慢时逐格补走，每格照常核对在途，所以不会给同一登记连投多拍。
		target := int64(time.Since(origin) / Resolution)
		for stepped < target && ctx.Err() == nil {
			stepped++
			d.step(ctx)
			if stepped%flushFrames == 0 {
				d.flush(time.Now())
			}
		}
	}
}

// step 推进一格：取出到期的登记，逐个核对，一批交给投递层。只由驱动 goroutine 调用（测试可以不启动循环直接调用）。
//
// 核对（不持有 Driver.mu，不阻塞）：
//   - 实例已 OnDestroy、已卸载 / 删档，或查到的是同 ID 的另一个实例：丢弃，不唤醒不在内存的实体；还没发布也不投；
//   - Remote 托管实体的实例处于转移中 / 已隔离：本进程不是拥有方，不投；处于共享模式：照常投一拍，由 handler 在
//     实体锁内停掉本登记（共享模式不允许挂载心跳，见 runBeat）；
//   - 上一拍在途：跳拍（计数、限频 Warn）；在途超过 StallAfter（连续卡住时翻倍）：视为丢失，重投一次（计 stalled）。
func (d *Driver) step(ctx context.Context) {
	d.mu.Lock()
	d.due = d.ring.step(d.due[:0])
	frame := d.ring.frame
	deliver, lookup := d.deliver, d.lookup
	d.mu.Unlock()

	d.batch = d.batch[:0]
	for i, r := range d.due {
		d.due[i] = nil
		h := r.host
		switch {
		case h.closed.Load():
			d.stats.dropStale.add(h.id)
			continue
		case h.owner.IsRemoved():
			d.stats.dropRemoved.add(h.id)
			continue
		}
		if lookup != nil {
			if live := lookup(h.id); live == nil {
				d.stats.dropNotLoaded.add(h.id)
				continue
			} else if live != h.owner {
				d.stats.dropStale.add(h.id)
				continue
			}
		}
		if h.remote != nil && !mayOwn(h.remote.RemoteOwnershipState()) {
			d.stats.dropNotOwner.add(h.id)
			continue
		}
		if !r.inflight.Load() {
			r.stalls = 0
		} else {
			if frame-r.deliveredFrame < d.stallAfter<<r.stalls {
				r.skipped.Add(1)
				d.stats.skipInflight.add(h.id)
				d.warn.note(r, "inflight")
				continue
			}
			d.stats.stalled.add(h.id)
			d.warn.note(r, "stalled")
			r.stalls = min(r.stalls+1, maxStallBackoff)
		}
		r.inflight.Store(true)
		r.deliveredFrame = frame
		d.batch = append(d.batch, beat{reg: r})
	}
	if len(d.batch) == 0 || deliver == nil {
		for _, b := range d.batch {
			b.reg.inflight.Store(false)
		}
		return
	}
	admitted := int64(len(d.batch))
	deliver.deliverBatch(ctx, d.batch, func(b beat, err error) {
		admitted--
		b.reg.inflight.Store(false)
		if ctx.Err() != nil {
			return // 驱动停止中：不算跳拍
		}
		b.reg.skipped.Add(1)
		reason := "rejected"
		if errors.Is(err, nest.ErrQueueFull) {
			reason = "queue_full"
			d.stats.skipQueueFull.add(b.reg.host.id)
		} else {
			d.stats.skipRejected.add(b.reg.host.id)
		}
		d.warn.noteErr(b.reg, reason, err)
	})
	d.stats.delivered.addN(admitted)
}

// mayOwn 报告 Remote 托管实体的实例在这个所有权状态下是否投递：LocalOwned 是本进程独占；Unknown / Recovering
// 是首次接触或待权威确认，交给 Nest 的 Remote 写准入判定（准入时取得写许可并转到 LocalOwned / Shared，否则拒绝、
// 这拍不执行）。共享（Sharing / Shared）也投：共享模式不允许挂载心跳，这一拍到 handler 里在实体锁内停掉登记
// （业务视角的 Active 与驱动一起变，不由驱动 goroutine 改）；每个登记至多一拍，停掉后移出环。转移中（Draining）、
// 已隔离（Fenced / Quarantined）不投。
func mayOwn(state entity.RemoteOwnershipState) bool {
	switch state {
	case entity.RemoteOwnershipLocalOwned, entity.RemoteOwnershipUnknown, entity.RemoteOwnershipRecovering,
		entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared:
		return true
	default:
		return false
	}
}

// apply 让一次启停 / 改档在驱动上生效（Reg.change 经 AfterCommit 或立即调用）。序号不大于已生效序号的变化是晚到的旧变化，
// 丢弃；实例已 OnDestroy 时忽略。
func (d *Driver) apply(r *Reg, seq uint64, active bool, t tier) {
	now := d.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.host.closed.Load() || seq <= r.ring.seq {
		return
	}
	r.ring.seq = seq
	switch {
	case active && !r.ring.active:
		r.ring.tier = t
		d.ring.insert(r)
		r.startedAt.Store(now.UnixNano())
		r.startGen.Add(1)
	case active && t != r.ring.tier:
		d.ring.remove(r)
		r.ring.tier = t
		d.ring.insert(r)
	case !active && r.ring.active:
		d.ring.remove(r)
		r.ring.tier = t
	default:
		r.ring.tier = t
	}
	r.ring.active = active
}

// close 把实例的全部登记移出环（Host.OnDestroy）。
func (d *Driver) close(h *Host) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// h.regs 只在实体锁内追加；OnDestroy 发生在卸载流程里，此后不会再登记（Register 检查 closed）。
	for _, r := range h.regs {
		if r.ring.active {
			d.ring.remove(r)
			r.ring.active = false
		}
	}
}

// Registered 返回正在跳的登记数。
func (d *Driver) Registered() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ring.count
}

// --- 计数、指标与限频 Warn ---

const (
	cDelivered = iota
	cExecuted
	cSkipInflight
	cSkipQueueFull
	cSkipRejected
	cStalled
	cDropRemoved
	cDropStale
	cDropNotLoaded
	cDropStopped
	cDropNotOwner
	cDropDuplicate
	cStoppedShared
	numCounters
)

// stripedCounter 按实体 ID 分 16 片累加：回调侧计数由全部 worker 并发写，一个原子量会成为热点。
type stripedCounter [16]struct {
	n atomic.Int64
	_ [56]byte
}

func (c *stripedCounter) add(id int64) { c[uint64(id)%16].n.Add(1) }
func (c *stripedCounter) addN(n int64) { c[0].n.Add(n) }
func (c *stripedCounter) load() (sum int64) {
	for i := range c {
		sum += c[i].n.Load()
	}
	return sum
}

type driverStats struct {
	delivered, executed                       stripedCounter
	skipInflight, skipQueueFull, skipRejected stripedCounter
	stalled                                   stripedCounter
	dropRemoved, dropStale, dropNotLoaded     stripedCounter
	dropStopped, dropNotOwner, dropDuplicate  stripedCounter
	stoppedShared                             stripedCounter
}

func (s *driverStats) counters() *[numCounters]*stripedCounter {
	return &[numCounters]*stripedCounter{
		cDelivered: &s.delivered, cExecuted: &s.executed,
		cSkipInflight: &s.skipInflight, cSkipQueueFull: &s.skipQueueFull, cSkipRejected: &s.skipRejected,
		cStalled:     &s.stalled,
		cDropRemoved: &s.dropRemoved, cDropStale: &s.dropStale, cDropNotLoaded: &s.dropNotLoaded,
		cDropStopped: &s.dropStopped, cDropNotOwner: &s.dropNotOwner, cDropDuplicate: &s.dropDuplicate,
		cStoppedShared: &s.stoppedShared,
	}
}

func (d *Driver) count(c *stripedCounter, id int64) { c.add(id) }

// Stats 是驱动计数的快照（测试与诊断用；运维看 metrics）。
type Stats struct {
	Delivered, Executed                                int64
	SkippedInflight, SkippedQueueFull, SkippedRejected int64
	Stalled                                            int64
	DroppedRemoved, DroppedStale, DroppedNotLoaded     int64
	DroppedStopped, DroppedNotOwner, DroppedDuplicate  int64
	StoppedShared                                      int64
	Registered                                         int
}

// Stats 返回计数快照。
func (d *Driver) Stats() Stats {
	s := &d.stats
	return Stats{
		Delivered: s.delivered.load(), Executed: s.executed.load(),
		SkippedInflight: s.skipInflight.load(), SkippedQueueFull: s.skipQueueFull.load(), SkippedRejected: s.skipRejected.load(),
		Stalled:        s.stalled.load(),
		DroppedRemoved: s.dropRemoved.load(), DroppedStale: s.dropStale.load(), DroppedNotLoaded: s.dropNotLoaded.load(),
		DroppedStopped: s.dropStopped.load(), DroppedNotOwner: s.dropNotOwner.load(), DroppedDuplicate: s.dropDuplicate.load(),
		StoppedShared: s.stoppedShared.load(),
		Registered:    d.Registered(),
	}
}

// flush 把计数增量写进 metrics 注册表，并按 WarnInterval 打一条汇总 Warn。只由驱动 goroutine 调用；sysNow 是系统时间。
func (d *Driver) flush(sysNow time.Time) {
	counters := d.stats.counters()
	var delta [numCounters]int64
	for i, c := range counters {
		v := c.load()
		delta[i] = v - d.flushed[i]
		d.flushed[i] = v
	}
	name := d.config.Name
	incCounter(DeliveredMetric, metrics.Labels{"driver": name}, delta[cDelivered])
	incCounter(ExecutedMetric, metrics.Labels{"driver": name}, delta[cExecuted])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "inflight"}, delta[cSkipInflight])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "queue_full"}, delta[cSkipQueueFull])
	incCounter(SkippedMetric, metrics.Labels{"driver": name, "reason": "rejected"}, delta[cSkipRejected])
	incCounter(StalledMetric, metrics.Labels{"driver": name}, delta[cStalled])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "removed"}, delta[cDropRemoved])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "stale"}, delta[cDropStale])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "not_loaded"}, delta[cDropNotLoaded])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "stopped"}, delta[cDropStopped])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "not_owner"}, delta[cDropNotOwner])
	incCounter(DroppedMetric, metrics.Labels{"driver": name, "reason": "duplicate"}, delta[cDropDuplicate])
	incCounter(StoppedMetric, metrics.Labels{"driver": name, "reason": "remote_shared"}, delta[cStoppedShared])
	metrics.SetGauge(RegisteredMetric, metrics.Labels{"driver": name}, int64(d.Registered()))
	d.warn.emit(d.config.Logger, name, d.config.WarnInterval, sysNow)
	if delta[cStoppedShared] > 0 {
		// 每个登记只停一次，不限频（至多每次 flush 一条，汇总本次的个数）。
		args := []any{"driver", name, "stopped", delta[cStoppedShared]}
		if r := d.sharedSample.Swap(nil); r != nil {
			args = append(args, "sample_entity", r.host.id, "sample_registration", r.name)
		}
		d.config.Logger.Warn("heartbeat: registrations stopped because their Remote entity entered shared mode; "+
			"heartbeats are not allowed in shared mode, drive the periodic logic from business code instead", args...)
	}
}

// incCounter 只写非零增量（flush 每秒对每个原因各调一次，大多是零）。
func incCounter(name string, labels metrics.Labels, delta int64) {
	if delta > 0 {
		metrics.IncCounter(name, labels, delta)
	}
}

// warnState 汇总上一条 Warn 之后的跳拍 / 卡住，按间隔打一条。只由驱动 goroutine 读写。
type warnState struct {
	last    time.Time
	pending map[string]int64
	sample  *Reg
	reason  string
	err     error
}

func (w *warnState) note(r *Reg, reason string) {
	if w.pending == nil {
		w.pending = make(map[string]int64)
	}
	w.pending[reason]++
	w.sample, w.reason = r, reason
}

func (w *warnState) noteErr(r *Reg, reason string, err error) {
	w.note(r, reason)
	w.err = err
}

func (w *warnState) emit(logger *slog.Logger, driver string, interval time.Duration, now time.Time) {
	if len(w.pending) == 0 || (!w.last.IsZero() && now.Sub(w.last) < interval) {
		return
	}
	args := []any{"driver", driver, "since_last_warn", w.pending, "sample_entity", w.sample.host.id,
		"sample_registration", w.sample.name, "sample_reason", w.reason}
	if w.err != nil {
		args = append(args, "sample_err", w.err)
	}
	logger.Warn("heartbeat: beats skipped (previous beat still in flight, admission refused, or presumed lost); "+
		"the entity's worker is falling behind or a callback is slow", args...)
	w.last = now
	w.pending, w.sample, w.err = nil, nil, nil
}
