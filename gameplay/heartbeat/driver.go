// Package heartbeat 是按实体的心跳：所有跑在 Nest 业务池上的实体周期逻辑（移动、actionflow / AI、周期效果、
// 实体自己的逻辑帧）的底层。设计与实现说明见 docs/framework/impl/10-time.md §2B。
//
// 职责按文件分：
//
//   - 登记（host.go）：组件在实体加载（OnInitFinish）时经 Host.Register 得到 Reg，按需要 Start / Stop / SetPeriod；
//     卸载、删档时 Host.OnDestroy 撤销。心跳本身什么都不持久，加载后由组件重新登记。
//   - 调度（driver.go、tier.go）：Driver 一个 goroutine，按档分环、10ms 一格；档位固定（Periods），相位 = hash(实体 ID)。
//   - 投递（delivery.go）：缺省（DefaultDelivery = DeliveryBatch）按 64 个一批交给 Nest 批量作业
//     （nest.NestMgr.DispatchTryBatch），同 ID 有已准入未完成的消息时 Nest 把这一拍转成单目标作业排在它后面，
//     Remote 托管实体仍单目标；DeliveryDispatch 时每个到期登记一条单目标 Nest 消息（nest.Client.Dispatch）。
//     两种方式都不合并。每次投递分配一个代际，凭据随消息交给 Nest，执行、准入被拒与交回都只结算自己那一代。
//   - 执行（host.go 的 runBeat）：回调在实体自己的 worker 上、实体锁与事务内执行；快池实体在快池、慢池实体在慢池
//     （Nest 按实体种类分流）。
//   - 观测（observe.go）：计数、指标与限频 Warn。
//
// 每个登记至多一拍在途：上一拍没执行完就跳过这一拍，不累积、不补发，计数并限频打 Warn；准入失败（快池 ErrQueueFull、
// 引擎停止中，含批量作业里冲突转单目标被拒）同样按跳拍处理。在途按投递代际识别：卡住兜底的重投沿用原代际，晚到的
// 旧消息（这一代的另一份已执行、已被拒或已交回）按 dropped{duplicate} 丢弃，不执行回调、不结算下一拍。框架不追帧，
// 回调按 Beat.Elapsed 算一次。持久、准点、离线也要算的到期动作用 framework/nest/entitytimer，不用心跳。
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
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

const (
	// DefaultStallAfter 是在途的拍多久没有执行就视为丢失、重新投递。正常情况下在途的拍都会执行（快池不阻塞）；丢失只来自
	// Nest 在执行 handler 之前放弃了消息，例如 Remote 托管实体的写准入被拒、目标在准入后被卸载且重载失败。重投沿用
	// 原拍的代际，两份消息至多一份执行回调（执行侧按代际认领，见 delivery.go）。
	//
	// 拍没丢、只是排得久（worker 跟不上）时重投会多压一份消息：同一登记连续卡住时阈值逐次翻倍（10s、20s、40s，
	// 上限 maxStallBackoff 次），一拍执行后复位，所以积压时每个登记多出的消息是对数级的；阈值不翻倍时重投与积压会
	// 互相放大（测量见 impl/10-time §2B 第 5 条）。
	DefaultStallAfter = 10 * time.Second
	// maxStallBackoff 是阈值翻倍的次数上限（10s × 2^3 = 80s）。
	maxStallBackoff = 3
	// DefaultWarnInterval 是跳拍 / 卡住 Warn 日志的最小间隔：间隔内的跳拍只计数，下一条 Warn 汇总。
	DefaultWarnInterval = 10 * time.Second
	// flushFrames 是驱动把内部计数刷到 metrics 注册表的间隔（100 格 = 1 秒）。
	flushFrames = int64(time.Second / Resolution)
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
	// Delivery 见 Delivery；空值是 DefaultDelivery（批量）。也可在首次 Start 之前用 SetDelivery 改（wiring/heartbeat 按
	// heartbeat.delivery 配置设）。
	Delivery Delivery
}

