package entitytimer

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/timer"
)

// Store 是实体 DAO 上的定时器存储，生成组件把 DAO 的节点 map 与种子字段适配成它。
// 只在实体的 Nest 锁内（或加载发布之前）调用；写入经 DAO 生成的 mutator，随事务提交 / 回滚。
type Store interface {
	// SavedTimers 返回存储里的全部节点（副本，顺序不限）。
	SavedTimers() []timer.Node
	// TimerSeed 是分配过的最大节点 ID。
	TimerSeed() int64
	PutTimer(node timer.Node)
	DeleteTimer(id int64)
	SetTimerSeed(seed int64)
}

// Fire 是交给业务回调的一次触发。
//
// 框架不追帧（维护者 2026-10-17 决定）：节点在实体不在内存期间到期，加载后只触发一次，不论错过多少个周期。
// 需要补算的业务用这里的字段自己算，例如周期奖励：
//
//	missed := int64(fire.Now.Sub(fire.Deadline) / fire.Interval) // 原定这次之外又错过的周期数
//
// 回调返回正值表示下次间隔，从 Now 起算（与 timer.Scheduler 一致）；返回 0 或负值结束这个定时器。
type Fire struct {
	// OwnerID 是实体的完整 ID。
	OwnerID int64
	// ID 是节点 ID（NewTimer 的返回值），实体内唯一、重启后不复用。
	ID       int64
	Type     int32
	Priority int32
	Param1   int64
	Param2   int64
	// Payload 是登记时复制的业务数据，回调可以保留（已是副本）。
	Payload []byte
	// Deadline 是原定期限：最近一次登记 / 改期 / 续期时算出的到期时刻。
	Deadline time.Time
	// Now 是实际触发时刻（这次 Tick 的业务时间）。在线时与 Deadline 只差投递延迟；补触发时可能差很多。
	Now time.Time
	// Interval 是最近一次登记 / 改期 / 续期用的间隔（周期定时器的周期）。
	Interval time.Duration
	// LoadedAt 是本实例从存储加载的业务时间；本进程内新建的实例为零值。
	LoadedAt time.Time
	// Overdue 为 true 表示原定期限不晚于本实例加载时刻：它在实体不在内存时（离线、停服、迁移）到期，
	// 这是加载后的补触发。在线时因为投递延迟晚了一点的触发不算。
	Overdue bool
}

// Late 是实际触发比原定期限晚了多久（不会为负）。
func (fire Fire) Late() time.Duration {
	if late := fire.Now.Sub(fire.Deadline); late > 0 {
		return late
	}
	return 0
}

// Handler 是一种定时器的业务回调，在实体的 Nest 锁与事务内执行：只改 DAO、记 effect，不做 I/O、不阻塞。
type Handler func(Fire) time.Duration

// Handlers 收集一个实体的定时器类型与回调，由业务文件在组件构造时登记。
type Handlers struct {
	byType map[int32]Handler
	err    error
}

// Register 登记 timerType 的回调。类型号必须为正（0 是 timer.TypeClosure），同一类型只能登记一次；
// 违反时记下错误，实体初始化（OnInitFinish）返回它。
func (handlers *Handlers) Register(timerType int32, handler Handler) {
	switch {
	case timerType <= 0:
		handlers.err = errors.Join(handlers.err, fmt.Errorf("entitytimer: timer type %d must be positive", timerType))
	case handler == nil:
		handlers.err = errors.Join(handlers.err, fmt.Errorf("entitytimer: timer type %d has a nil handler", timerType))
	case handlers.byType[timerType] != nil:
		handlers.err = errors.Join(handlers.err, fmt.Errorf("entitytimer: timer type %d is registered twice", timerType))
	default:
		if handlers.byType == nil {
			handlers.byType = make(map[int32]Handler)
		}
		handlers.byType[timerType] = handler
	}
}

// overdueDelay 是 NewTimerAt 给已经过去的时刻用的间隔：Scheduler 不接受非正间隔，持久精度是毫秒，
// 所以“现在就到期”登记为 1 毫秒之后，由下一次 Tick 触发。
const overdueDelay = time.Millisecond

// Host 是一个实体实例的定时器宿主（生成组件持有一个）。除 OnDestroy 外的方法都只能在实体的 Nest 锁内调用
// （Nest handler 里，或加载 / 新建的初始化阶段）；它们读写的是 DAO，不是 Host 自己的状态。
//
// 最外层调用从 DAO 现建一个 timer.Scheduler、钩子写回 DAO、用完丢弃；Tick 回调中的组件操作（含 Timer / Timers
// 读取）复用这次调用的调度视图，不从中间 DAO 重建。因此事务回滚就是 DAO 的回滚，没有跨事务的内存堆。调用结束时报告最早期限：
// 在事务里经 nest.AfterCommit（提交后才更新索引，回滚的事务不改索引），不在事务里时立即报告。
type Host struct {
	driver   *Driver
	owner    entity.IThreadSafeEntity
	store    Store
	handlers map[int32]Handler
	err      error
	slot     *slot
	// loadedAt 在 OnInitFinish（发布之前）写一次，之后只读。
	loadedAt time.Time
	// seq 只在实体锁内递增；它不是事务状态（回滚不退回），只给驱动排定报告的先后。
	seq uint64
	// active 只在当前 Tick 的调用栈内存在。回调通过组件操作定时器时共享这个调度视图，
	// 避免从中间 DAO 状态重建第二个堆；退出或 panic 都清掉，不跨事务保存状态。
	active *timer.Scheduler
}

