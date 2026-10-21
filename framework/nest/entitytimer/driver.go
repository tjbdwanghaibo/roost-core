// Package entitytimer 是按实体的持久定时器（DESIGN-TODO D11，维护者 2026-10-17 决定方案 B）。
//
// 三件事分在三处：
//
//   - 期限：存在实体自己的 DAO 里（Store，生成组件把 DAO 字段适配进来），增删改都在 Nest 事务里经
//     infra/base/timer.Scheduler 的 ChangeFunc 写回，随事务提交 / 回滚；没有第二份存储。
//   - 触发：Host.Tick 在实体的 Nest 锁内执行（生成的 Tick handler），回调只改 DAO / 记 effect，不做 I/O。
//   - 驱动：Driver 是进程内派生索引，只记“已加载实例的最早期限”。到期才给那一个实体投递一次 Tick 消息
//     （nest.Client.Dispatch，走正式准入），不按在线实体数轮询。索引在事务提交后（nest.AfterCommit）更新，
//     回滚的事务不改索引；同一实例的更新带单调序号，晚到的旧更新被丢弃。
//
// 离线语义（维护者 2026-10-17 决定）：框架不追帧。实体不在内存期间到期的节点，在实体加载起来之后只触发一次，
// 不论错过多少个周期；Fire 带上原定期限、实际触发时刻、本实例加载时刻、是否为加载后补触发与登记间隔，
// 补算错过的周期由业务在回调里做。回调返回值与 Scheduler 一致：正值是下次间隔，从本次触发时刻起算。
//
// 驱动不唤醒未加载的实体：离线实体到期不会被准点处理，只在下次加载时补触发。需要“离线也准点”的到期动作
// 要另外的全局扫描机制（不在本包范围）。
package entitytimer

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

const (
	// DefaultRedeliver 是投递之后迟迟没有收到该实例更新时再投一次的间隔。Tick handler 正常提交时总会经
	// AfterCommit 报告新的最早期限；没有报告说明这次 Tick 没有执行或被回滚（handler 报错、提交被拒、
	// 消息在卸载前后丢失），等这么久再投。重投是幂等的：Tick 只触发此刻已到期的节点。
	DefaultRedeliver = 10 * time.Second
	// DefaultMaxWait 是驱动循环最长的一次睡眠。业务时钟的偏移可以在运行中调整，睡眠按系统时间计，
	// 所以每隔这么久重新读一次业务时钟；它是每个驱动一次唤醒，不是每个实体一次。
	DefaultMaxWait = time.Second
	// dispatchRetry 是准入失败（队列满、引擎停止中）之后的重试间隔。
	dispatchRetry = time.Second
)

const (
	// DeliveredMetric 计投递出去的 Tick 消息，标签 driver 为 Config.Name。
	DeliveredMetric = "entitytimer.delivered_total"
	// RedeliveredMetric 计其中因为上一次投递之后没有收到报告而重投的。持续增长说明 Tick handler 在报错或回滚。
	RedeliveredMetric = "entitytimer.redelivered_total"
	// DeliverFailedMetric 计准入失败（之后按 dispatchRetry 重试）。
	DeliverFailedMetric = "entitytimer.deliver_failed_total"
	// DroppedMetric 计到期时发现实例已卸载 / 已删除 / 已被同 ID 的新实例取代而丢弃、不投递的条目。
	DroppedMetric = "entitytimer.dropped_total"
	// PendingMetric 是索引里等待到期（含已投递待报告）的实例数（Gauge）。
	PendingMetric = "entitytimer.pending"
)

// ErrNotBound 表示驱动还没有接上 Nest（Bind）就被要求投递或启动。
var ErrNotBound = errors.New("entitytimer: driver is not bound to a nest client")

// Config 是 NewDriver 的输入。生成组件把它写成包级变量，进程装配（wiring/entitytimer）再 Bind。
type Config struct {
	// Name 用于日志与指标标签，例如 "player.timer"。必填。
	Name string
	// Handler 是到期时投递的 Nest handler：单目标、没有参数，handler 里调 Host.Tick()（按消息冻结的业务时间触发）。必填。
	Handler nest.HandlerName
	// Redeliver 见 DefaultRedeliver；0 取缺省。
	Redeliver time.Duration
	// MaxWait 见 DefaultMaxWait；0 取缺省。
	MaxWait time.Duration
}