// Lookup 只读内存里已发布的实体：不加载、不等待（ManagerAccess.Manager().Get）。
type Lookup func(id int64) entity.IThreadSafeEntity

// Driver 是一个 Nest 引擎上的心跳驱动（进程里每个引擎一个），由 wiring/heartbeat 装配。
type Driver struct {
	config     Config
	stallAfter int64 // 帧
	now        atomic.Pointer[func() time.Time]

	mu      sync.Mutex
	ring    *tierRing
	client  nest.Client // Bind 接上的客户端；SetDelivery 据此重建投递层
	deliver deliverer
	lookup  Lookup
	cancel  context.CancelFunc
	done    chan struct{}
	// lastDone 是最近一次启动的循环的退出信号，Stop 之后保留，Start 据此拒绝与未退出的旧循环并存。
	lastDone chan struct{}

	// returned 是 Nest 批量作业交回、等驱动处理的拍（handBack 追加，drainReturned 取走）；drained 是取走后留着复用的底层数组。
	returnedMu sync.Mutex
	returned   []returnedBeat
	drained    []returnedBeat

	stats driverStats
	// sharedSample 是最近一个因共享模式被停掉的登记（回调侧写、flush 读后清），Warn 的样本。
	sharedSample atomic.Pointer[Reg]

	// 以下只由驱动 goroutine（循环或测试直接调用 step / flush）读写。
	due     []*Reg
	batch   []*beatTicket
	flushed [numCounters]int64
	warn    warnState
	// sharedWarn 汇总上一条“共享模式停掉心跳”Warn 之后停掉的登记数，按 WarnInterval 限频。
	sharedWarn struct {
		last    time.Time
		pending int64
	}
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
	delivery, err := ParseDelivery(string(config.Delivery))
	if err != nil {
		panic(err)
	}
	config.Delivery = delivery
	d := &Driver{config: config, ring: newTierRing()}
	d.stallAfter = max(int64(config.StallAfter/Resolution), 1)
	now := fctx.Now
	d.now.Store(&now)
	return d
}

// Name 返回 Config.Name。
func (d *Driver) Name() string { return d.config.Name }

// SetClock 换掉业务时钟，只给测试（可控时钟）。缺省是 fctx.Now：在一拍的 Nest 消息里是这条消息冻结的
// 业务时间，回调里读 fctx.Now 得到同一个值；不在消息里（启停经 AfterCommit 在提交后生效时）现读进程业务时钟。
// 进程装配不调用它：进程业务时钟由 App 启动时按 time.logic_offset 设定，与 Registry 的业务时钟同源。
// Beat.Now / Elapsed 与启动时刻按它计算；驱动何时醒来按系统单调时间，不受它影响。挂了时间域的登记
// （WithTimeDomain）读域时间，不读这里。
func (d *Driver) SetClock(now func() time.Time) {
	if now != nil {
		d.now.Store(&now)
	}
}

// Now 是驱动的业务时间。
func (d *Driver) Now() time.Time { return (*d.now.Load())() }

// startClock 是登记启动生效时记下的时刻：挂了时间域时是域时间，与 Beat.Now 同一把尺子。
func (r *Reg) startClock() time.Time {
	if r.domain != nil {
		return r.domain.Now()
	}
	return r.host.driver.Now()
}

