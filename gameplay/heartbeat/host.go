package heartbeat

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
)

var (
	// ErrUnsupportedPeriod 表示周期不是固定档位之一（Periods）。
	ErrUnsupportedPeriod = errors.New("heartbeat: unsupported period")
	// ErrDuplicateName 表示同一实体实例上同名登记了两次。
	ErrDuplicateName = errors.New("heartbeat: registration name already used on this entity")
	// ErrHostClosed 表示实例已销毁（OnDestroy 之后，或驱动发现实例已移除），不能再登记。
	ErrHostClosed = errors.New("heartbeat: host is closed")
	// ErrRegClosed 表示登记已撤销（Reg.Close 之后），不能再启动或换档。
	ErrRegClosed = errors.New("heartbeat: registration is closed")
	// ErrHeartbeatRemoteShared 表示 Remote 托管实体处于共享模式（Sharing / Shared），不允许挂载心跳（维护者 2026-10-21）：
	// 共享模式下各写者进程的实例状态都是 Shared、都经分布式锁写，本地分不出谁该跳，心跳无法执行。Register 与 Start
	// 在共享模式下返回它；运行期才进入共享模式的，已挂的心跳由下一拍在实体锁内停掉（计数 heartbeat.stopped_total、打
	// Warn）。替代做法：共享实体的周期逻辑由业务层自己驱动，例如由负责它的服务按周期发 Nest 消息。
	ErrHeartbeatRemoteShared = errors.New("heartbeat: Remote entity is in shared mode, heartbeats are not allowed " +
		"(every writer's instance is shared, so no process can tell it is the one to beat); drive the periodic " +
		"logic from business code instead, e.g. the owning service sends Nest messages on its own schedule")
)

// DefaultMaxElapsed 是 Beat.Elapsed 的上限（维护者 2026-10-21：“距上次执行，最多 1 秒”）。档位本身长于 1 秒时上限取
// 档位周期，否则 2s 及以上的档每拍都会少算（见 Beat.Elapsed）。
const DefaultMaxElapsed = time.Second

// Beat 是交给回调的一拍。
type Beat struct {
	// Now 是本拍的业务时间：这一拍的 Nest 消息冻结的 fctx.Now（D22-1），回调里再读 fctx.Now 是同一个值。
	// 挂了时间域（WithTimeDomain）时是这条消息的域时间。
	Now time.Time
	// Elapsed 是距本登记上次执行的业务时间（挂了时间域时是域时间，上限再乘倍率）；首拍是距本次启动生效。
	// 夹到 [0, max(DefaultMaxElapsed, Period)]：
	// 掉拍、进程停顿或业务时钟被调整时不让一拍走出几公里。框架不追帧，掉拍后下一拍按 Elapsed 算一次。
	Elapsed time.Duration
	// Period 是本登记当前档位。
	Period time.Duration
	// Skipped 是上次执行以来因上一拍在途或准入失败被跳过的拍数；只供观测，不补投。
	Skipped int
	// First 为 true 表示本登记这次启动后的第一拍（实体重载后重新启动、Stop 后再 Start 都算新的一次）。
	First bool
}

// Func 是登记的回调：在实体自己的 Nest worker 上、实体锁与事务内执行，与该实体的其他消息串行。只做内存计算与入队
// （改 DAO、记 effect、QueueMove），不阻塞、不做 I/O、不 Dispatch / Request。返回错误时本拍事务回滚。
type Func func(Beat) error

// RegOption 调整一个登记。
type RegOption func(*regOptions)

type regOptions struct {
	durability nest.DurabilityPolicy
	domain     *timedomain.Domain
}

// WithDurability 选本登记心跳 handler 的持久级别（缺省 nest.DurabilityAsync）。心跳本身什么都不持久；回调里改了持久
// 字段时按这里的级别提交（维护者决定 ⑧：由业务按自己的持久级别处理）。DurabilityMemory 下改持久字段会在提交点被拒绝；
// DurabilityPipelined 要求引擎装了 pipelined 提交器。回滚策略固定 undo。
func WithDurability(policy nest.DurabilityPolicy) RegOption {
	return func(o *regOptions) { o.durability = policy }
}