// Lookup 只读内存里已发布的实体：不加载、不等待、不 Touch（ManagerAccess.Manager().Get）。
type Lookup func(id int64) entity.IThreadSafeEntity

// Driver 是一种实体定时器的进程内驱动：按实例记最早期限的小堆，加一个只在到期时投递的循环。
// 方法可从任意 goroutine 调用；进入索引的更新只持有一把短锁，不做 I/O、不等待，可以在 Nest 业务池上调用。
type Driver struct {
	config Config

	mu     sync.Mutex
	now    func() time.Time
	pend   slotHeap
	client nest.Client
	lookup Lookup
	// kick 在堆顶变早时唤醒循环（容量 1，非阻塞发送）。
	kick chan struct{}
	// cancel / done 属于正在运行的循环；未启动时为 nil。
	cancel context.CancelFunc
	done   chan struct{}
}

// NewDriver 校验配置并返回一个未接线的驱动；配置是生成代码里的常量，错了就是生成物不一致，所以 panic。
func NewDriver(config Config) *Driver {
	if config.Name == "" {
		panic("entitytimer: driver name is required")
	}
	if config.Handler.String() == "" {
		panic("entitytimer: driver " + config.Name + ": tick handler name is required")
	}
	if config.Redeliver <= 0 {
		config.Redeliver = DefaultRedeliver
	}
	if config.MaxWait <= 0 {
		config.MaxWait = DefaultMaxWait
	}
	return &Driver{config: config, now: fctx.Now, kick: make(chan struct{}, 1)}
}

// Name 返回 Config.Name。
func (d *Driver) Name() string { return d.config.Name }

// Handler 返回投递的 Nest handler 名。
func (d *Driver) Handler() nest.HandlerName { return d.config.Handler }

// SetClock 换掉业务时钟，只给测试（可控时钟）；期限、触发时刻与加载时刻都按这一个时钟计算，所以必须在实体加载
// 之前设置。缺省是 fctx.Now（D22-1）：在 Nest 消息里（Tick、业务 handler 里增删改定时器、加载时的 OnInitFinish）是
// 这次执行冻结的业务时间，同一条消息里的期限都从同一时刻算起；驱动自己的循环不在消息里，现读进程业务时钟。进程
// 装配不调用它：进程业务时钟由 App 启动时按 time.logic_offset 设定，与 app.BusinessClock(r) 同一份配置。
func (d *Driver) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	d.mu.Lock()
	d.now = now
	d.mu.Unlock()
	d.wake()
}

// Now 是驱动的业务时间，截到毫秒：期限按毫秒持久，内存里的期限与重建后的期限必须同一精度，
// 否则加载前后的触发顺序和“是否已到期”会不同。
func (d *Driver) Now() time.Time {
	d.mu.Lock()
	now := d.now
	d.mu.Unlock()
	return time.UnixMilli(now().UnixMilli())
}

// Bind 接上投递用的 Nest 客户端与只读实体查找。进程装配在 Nest 引擎可用之后调用一次；可以重复调用（测试）。
func (d *Driver) Bind(client nest.Client, lookup Lookup) error {
	if client == nil || lookup == nil {
		return fmt.Errorf("entitytimer: driver %s: nest client and entity lookup are required", d.config.Name)
	}
	d.mu.Lock()
	d.client, d.lookup = client, lookup
	d.mu.Unlock()
	return nil
}

// Start 启动投递循环。已经在运行时返回 nil。
func (d *Driver) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client == nil {
		return ErrNotBound
	}
	if d.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.cancel, d.done = cancel, done
	go d.loop(ctx, done)
	return nil
}