// NewHost 为一个实体实例构造宿主；register 登记业务回调（生成组件传入业务文件里的登记函数）。
func (d *Driver) NewHost(owner entity.IThreadSafeEntity, store Store, register func(*Handlers)) *Host {
	host := &Host{driver: d, owner: owner, store: store}
	if owner == nil || store == nil {
		host.err = fmt.Errorf("entitytimer: driver %s: host needs an owner and a store", d.config.Name)
		return host
	}
	var handlers Handlers
	if register != nil {
		register(&handlers)
	}
	host.handlers, host.err = handlers.byType, handlers.err
	host.slot = d.newSlot(owner)
	return host
}

// OnInitFinish 在实体初始化末尾调用（组件的 OnInitFinish）。加载（isCreate 为 false）时记下加载时刻——
// 原定期限不晚于它的节点触发时 Fire.Overdue 为 true——对没有回调的存量类型每种告警一次，并把最早期限报告给
// 驱动；已经过期的节点因此在加载后由驱动投递一次 Tick 补触发。这里不改 DAO：加载不产生写入。
func (host *Host) OnInitFinish(isCreate bool) error {
	if host.err != nil {
		return host.err
	}
	now := host.driver.Now()
	if !isCreate {
		host.loadedAt = now
	}
	scheduler := host.scheduler(now)
	scheduler.ReportUnhandledTypes()
	host.settle(scheduler, now)
	return nil
}

// OnDestroy 在实体销毁时调用（卸载、删档、撤销新建）：实例永久离开驱动索引，之后的报告一律丢弃。
func (host *Host) OnDestroy() {
	if host != nil {
		host.driver.close(host.slot)
	}
}

// LoadedAt 返回本实例的加载时刻（本进程新建的为零值）。
func (host *Host) LoadedAt() time.Time { return host.loadedAt }

// NewTimer 登记一个 delay 之后到期的定时器，返回节点 ID；delay 不为正或类型不合法时返回 0。
func (host *Host) NewTimer(delay time.Duration, timerType int32, param1, param2 int64, payload []byte) int64 {
	return host.NewTimerWithPriority(delay, timerType, 0, param1, param2, payload)
}

// NewTimerWithPriority 与 NewTimer 相同，另外指定同一期限内的触发顺序（小的先触发，相同按登记顺序）。
func (host *Host) NewTimerWithPriority(delay time.Duration, timerType int32, priority int32, param1, param2 int64, payload []byte) int64 {
	if host.err != nil {
		return 0
	}
	now := host.driver.Now()
	scheduler := host.scheduler(now)
	id := scheduler.NewTimerWithPriority(delay, timerType, priority, param1, param2, payload)
	host.settle(scheduler, now)
	return id
}

// NewTimerAt 登记一个在 at 到期的定时器（按毫秒）；at 已经过去时登记为 1 毫秒之后，由下一次 Tick 触发。
// 间隔与登记用同一个 now 计算，期限正好是 at。
func (host *Host) NewTimerAt(at time.Time, timerType int32, param1, param2 int64, payload []byte) int64 {
	if host.err != nil {
		return 0
	}
	now := host.driver.Now()
	delay := time.UnixMilli(at.UnixMilli()).Sub(now)
	if delay <= 0 {
		delay = overdueDelay
	}
	scheduler := host.scheduler(now)
	id := scheduler.NewTimer(delay, timerType, param1, param2, payload)
	host.settle(scheduler, now)
	return id
}

// ChangeTimer 把节点改到现在起 delay 之后到期；节点不存在时返回 false。
func (host *Host) ChangeTimer(id int64, delay time.Duration) bool {
	if host.err != nil {
		return false
	}
	now := host.driver.Now()
	scheduler := host.scheduler(now)
	changed := scheduler.ChangeTimer(id, now, delay)
	host.settle(scheduler, now)
	return changed
}

// RemoveTimer 取消节点；返回 true 之后它不再触发，节点不存在时返回 false。
func (host *Host) RemoveTimer(id int64) bool {
	if host.err != nil {
		return false
	}
	now := host.driver.Now()
	scheduler := host.scheduler(now)
	removed := scheduler.RemoveTimer(id)
	host.settle(scheduler, now)
	return removed
}