// WithTimeDomain 让本登记按场景的时间域计时（DESIGN-TODO D22，维护者 2026-10-21 决定 ③）：调度仍按真实时间（档位
// 不缩放，CPU 不随倍率涨）；Beat.Now 是这一拍消息的域时间（timedomain.Domain.Now），Elapsed 是距上次执行的域时间，
// 时停时为 0；Elapsed 的上限乘这一拍的倍率（max(DefaultMaxElapsed, 档位) × 倍率），否则 10 倍速时 100ms 档每拍
// 都被夹住。d 为 nil 时不挂。
func WithTimeDomain(d *timedomain.Domain) RegOption {
	return func(o *regOptions) { o.domain = d }
}

// Host 是一个实体实例的心跳宿主，由组件在构造或 OnInitFinish 里经 Driver.NewHost 取得。Register 与 Reg 的
// Start / Stop / SetPeriod / Close 只能在实体锁内（Nest handler 里，或加载 / 新建的初始化阶段）调用；OnDestroy 任意
// goroutine。
type Host struct {
	driver *Driver
	owner  entity.IThreadSafeEntity
	id     int64
	hash   uint64
	// remote 非 nil 表示 Remote 托管实体：只有本进程实例是当前拥有方（LocalOwned）时才跳；共享模式不允许挂载。
	remote entity.IThreadSafeRemoteEntity
	closed atomic.Bool

	// regs 在实体锁内且持 Driver.mu 时追加、删除（Reg.Close）；实体锁内或 Driver.mu 下读。
	regs []*Reg
}

// NewHost 为一个实体实例构造宿主。owner 为 nil 时 panic（组件接线错误）。
func (d *Driver) NewHost(owner entity.IThreadSafeEntity) *Host {
	if owner == nil {
		panic("heartbeat: NewHost needs an entity")
	}
	h := &Host{driver: d, owner: owner, id: owner.ID(), hash: phaseHash(owner.ID())}
	if remote, ok := owner.(entity.IThreadSafeRemoteEntity); ok && entity.IsEntityKindRemoteManaged(owner.GetEntityKind()) {
		h.remote = remote
	}
	return h
}

// Owner 返回宿主所属的实体实例。
func (h *Host) Owner() entity.IThreadSafeEntity { return h.owner }

// sharedErr 在 Remote 托管实体处于共享模式时返回包裹 ErrHeartbeatRemoteShared 的错误，否则 nil。实体锁内调用。
func (h *Host) sharedErr(name string) error {
	if h.remote == nil {
		return nil
	}
	if state := h.remote.RemoteOwnershipState(); remoteShared(state) {
		return fmt.Errorf("%w: registration %q on entity %d (ownership %s)", ErrHeartbeatRemoteShared, name, h.id, state)
	}
	return nil
}

// remoteShared 报告所有权状态是否是共享模式（含进入共享的过程 Sharing）。
func remoteShared(state entity.RemoteOwnershipState) bool {
	return state == entity.RemoteOwnershipSharing || state == entity.RemoteOwnershipShared
}

// Register 登记一个心跳，返回停止状态的 Reg；之后 Start 才开始跳。同一实例上的多个登记各自投递、互不合并（维护者决定 ③）。
// name 在实例内唯一（用于日志与诊断），Reg.Close 之后可以再用；period 必须是 Periods 之一。Remote 托管实体处于共享模式时返回
// ErrHeartbeatRemoteShared（不允许挂载）。共享与否是运行期所有权状态、不是实体种类的声明：刚加载的实例是 Unknown，
// 第二个写者首次接触共享实体时这里看不出来，由第一拍停掉（见 runBeat）。
func (h *Host) Register(name string, period time.Duration, fn Func, opts ...RegOption) (*Reg, error) {
	if h.closed.Load() {
		return nil, ErrHostClosed
	}
	if err := h.sharedErr(name); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("heartbeat: registration %q has a nil callback", name)
	}
	t, err := tierOf(period)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: registration %q: %w", name, err)
	}
	for _, r := range h.regs {
		if r.name == name {
			return nil, fmt.Errorf("%w: %q on entity %d", ErrDuplicateName, name, h.id)
		}
	}
	o := regOptions{durability: nest.DurabilityAsync}
	for _, opt := range opts {
		opt(&o)
	}
	handler, ok := beatHandlerFor(o.durability)
	if !ok {
		return nil, fmt.Errorf("heartbeat: registration %q: unsupported durability %v", name, o.durability)
	}
	r := &Reg{host: h, name: name, fn: fn, handler: handler, tier: t, domain: o.domain}
	r.params = nest.Params{r}
	// 追加在实体锁内；同时持 Driver.mu，让 OnDestroy（卸载流程里解锁之后调用）读到完整的列表。
	h.driver.mu.Lock()
	h.regs = append(h.regs, r)
	h.driver.mu.Unlock()
	return r, nil
}