// Bind 接上投递用的 Nest 客户端与只读实体查找。进程装配在 Nest 可用之后调用一次；可以重复调用（测试）。
func (d *Driver) Bind(client nest.Client, lookup Lookup) error {
	if client == nil || lookup == nil {
		return fmt.Errorf("heartbeat: driver %s: nest client and entity lookup are required", d.config.Name)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	deliver, err := d.newDelivererLocked(client)
	if err != nil {
		return err
	}
	d.client, d.deliver, d.lookup = client, deliver, lookup
	return nil
}

// SetDelivery 改投递方式。只能在驱动第一次 Start 之前调用（进程装配读配置时）：运行期不切换，启动过的驱动返回错误。
// 已经 Bind 过的按新方式重建投递层；DeliveryBatch 要求 Bind 的客户端实现 nest.TryBatchDispatcher。
func (d *Driver) SetDelivery(delivery Delivery) error {
	delivery, err := ParseDelivery(string(delivery))
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancel != nil || d.lastDone != nil {
		return fmt.Errorf("heartbeat: driver %s: delivery cannot change after the driver has started", d.config.Name)
	}
	previous := d.config.Delivery
	d.config.Delivery = delivery
	if d.client == nil {
		return nil
	}
	deliver, err := d.newDelivererLocked(d.client)
	if err != nil {
		d.config.Delivery = previous
		return err
	}
	d.deliver = deliver
	return nil
}

// Delivery 返回当前的投递方式。
func (d *Driver) Delivery() Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.config.Delivery
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
//   - 实例已卸载 / 删档（IsRemoved）：丢弃，并撤销整个宿主（Host.OnDestroy，组件漏调时由这里补上）；
//   - 实例已 OnDestroy、登记已 Close，或查到的是同 ID 的另一个实例：丢弃，不唤醒不在内存的实体；还没发布也不投
//     （同 ID 另一个实例与还没发布都可能是暂时的，例如重载时新实例在旧实例离开之前构造，所以只丢这一拍、不撤销）；
//   - Remote 托管实体的实例处于转移中 / 已隔离：本进程不是拥有方，不投；处于共享模式：照常投一拍，由 handler 在
//     实体锁内停掉本登记（共享模式不允许挂载心跳，见 runBeat）；
//   - 上一拍在途（投递槽不空闲，见 delivery.go）：跳拍（计数、限频 Warn）；在途超过 StallAfter（连续卡住时翻倍）：
//     视为丢失，按同一代际重投一次（计 stalled）。槽空闲时分配新一代投出。
func (d *Driver) step(ctx context.Context) {
	d.mu.Lock()
	d.due = d.ring.step(d.due[:0])
	frame := d.ring.frame
	deliver, lookup := d.deliver, d.lookup
	d.mu.Unlock()

	d.drainReturned(ctx)
	d.batch = d.batch[:0]
	for i, r := range d.due {
		d.due[i] = nil
		h := r.host
		switch {
		case h.owner.IsRemoved():
			// 移除是终态（卸载、删档）：组件没有调 Host.OnDestroy 时由驱动替它撤销，环不再持有这个实例。
			d.stats.dropRemoved.add(h.id)
			h.OnDestroy()
			continue
		case h.closed.Load() || r.closed.Load():
			d.stats.dropStale.add(h.id)
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
		var ticket *beatTicket
		if gen, inFlight := r.inFlight(); !inFlight {
			r.stalls = 0
			ticket = r.issue()
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
			ticket = newBeatTicket(r, gen) // 重投：同一代，与原拍至多一份执行
		}
		r.deliveredFrame = frame
		d.batch = append(d.batch, ticket)
	}
	if len(d.batch) == 0 || deliver == nil {
		for _, t := range d.batch {
			t.reg.settle(t.gen)
		}
		clear(d.batch)
		return
	}
	admitted := int64(len(d.batch))
	deliver.deliverBatch(ctx, d.batch, func(t *beatTicket, err error) {
		admitted--
		d.refused(ctx, t, err) // 驱动停止中只结算、不算跳拍
	})
	clear(d.batch)
	d.stats.delivered.addN(admitted)
	// 准入前就能判定的交回（例如 ID 非法）在 DispatchTryBatch 里同步发生，本格处理掉。
	d.drainReturned(ctx)
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
	now := r.startClock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.host.closed.Load() || r.closed.Load() || seq <= r.ring.seq {
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