// Timer 返回一个节点。Tick 回调里读当前调度视图（见 Timers），其余时候读 DAO。
func (host *Host) Timer(id int64) (timer.Node, bool) {
	if host.active != nil {
		return host.active.Lookup(id)
	}
	for _, node := range host.store.SavedTimers() {
		if node.ID == id {
			return node, true
		}
	}
	return timer.Node{}, false
}

// Timers 返回全部节点，按触发顺序（期限、优先级、ID）。
//
// Tick 回调里读当前调度视图（timer.Scheduler 的 LiveNodes）而不是 DAO：回调里新建、改期的节点要到最外层 Tick
// 收尾才写回 DAO，读 DAO 会看不到自己刚写的（“没有就建一个”会在一次 Tick 内重复登记）。视图里的节点与
// ChangeTimer / RemoveTimer 能操作的一致，包括正在触发的节点自己（期限为原定期限，显式改期后为新期限；
// 它已从 DAO 删除，回调返回正值时收尾再写回）。
func (host *Host) Timers() []timer.Node {
	if host.active != nil {
		return host.active.LiveNodes()
	}
	nodes := host.store.SavedTimers()
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if !a.End.Equal(b.End) {
			return a.End.Before(b.End)
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})
	return nodes
}

// Tick 触发此刻已到期的节点（生成的 Tick handler 调用），返回剩下的节点数。此刻是驱动的业务时间：Tick 消息里就是
// 这条消息冻结的 fctx.Now（按毫秒），回调的 Fire.Now 与回调里读的 fctx.Now 是同一时刻。
// 每个到期节点只触发一次：离线错过多个周期的周期定时器也只触发这一次，回调按返回值从此刻起续期。
func (host *Host) Tick() int { return host.TickAt(host.driver.Now()) }

// TickAt 与 Tick 相同，但触发时刻由调用方给出（按毫秒）：手写的 Tick handler 想用别的时刻、测试与回放用。
// 生成的 Tick handler 用 Tick。
func (host *Host) TickAt(now time.Time) int {
	if host.err != nil {
		return 0
	}
	now = time.UnixMilli(now.UnixMilli())
	scheduler := host.scheduler(now)
	previous := host.active
	host.active = scheduler
	defer func() { host.active = previous }()
	scheduler.Tick(now)
	host.active = previous
	host.settle(scheduler, now)
	return len(host.store.SavedTimers())
}

// scheduler 从 DAO 建本次调用的调度器，时钟固定为 now：一次调用里的期限都从同一个时刻算起。
func (host *Host) scheduler(now time.Time) *timer.Scheduler {
	if host.active != nil {
		host.active.SetClock(func() time.Time { return now })
		return host.active
	}
	scheduler := timer.NewScheduler(host.owner.ID(), host.store.TimerSeed(), host.store.SavedTimers(), host.persist)
	for timerType, handler := range host.handlers {
		scheduler.RegisterHandler(timerType, host.wrap(handler))
	}
	scheduler.SetClock(func() time.Time { return now })
	return scheduler
}

func (host *Host) wrap(handler Handler) timer.Handler {
	return func(ctx timer.Context) time.Duration {
		node := ctx.Node
		return handler(Fire{
			OwnerID: ctx.OwnerID, ID: node.ID, Type: node.Type, Priority: node.Priority,
			Param1: node.Param1, Param2: node.Param2, Payload: node.Payload,
			Deadline: node.End, Now: ctx.Now, Interval: node.Delay,
			LoadedAt: host.loadedAt,
			Overdue:  !host.loadedAt.IsZero() && !node.End.After(host.loadedAt),
		})
	}
}

// persist 是调度器的变化钩子：每次增、删、改落到 DAO。种子随 upsert 推进并持久，重启后不会把已经交出去的
// ID 再分配一次（Scheduler 保证每个分配过的 ID 至少经过一次 upsert）。
func (host *Host) persist(change timer.ChangeType, node timer.Node) {
	if change == timer.ChangeDelete {
		host.store.DeleteTimer(node.ID)
		return
	}
	host.store.PutTimer(node)
	if node.ID > host.store.TimerSeed() {
		host.store.SetTimerSeed(node.ID)
	}
}

// settle 把调度器剩下的最早期限报告给驱动。存量里有非法节点时报告 now：它们只能由事务内的 Tick 清理。
func (host *Host) settle(scheduler *timer.Scheduler, now time.Time) {
	// 内层组件操作只改当前调度视图，最外层 Tick 落完所有节点后统一报告。
	if host.active != nil {
		return
	}
	due := scheduler.NextTime()
	if scheduler.NeedsCleanup() {
		due = now
	}
	host.seq++
	seq, slot, driver := host.seq, host.slot, host.driver
	report := func() { driver.report(slot, seq, due) }
	if !nest.AfterCommit(report) {
		report()
	}
}