// OnDestroy 让实例永久离开驱动：卸载、删档、撤销新建时由组件的 OnDestroy 调用，可重复。之后的启停一律忽略，
// 已经在队列里的拍到达时不执行（按过期处理）。组件漏调时，驱动在登记到期时发现实例已移除（IsRemoved）也会替它
// 做同样的事（计 dropped{reason="removed"} 一次），环与驱动不再持有这个实例。
func (h *Host) OnDestroy() {
	if h == nil || h.closed.Swap(true) {
		return
	}
	h.driver.close(h)
}

// Reg 是一个心跳登记。
type Reg struct {
	host    *Host
	name    string
	fn      Func
	handler nest.HandlerName
	params  nest.Params
	// domain 非 nil 时本登记按时间域计时（WithTimeDomain）：Beat.Now / Elapsed 与启动时刻都是域时间。
	domain *timedomain.Domain

	// 以下只在实体锁内读写：业务看到的状态（随事务回滚恢复）与回调侧的上次执行记录。
	active  bool
	tier    tier
	seq     uint64 // 每次启停 / 改档加一，驱动只接受更新的序号（AfterCommit 可能乱序到达）
	lastRun time.Time
	lastGen uint64

	// 以下只在 Driver.mu 下读写：驱动已生效的状态与环上位置。
	ring struct {
		seq    uint64
		active bool
		tier   tier
		cell   int32
		idx    int32
	}

	// closed 由 Close 置位（实体锁内），驱动 goroutine、提交后回调与执行侧都读。
	closed atomic.Bool

	// 原子：驱动与回调之间。
	startGen  atomic.Uint64 // 每次启动生效加一；回调据此判断 First
	startedAt atomic.Int64  // 最近一次启动生效的业务时间（挂了时间域时是域时间，UnixNano），先于 startGen 写
	inflight  atomic.Bool   // 已投递、回调还没执行完
	skipped   atomic.Int64  // 上次执行以来跳过的拍数

	// 只由驱动 goroutine 读写。
	deliveredFrame int64
	stalls         uint // 连续视为丢失的次数，决定下次的卡住阈值；看到不在途时清零
}

// Name 返回登记名。
func (r *Reg) Name() string { return r.name }

// GoString 只输出构造后不变的字段（登记名、实体 ID）。Nest 的慢派发 trace 在 handler 执行期间由另一个 goroutine 取
// 参数样本：实现了 fmt.GoStringer 的参数调用 GoString，其余非标量只给类型名（不按反射读可变字段）；所以这里不能读
// 回调正在原子写的在途 / 跳拍计数，也是登记名能出现在 trace 里的方式。
func (r *Reg) GoString() string {
	if r == nil {
		return "(*heartbeat.Reg)(nil)"
	}
	return fmt.Sprintf("&heartbeat.Reg{name:%q, entity:%d}", r.name, r.host.id)
}

// Active 返回业务视角的启停状态（实体锁内读；含本事务里尚未提交的启停）。
func (r *Reg) Active() bool { return r.active }

// Period 返回业务视角的档位。
func (r *Reg) Period() time.Duration { return r.tier.period() }

// Start 开始跳；已在跑时无操作。在事务里调用时提交后生效、回滚作废（nest.AfterCommit）；不在事务里立即生效。
// Remote 托管实体处于共享模式时不启动，返回 ErrHeartbeatRemoteShared；回到独占（LeaveRemoteSharedMode）后可以再 Start。
// 登记已 Close 时返回 ErrRegClosed。
func (r *Reg) Start() error {
	if r.closed.Load() {
		return fmt.Errorf("%w: %q on entity %d", ErrRegClosed, r.name, r.host.id)
	}
	if err := r.host.sharedErr(r.name); err != nil {
		return err
	}
	r.change(true, r.tier)
	return nil
}