// Stop 停止投递循环并等它退出，或等到 ctx 结束（返回 ctx 的错误，循环随后自行退出）。
// 索引保留：停止只是不再投递，已登记的期限仍在 DAO 里，下一次加载照样补触发。
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
	wait := time.NewTimer(0)
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
		case <-d.kick:
		}
		d.DeliverDue(ctx)
		wait.Reset(d.nextWait())
	}
}

// nextWait 是到堆顶期限的系统时间间隔，上限 MaxWait。
func (d *Driver) nextWait() time.Duration {
	now := d.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pend) == 0 {
		return d.config.MaxWait
	}
	wait := d.pend[0].wake.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return min(wait, d.config.MaxWait)
}

func (d *Driver) wake() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// delivery 是一次投递：出锁之后才调用 Dispatch，回来按 gen 判断这期间实例有没有报告过新状态。
type delivery struct {
	slot *slot
	gen  uint64
	id   int64
}

// DeliverDue 把此刻（业务时间）已到期的实例各投递一次 Tick，返回投递成功的条数。循环按期限调用它；
// 测试可以不启动循环、推进可控时钟后直接调用，事件顺序由测试决定。
//
// 每个到期条目先核对实例：已卸载 / 已删除（IsRemoved）或已被同 ID 的另一个实例取代时丢弃，不投递——
// 驱动不唤醒不在内存里的实体。查不到任何实例时照常投递：那是实例刚构建、还没发布（加载中，Nest 的冷加载
// 会等这次加载完成再执行 Tick）。投递后条目转为“待报告”，期限换成 Redeliver 之后；Tick handler 提交时
// 实例的报告把它换回新的最早期限。
func (d *Driver) DeliverDue(ctx context.Context) int {
	now := d.Now()
	d.mu.Lock()
	client, lookup := d.client, d.lookup
	if client == nil || lookup == nil {
		d.mu.Unlock()
		return 0
	}
	var batch []delivery
	dropped, redelivered := 0, 0
	for len(d.pend) > 0 && !d.pend[0].wake.After(now) {
		s := d.pend[0]
		if s.owner.IsRemoved() {
			d.unqueue(s)
			dropped++
			continue
		}
		if live := lookup(s.id); live != nil && live != s.owner {
			d.unqueue(s)
			dropped++
			continue
		}
		if s.inflight {
			redelivered++
		}
		s.inflight = true
		s.gen++
		s.wake = now.Add(d.config.Redeliver)
		heap.Fix(&d.pend, s.index)
		batch = append(batch, delivery{slot: s, gen: s.gen, id: s.id})
	}
	pending := len(d.pend)
	d.mu.Unlock()

	labels := metrics.Labels{"driver": d.config.Name}
	if dropped > 0 {
		metrics.IncCounter(DroppedMetric, labels, int64(dropped))
	}
	if redelivered > 0 {
		metrics.IncCounter(RedeliveredMetric, labels, int64(redelivered))
	}
	metrics.SetGauge(PendingMetric, labels, int64(pending))

	delivered := 0
	// Tick 消息不带时间：handler 执行时按这条消息冻结的业务时间触发（Host.Tick），排队的时间不算进“之前”。
	for _, item := range batch {
		err := client.Dispatch(ctx, d.config.Handler, item.id, nil)
		if err == nil {
			delivered++
			continue
		}
		metrics.IncCounter(DeliverFailedMetric, labels, 1)
		if ctx.Err() == nil {
			slog.Warn("entitytimer: tick delivery not admitted; retrying",
				"driver", d.config.Name, "entity_id", item.id, "retry_in", dispatchRetry, "err", err)
		}
		d.mu.Lock()
		if s := item.slot; s.gen == item.gen && s.index >= 0 {
			// 这期间没有新报告：按准入重试间隔再投，而不是等满 Redeliver。
			s.inflight = false
			s.wake = now.Add(dispatchRetry)
			heap.Fix(&d.pend, s.index)
		}
		d.mu.Unlock()
	}
	if delivered > 0 {
		metrics.IncCounter(DeliveredMetric, labels, int64(delivered))
	}
	return delivered
}

// Pending 返回索引里的条目数（等待到期或已投递待报告）。
func (d *Driver) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pend)
}