// Stop 停止；停止生效后到达的在途拍不执行。生效方式同 Start。
func (r *Reg) Stop() { r.change(false, r.tier) }

// SetPeriod 换档：下一拍按新档位所在的格触发，Elapsed 仍从本登记上次执行起算。period 必须是 Periods 之一。
// 生效方式同 Start；停止状态下只记下档位，下次 Start 用它。登记已 Close 时返回 ErrRegClosed。
func (r *Reg) SetPeriod(period time.Duration) error {
	if r.closed.Load() {
		return fmt.Errorf("%w: %q on entity %d", ErrRegClosed, r.name, r.host.id)
	}
	t, err := tierOf(period)
	if err != nil {
		return err
	}
	r.change(r.active, t)
	return nil
}

// Close 撤销本登记：立即移出驱动、释放名字（同一实例上可以马上用同名再 Register），之后 Start / SetPeriod 返回
// ErrRegClosed、Stop 无操作，可重复调用。用于成员换场景（换时间域）时重建行为组件（actionflow.BeatRunner / Behavior、
// ai.Controller 的 Close）。只能在实体锁内调用。
//
// 与 Register 一样是组件的生命周期操作，立即生效、不随事务回滚（事务回滚恢复的是启停，不恢复登记本身）；同一事务里
// 更早的 Start / Stop 在提交后到达时被忽略。已经投出、还在队列里的拍到达时不执行回调（按过期丢弃，计
// dropped{reason="stale"}）；所以按名字看，旧登记的在途拍与新登记的第一拍可能同时在队列里，但只有新登记的会执行，
// “每个登记至多一拍在途”对新登记照常成立。
func (r *Reg) Close() {
	if r == nil || r.closed.Swap(true) {
		return
	}
	h, d := r.host, r.host.driver
	d.mu.Lock()
	if r.ring.active {
		d.ring.remove(r)
		r.ring.active = false
	}
	h.regs = slices.DeleteFunc(h.regs, func(other *Reg) bool { return other == r })
	d.mu.Unlock()
	r.active = false
}

// Closed 报告登记是否已 Close。
func (r *Reg) Closed() bool { return r.closed.Load() }

// change 改业务视角的状态并安排驱动生效。事务里：登记回滚恢复（DeferRollback）与提交后生效（AfterCommit），
// 生效带序号，晚到的旧变化被驱动丢弃；不在事务里（加载发布前、memory 快路径）：立即生效。
func (r *Reg) change(active bool, t tier) {
	if r.host.closed.Load() || r.closed.Load() || (active == r.active && t == r.tier) {
		return
	}
	prevActive, prevTier := r.active, r.tier
	r.active, r.tier = active, t
	r.seq++
	seq, d := r.seq, r.host.driver
	apply := func() { d.apply(r, seq, active, t) }
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		apply()
		return
	}
	tx.DeferRollback(func() error {
		r.active, r.tier = prevActive, prevTier
		return nil
	})
	tx.AfterCommit(apply)
}

// --- 心跳 handler ---

// 心跳 handler 按持久级别各注册一个（包级，进程内全部驱动共用）：参数是 *Reg，回滚策略 undo。
var beatHandlers = [...]struct {
	durability nest.DurabilityPolicy
	name       nest.HandlerName
}{
	{nest.DurabilityMemory, nest.NewHandlerName("heartbeat.beat.memory")},
	{nest.DurabilityAsync, nest.NewHandlerName("heartbeat.beat.async")},
	{nest.DurabilityStrict, nest.NewHandlerName("heartbeat.beat.strict")},
	{nest.DurabilityPipelined, nest.NewHandlerName("heartbeat.beat.pipelined")},
}

func init() {
	for _, h := range beatHandlers {
		nest.MustRegisterHandlerWithMeta(h.name, beatHandler, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: h.durability})
	}
}

func beatHandlerFor(policy nest.DurabilityPolicy) (nest.HandlerName, bool) {
	for _, h := range beatHandlers {
		if h.durability == policy {
			return h.name, true
		}
	}
	return nest.HandlerName{}, false
}