// NextDue 返回索引里最早的唤醒时刻（期限，或已投递条目的重投时刻）；索引为空时 ok 为 false。
func (d *Driver) NextDue() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pend) == 0 {
		return time.Time{}, false
	}
	return d.pend[0].wake, true
}

// DueOf 返回 owner 实例在索引里登记的最早期限；没有登记、已投递待报告或已关闭时 ok 为 false。测试与诊断用。
func (d *Driver) DueOf(owner entity.IThreadSafeEntity) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.pend {
		if s.owner == owner && !s.inflight {
			return s.due, true
		}
	}
	return time.Time{}, false
}

// slot 是一个实例（一次加载 / 新建得到的那个对象）在驱动里的位置，由 Host 持有。
// 除 owner / id 外的字段都只在 Driver.mu 下读写。
type slot struct {
	owner entity.IThreadSafeEntity
	id    int64
	// seq 是该实例最后接受的报告序号；序号不大于它的报告是晚到的旧报告，丢弃。
	seq uint64
	// due 是实例报告的最早期限（零值表示没有期限，不在堆里）。
	due time.Time
	// wake 是堆键：未投递时等于 due，已投递待报告时是重投时刻。
	wake time.Time
	// inflight：已投递 Tick，还没有收到实例的报告。
	inflight bool
	// gen 每次投递与每次接受报告都加一，出锁投递回来时据此判断期间有没有新报告。
	gen uint64
	// closed：实例已销毁（卸载 / 删除 / 撤销新建），之后的报告一律丢弃，也不再进堆。
	closed bool
	index  int
}

func (d *Driver) newSlot(owner entity.IThreadSafeEntity) *slot {
	return &slot{owner: owner, id: owner.ID(), index: -1}
}

// report 接受实例 seq 号报告的最早期限（零值为没有期限）。已关闭的实例、晚到的旧报告不改索引。
func (d *Driver) report(s *slot, seq uint64, due time.Time) {
	if s == nil {
		return
	}
	d.mu.Lock()
	if s.closed || seq <= s.seq {
		d.mu.Unlock()
		return
	}
	s.seq = seq
	s.gen++
	s.inflight = false
	s.due = due
	var before time.Time
	if len(d.pend) > 0 {
		before = d.pend[0].wake
	}
	if due.IsZero() {
		d.unqueue(s)
	} else {
		s.wake = due
		if s.index >= 0 {
			heap.Fix(&d.pend, s.index)
		} else {
			heap.Push(&d.pend, s)
		}
	}
	// 只有堆顶变早才需要叫醒循环：变晚时循环按旧时刻醒来，重新计算即可。
	kick := len(d.pend) > 0 && (before.IsZero() || d.pend[0].wake.Before(before))
	d.mu.Unlock()
	if kick {
		d.wake()
	}
}

// close 让实例永久离开索引：卸载、删档或撤销新建时调用，可重复。
func (d *Driver) close(s *slot) {
	if s == nil {
		return
	}
	d.mu.Lock()
	s.closed = true
	d.unqueue(s)
	d.mu.Unlock()
}

// unqueue 把条目移出堆（调用方持有 mu）；不在堆里时无操作。
func (d *Driver) unqueue(s *slot) {
	if s.index >= 0 {
		heap.Remove(&d.pend, s.index)
	}
	s.inflight = false
}

// slotHeap 按 wake 排序；同一时刻按实体 ID，让投递顺序确定。
type slotHeap []*slot

func (h slotHeap) Len() int { return len(h) }
func (h slotHeap) Less(i, j int) bool {
	if !h[i].wake.Equal(h[j].wake) {
		return h[i].wake.Before(h[j].wake)
	}
	return h[i].id < h[j].id
}
func (h slotHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *slotHeap) Push(x any) {
	s := x.(*slot)
	s.index = len(*h)
	*h = append(*h, s)
}
func (h *slotHeap) Pop() any {
	old := *h
	n := len(old)
	s := old[n-1]
	old[n-1] = nil
	s.index = -1
	*h = old[:n-1]
	return s
}