// HandlerNames 返回心跳 handler 名（诊断、按 handler 统计时用）。
func HandlerNames() []nest.HandlerName {
	out := make([]nest.HandlerName, len(beatHandlers))
	for i, h := range beatHandlers {
		out[i] = h.name
	}
	return out
}

func beatHandler(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	if len(es) != 1 || len(params) != 1 {
		return nil, fmt.Errorf("heartbeat: beat handler wants one target and one parameter, got %d / %d", len(es), len(params))
	}
	r, ok := params[0].(*Reg)
	if !ok || r == nil {
		return nil, fmt.Errorf("heartbeat: beat handler parameter is %T, want *heartbeat.Reg", params[0])
	}
	return nil, r.host.driver.runBeat(es[0], r)
}

// runBeat 在实体锁与事务内执行一拍。核对顺序：
//  1. 在途标志：没有在途（同一次投递的另一份已经执行过，见卡住兜底）就不执行；
//  2. 实例：消息的目标必须是登记所属的那个实例、且没有 OnDestroy（卸载后同 ID 重载的新实例收到旧实例的拍，按过期丢弃），
//     登记没有 Close（换场景重建后旧登记的在途拍，同样按过期丢弃）；
//  3. 业务视角的启停：锁序上更早提交的 Stop 已经让它停了；
//  4. Remote 托管实体：共享模式（Sharing / Shared）不允许挂载心跳，在这里停掉本登记（Stop 同样随本拍事务提交生效），
//     计数并由驱动汇总打 Warn，回调不执行；其余状态要求本进程实例是当前拥有方（LocalOwned）。
//
// 在途标志在返回时清掉（panic 也清），所以“上一拍没执行完”就是 handler 还没返回。
func (d *Driver) runBeat(target entity.IThreadSafeEntity, r *Reg) error {
	if !r.inflight.Load() {
		d.count(&d.stats.dropDuplicate, r.host.id)
		return nil
	}
	defer r.inflight.Store(false)
	h := r.host
	if target != h.owner || h.closed.Load() || r.closed.Load() {
		d.count(&d.stats.dropStale, h.id)
		return nil
	}
	if !r.active {
		d.count(&d.stats.dropStopped, h.id)
		return nil
	}
	if h.remote != nil {
		switch state := h.remote.RemoteOwnershipState(); {
		case remoteShared(state):
			// 运行期进入共享模式（EnterRemoteSharedMode，或第二个写者的第一拍经准入转到 Shared）：不允许挂载，停掉。
			r.change(false, r.tier)
			d.count(&d.stats.stoppedShared, h.id)
			d.sharedSample.Store(r)
			return nil
		case state != entity.RemoteOwnershipLocalOwned:
			d.count(&d.stats.dropNotOwner, h.id)
			return nil
		}
	}

	now, rate := d.Now(), 1.0
	if r.domain != nil {
		reading := r.domain.Read() // 这一拍消息的域时间与倍率，同一次冻结
		now, rate = reading.Now, reading.Rate
	}
	period := r.tier.period()
	gen := r.startGen.Load()
	first := gen != r.lastGen
	since := r.lastRun
	if first {
		since = time.Unix(0, r.startedAt.Load())
	}
	elapsed := now.Sub(since)
	limit := max(DefaultMaxElapsed, period)
	if r.domain != nil {
		limit = time.Duration(float64(limit) * rate) // 时停时为 0：Elapsed 为 0
	}
	elapsed = min(max(elapsed, 0), limit)
	skipped := r.skipped.Swap(0)

	// 回调侧记录随本拍事务回滚：回滚的一拍不算执行过，下一拍的 Elapsed 覆盖它。
	prevRun, prevGen := r.lastRun, r.lastGen
	r.lastRun, r.lastGen = now, gen
	if tx := nest.CurrentRollbackTx(); tx != nil {
		tx.DeferRollback(func() error {
			r.lastRun, r.lastGen = prevRun, prevGen
			r.skipped.Add(skipped)
			return nil
		})
	}
	d.count(&d.stats.executed, h.id)
	return r.fn(Beat{Now: now, Elapsed: elapsed, Period: period, Skipped: int(skipped), First: first})
}
