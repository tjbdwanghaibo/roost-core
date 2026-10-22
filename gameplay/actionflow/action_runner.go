package actionflow

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"math"
	"sort"
	"sync"
	"time"
)

var (
	ErrActionGroupInvalid = errors.New("actionflow: action group is invalid")
	ErrActionGroupFrozen  = errors.New("actionflow: action group is frozen")
	ErrActionIDExhausted  = errors.New("actionflow: action id exhausted")
	// ErrActionGroupNotFrozen：Recover 的组没有被冻结（Freeze / Recover 不成对，计数不会变成负数）。
	ErrActionGroupNotFrozen = errors.New("actionflow: action group is not frozen")
	// ErrActionNotFound：EndAction 的 ID 既不是某组的当前动作，也不在排队中（已经结束，或从未分配）。
	ErrActionNotFound = errors.New("actionflow: action not found")
	// ErrReplaceRejected：同组当前动作（Replacer）拒绝被新动作取代。
	ErrReplaceRejected = errors.New("actionflow: current action rejects replacement")
	// ErrBlockedByGroup：另一组的当前动作按 ActionRunnerConfig.Conflict 挡住了新动作。
	ErrBlockedByGroup = errors.New("actionflow: blocked by another group's running action")
	// ErrDeferredQueueFull：一次最外层调用里，回调发起的待执行命令超过
	// ActionRunnerConfig.MaxDeferredCommands。被拒的 Start / Enqueue 不分配 ID。
	ErrDeferredQueueFull = errors.New("actionflow: deferred command queue is full")
	// ErrDeferredRunaway：回调互相触发，一次最外层调用执行的延后命令超过
	// ActionRunnerConfig.MaxDeferredSteps。剩余命令被丢弃，已交出 ID 的动作各收到一次
	// 取消状态的 OnEnded；截停期间回调里的新变更直接返回本错误。
	ErrDeferredRunaway = errors.New("actionflow: deferred commands kept triggering more commands")
)

const (
	defaultMaxDeferredCommands = 64
	defaultMaxDeferredSteps    = 1024
)

// ActionSnapshot 是一个动作的身份快照：ID、所属任务、种类、组与动作本身。
type ActionSnapshot struct {
	ID        int64
	MissionID int64
	Kind      ActionKind
	Group     ActionGroup
	Action    Action
}

// ActionRunnerHooks 是 ActionRunner 的回调钩子，都在“执行中”运行，钩子里发起的变更会被延后。
type ActionRunnerHooks struct {
	// PopulateContext 是不分配内存的上下文钩子。Context 为兼容而保留，它的结果会被复制进 runner 持有的回调期存储。
	PopulateContext func(*ActionContext, time.Time)
	Context         func(time.Time) *ActionContext
	OnQueued        func(ActionSnapshot)
	OnTransition    func(ActionSnapshot, bool)
	OnEnded         func(ActionSnapshot, ActionReason)
	OnError         func(error)
	// OnFreeze 在组的冻结状态切换时调用：计数从 0 变 1 时 frozen=true，回到 0 时 frozen=false（BeatRunner 据此停 /
	// 恢复组的心跳）。只做记账，不要在里面改 runner。
	OnFreeze func(group ActionGroup, frozen bool)
}

// Conflict 是跨组冲突规则的结论（对照 ssr 的 NeedCancelCurAction 矩阵，这里是业务配置的函数）。
type Conflict uint8

const (
	// NoConflict：两组互不影响（缺省）。
	NoConflict Conflict = iota
	// BlockIncoming：另一组正在运行的动作挡住新动作：直接调用时 Start 返回 ErrBlockedByGroup，回调里延后的 Start
	// 与排队项以失败状态结束。
	BlockIncoming
	// PreemptRunning：先取消另一组正在运行的动作（取消状态的 OnEnded），再启动新动作。被抢占组的队列此时不推进
	// （排队项不会先启动、再被正在启动的抢占方挡住），等抢占方结束时推进一次（见 ActionRunner.preempt）。
	PreemptRunning
)

// ActionRunnerConfig 是 ActionRunner 的注册表、种类到组的映射、钩子与延后队列上限。
type ActionRunnerConfig struct {
	Registry     *Registry
	GroupForKind func(ActionKind) (ActionGroup, bool)
	Hooks        ActionRunnerHooks
	// Conflict 是可选的跨组冲突规则：一个动作要启动（Start，或排队项成为当前动作）时，对其他每个有当前动作的组
	// （按组号升序）调用 Conflict(running, incoming)。任一组 BlockIncoming 即拒绝新动作，什么都不改；否则取消全部
	// PreemptRunning 的组的当前动作，再按同组规则（Replacer）启动。nil 表示没有跨组规则。只做判断，panic 按
	// NoConflict 处理并经 OnError 报告。
	Conflict func(running, incoming ActionSnapshot) Conflict
	// MaxDeferredCommands 限制一次最外层调用里同时待执行的延后命令数（回调发起的变更），
	// 0 取默认 64。超出时变更方法返回 ErrDeferredQueueFull。
	MaxDeferredCommands int
	// MaxDeferredSteps 限制一次最外层调用总共执行的延后命令数，防止回调无限互相触发；
	// 0 取默认 1024。超出时最外层调用返回 ErrDeferredRunaway。
	MaxDeferredSteps int
}

type actionEntry struct {
	ActionSnapshot
	// preempted 表示它启动时抢占过别的组（那些组的 preemptedBy 记着它的 ID），结束时要推进它们的队列。
	preempted bool
}
type actionGroupState struct {
	// freezes 是冻结计数（Freeze 加一、Recover 减一），大于 0 即冻结。
	freezes int
	cur     *actionEntry
	next    []*actionEntry
	// advancing 表示延后队列里已有一条“推进本组队列”的内部命令，避免重复登记。
	advancing bool
	// preemptedBy 是抢占了本组当前动作、还没结束的动作 ID（0 表示没有）：它结束时本组若仍空闲就推进队列。
	preemptedBy int64
}

// commandOp 是 ActionRunner 的一种变更。公开的变更方法都先组成一条命令，再交给 submit：
// 不在回调里就立即执行，在回调里就进延后队列。
type commandOp uint8

const (
	opStart commandOp = iota + 1
	opEnqueue
	opTick
	opEnd
	opEndAll
	opClearQueue
	opClearMission
	opRecover
	opEndAction
	// opInterrupt 是 Freeze(FreezeCancel) 引出的命令：取消组的当前动作（不推进队列，组已冻结）。
	opInterrupt
	// opAdvance 是内部命令：组空出来（结束、启动失败、Enqueue 到空闲组、Recover）后启动
	// 队首。它也排在延后队列里，所以同一时刻回调里显式发起的 Start 先执行、排队项不会被
	// “先启动再被替换”。
	opAdvance
)

func (op commandOp) String() string {
	switch op {
	case opStart:
		return "start"
	case opEnqueue:
		return "enqueue"
	case opTick:
		return "tick"
	case opEnd:
		return "end"
	case opEndAll:
		return "end all"
	case opClearQueue:
		return "clear queue"
	case opClearMission:
		return "clear mission"
	case opRecover:
		return "recover"
	case opEndAction:
		return "end action"
	case opInterrupt:
		return "freeze interrupt"
	case opAdvance:
		return "advance queue"
	default:
		return "unknown"
	}
}

type runnerCommand struct {
	op        commandOp
	group     ActionGroup
	entry     *actionEntry // opStart / opEnqueue：已构建、ID 已交给调用方
	now       time.Time
	force     bool
	reason    ActionReason
	missionID int64
	actionID  int64 // opEndAction
	// outer 表示命令属于最外层调用自己（它本身，或它直接引出的队列推进）：错误返回给
	// 最外层调用方。回调发起的命令及其引出的推进错误经 OnError 报告——发起它的回调早已
	// 拿到 nil 返回。
	outer bool
}

// ActionRunner 刻意无锁。拥有它的实体必须用实体锁串行化每次调用，包括心跳驱动的 Tick。
//
// 回调里的变更延后执行：动作的 Start / Tick / Cancel 和
// OnQueued / OnTransition / OnEnded 钩子都在某次最外层调用“执行中”运行；这期间对本 runner
// 的 Start、Enqueue、Tick、End、EndAction、EndAll、ClearQueue、ClearMission、Recover 不立即生效，而是
// 按发起顺序进延后队列，最外层调用做完自己那一步后依次执行（执行中引出的回调再发起的
// 命令接着排在后面），全部执行完才返回。所以任一回调运行期间当前位、队列都不会被改，
// 不需要在每个回调点之后事后比对。
//
//   - 回调里的 Start / Enqueue 立即构建动作并返回已分配的 ID、错误为 nil（Deferring() 为真
//     即表示本次调用被延后）。拿到 ID 的动作一定有结论：启动后照常结束；延后执行时已无法
//     启动（组已冻结、被截停）也发一次失败 / 取消状态的 OnEnded，ai.TaskflowAction 这类按 ID
//     等结束的调用方不会挂住。
//   - 延后命令按“组 / 任务”而非发起时的快照解释，等价于回调返回后立刻按序调用。
//   - Freeze 的计数始终立即生效（FreezeCancel 的打断照常延后）；Update 在回调里立即对当前动作调用 fn。
//     读方法（Current、QueueLength、Pending 等）在回调里读到的是尚未执行延后命令的状态。
//   - 队列有界（MaxDeferredCommands → ErrDeferredQueueFull），一次最外层调用执行的命令
//     总数有界（MaxDeferredSteps → ErrDeferredRunaway），回调互相触发不会无限执行。
type ActionRunner struct {
	registry     *Registry
	groupForKind func(ActionKind) (ActionGroup, bool)
	conflict     func(running, incoming ActionSnapshot) Conflict
	hooks        ActionRunnerHooks
	nextID       int64
	groups       map[ActionGroup]*actionGroupState
	contextPool  sync.Pool

	maxDeferred      int
	maxDeferredSteps int
	// executing 为真表示某次最外层调用正在执行（含它触发的全部回调）。这是“回调期间”
	// 的唯一判定点。
	executing bool
	// applyingOuter 记录正在执行的命令是否属于最外层调用，决定它引出的推进命令归属。
	applyingOuter bool
	// runaway 为真表示本次最外层调用已超出执行预算，正在丢弃剩余命令。
	runaway  bool
	deferred []runnerCommand
	head     int
}

// NewActionRunner 构造 ActionRunner；缺少 Registry 或 GroupForKind 时返回 ErrActionGroupInvalid。
func NewActionRunner(config ActionRunnerConfig) (*ActionRunner, error) {
	if config.Registry == nil || config.GroupForKind == nil {
		return nil, ErrActionGroupInvalid
	}
	runner := &ActionRunner{
		registry:         config.Registry,
		groupForKind:     config.GroupForKind,
		conflict:         config.Conflict,
		hooks:            config.Hooks,
		groups:           make(map[ActionGroup]*actionGroupState),
		maxDeferred:      config.MaxDeferredCommands,
		maxDeferredSteps: config.MaxDeferredSteps,
	}
	if runner.maxDeferred <= 0 {
		runner.maxDeferred = defaultMaxDeferredCommands
	}
	if runner.maxDeferredSteps <= 0 {
		runner.maxDeferredSteps = defaultMaxDeferredSteps
	}
	runner.contextPool.New = func() any { return new(ActionContext) }
	return runner, nil
}

// Deferring 报告此刻对本 runner 的变更调用是否会被延后（正处在某次调用的回调里）。
// 回调里的 Start / Enqueue 返回的 ID 已分配，动作在回调返回后才启动 / 入队。
func (r *ActionRunner) Deferring() bool { return r.executing }

// Start 启动一个动作，替换组里的当前动作（排队项保留）。在回调里调用时延后执行，返回
// 已分配的 ID 与 nil。
func (r *ActionRunner) Start(kind ActionKind, param any, missionID int64, now time.Time) (int64, error) {
	group, err := r.admit(kind)
	if err != nil {
		return 0, err
	}
	if !r.executing {
		// 立即执行：冻结在构建前就拒绝，不消耗 ID（与延后前的行为一致）。
		if unit := r.groups[group]; unit != nil && unit.freezes > 0 {
			return 0, ErrActionGroupFrozen
		}
	}
	entry, err := r.build(kind, param, missionID, group)
	if err != nil {
		return 0, err
	}
	if err := r.submit(runnerCommand{op: opStart, group: group, entry: entry, now: now}); err != nil {
		return 0, err
	}
	return entry.ID, nil
}

// Enqueue 把动作排到组队列末尾；组空闲且未冻结时随即启动队首。在回调里调用时延后执行，
// 返回已分配的 ID 与 nil。立即执行时，返回的错误来自随之启动失败的排队项，ID 仍有效。
func (r *ActionRunner) Enqueue(kind ActionKind, param any, missionID int64) (int64, error) {
	group, err := r.admit(kind)
	if err != nil {
		return 0, err
	}
	entry, err := r.build(kind, param, missionID, group)
	if err != nil {
		return 0, err
	}
	return entry.ID, r.submit(runnerCommand{op: opEnqueue, group: group, entry: entry})
}

// Current 返回组的当前动作；没有时返回 nil。
func (r *ActionRunner) Current(group ActionGroup) Action {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return nil
	}
	return unit.cur.Action
}

// CurrentSnapshot 返回组当前动作的快照。
func (r *ActionRunner) CurrentSnapshot(group ActionGroup) (ActionSnapshot, bool) {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return ActionSnapshot{}, false
	}
	return unit.cur.ActionSnapshot, true
}

// QueueLength 返回组里排队的动作数。
func (r *ActionRunner) QueueLength(group ActionGroup) int {
	unit := r.groups[group]
	if unit == nil {
		return 0
	}
	return len(unit.next)
}

// Pending 按顺序返回组里排队动作的快照副本。
func (r *ActionRunner) Pending(group ActionGroup) []ActionSnapshot {
	unit := r.groups[group]
	if unit == nil || len(unit.next) == 0 {
		return nil
	}
	pending := make([]ActionSnapshot, 0, len(unit.next))
	for _, entry := range unit.next {
		if entry != nil {
			pending = append(pending, entry.ActionSnapshot)
		}
	}
	return pending
}

// Update 对组的当前动作调用 fn。在回调里立即调用；不在回调里时 fn 运行期间同样算回调，
// fn 里的变更在 fn 返回后执行。fn 的 panic 与动作回调一样恢复成错误返回：fn 运行期间
// executing 已置位，panic 直接穿出会让它永远不复位，之后的变更全被当成延后命令、再没有
// 人执行。
func (r *ActionRunner) Update(group ActionGroup, fn func(Action) error) error {
	if fn == nil {
		return nil
	}
	unit := r.groups[group]
	if unit == nil || unit.cur == nil || unit.cur.Action == nil {
		return nil
	}
	if r.executing {
		return callUpdate(fn, unit.cur.Action)
	}
	r.executing = true
	defer r.finishExecution()
	err := callUpdate(fn, unit.cur.Action)
	return errors.Join(err, r.drain())
}

// Tick 推进组的当前动作；动作结束时启动队首。在回调里调用时延后执行。
func (r *ActionRunner) Tick(group ActionGroup, now time.Time) error {
	return r.submit(runnerCommand{op: opTick, group: group, now: now})
}

// End 结束组的当前动作（force 时取消），并启动队首。在回调里调用时延后执行。
func (r *ActionRunner) End(group ActionGroup, force bool, reason ActionReason) error {
	return r.submit(runnerCommand{op: opEnd, group: group, force: force, reason: reason})
}

// EndAll 结束每个组的当前动作并丢弃排队项。回调（如 OnEnded 里推进任务）随之发起的
// 变更在全部结束之后按序执行，所以 EndAll 返回时可能已有回调启动的新动作在运行。
// EndAll 不是“清场”：要让什么都不再运行，先结束当前任务（MissionRunner.EndCurMission，
// 任务结束后送来的动作结束被忽略、不再推进），再调 EndAll。
func (r *ActionRunner) EndAll(force bool, reason ActionReason) error {
	return r.submit(runnerCommand{op: opEndAll, force: force, reason: reason})
}

// ClearQueue 丢弃全部排队动作；每个被丢弃的动作发一次 OnEnded（取消）。
func (r *ActionRunner) ClearQueue() {
	if err := r.submit(runnerCommand{op: opClearQueue}); err != nil {
		r.report(err)
	}
}

// ClearMission 结束属于该任务的当前动作并丢弃它的排队动作；missionID 为 0 时是空操作。
func (r *ActionRunner) ClearMission(missionID int64, cancel bool, reason ActionReason) error {
	if missionID == 0 {
		return nil
	}
	return r.submit(runnerCommand{op: opClearMission, missionID: missionID, force: cancel, reason: reason})
}

// EndAction 按 ID 结束一个动作：是某组的当前动作时按 force 结束（force 时调 Cancel、取消状态）并推进
// 该组队列；在排队中时移出队列并发一次取消状态的 OnEnded。直接调用时找不到返回 ErrActionNotFound；在回调里调用时
// 延后执行，执行时已经结束的动作被忽略（不报告）。
func (r *ActionRunner) EndAction(id int64, force bool, reason ActionReason) error {
	if id <= 0 {
		return ErrActionNotFound
	}
	return r.submit(runnerCommand{op: opEndAction, actionID: id, force: force, reason: reason})
}

// FreezeMode 是冻结对组的当前动作的处理。
type FreezeMode uint8

const (
	// FreezePause：暂停——当前动作留在组里但不被 tick，拒绝新动作，队列不推进；解冻后当前动作接着跑。
	FreezePause FreezeMode = iota
	// FreezeCancel：打断——取消当前动作（取消状态的 OnEnded，原因 "action group frozen"），其余同 FreezePause；
	// 排队项保留，解冻后推进。对照 ssr 的 Frozen（定身打断移动）。
	FreezeCancel
)

// Freeze 冻结组：冻结是计数的，多个来源（定身、缴械、剧情……）可以同时冻结同一组，每次 Freeze 对应一次
// Recover，计数归零才解冻。冻结期间 Start 被拒绝（ErrActionGroupFrozen）、当前动作不被 tick、队列不推进。计数
// 立即生效（不延后）；mode 为 FreezeCancel 时还取消组的当前动作——这一步有回调，在回调里调用时延后执行。返回的
// 错误只来自被打断动作的 Cancel。
func (r *ActionRunner) Freeze(group ActionGroup, mode FreezeMode) error {
	unit := r.group(group)
	unit.freezes++
	if unit.freezes == 1 {
		r.freezeChanged(group, true)
	}
	if mode != FreezeCancel {
		return nil
	}
	return r.submit(runnerCommand{op: opInterrupt, group: group})
}

// Frozen 报告组是否被冻结（计数大于 0）。
func (r *ActionRunner) Frozen(group ActionGroup) bool {
	unit := r.groups[group]
	return unit != nil && unit.freezes > 0
}

// FreezeCount 返回组的冻结计数（诊断用）。
func (r *ActionRunner) FreezeCount(group ActionGroup) int {
	if unit := r.groups[group]; unit != nil {
		return unit.freezes
	}
	return 0
}

// Recover 撤销一次 Freeze；计数归零时解冻，组空闲则随即推进队列。没有冻结时返回 ErrActionGroupNotFrozen（直接
// 调用）或经 OnError 报告（回调里）。在回调里调用时延后执行。
func (r *ActionRunner) Recover(group ActionGroup, now time.Time) error {
	return r.submit(runnerCommand{op: opRecover, group: group, now: now})
}

// admit 解析动作所属的组，并在回调里预先检查延后队列能否接纳，被拒时不分配 ID。
func (r *ActionRunner) admit(kind ActionKind) (ActionGroup, error) {
	group, ok, err := r.resolveGroup(kind)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: kind=%d", ErrActionGroupInvalid, kind)
	}
	if r.executing {
		if err := r.deferrable(); err != nil {
			return 0, err
		}
	}
	return group, nil
}

func (r *ActionRunner) deferrable() error {
	if r.runaway {
		return ErrDeferredRunaway
	}
	if len(r.deferred)-r.head >= r.maxDeferred {
		return ErrDeferredQueueFull
	}
	return nil
}

// submit 是变更的唯一入口：回调期间（executing）进延后队列，否则作为最外层调用立即
// 执行，并在返回前执行完全部延后命令。
func (r *ActionRunner) submit(cmd runnerCommand) error {
	if r.executing {
		if err := r.deferrable(); err != nil {
			return err
		}
		r.deferred = append(r.deferred, cmd)
		return nil
	}
	r.executing = true
	defer r.finishExecution()
	cmd.outer = true
	r.applyingOuter = true
	err := r.apply(cmd)
	return errors.Join(err, r.drain())
}

// finishExecution 由最外层入口负责复位，不能依赖 apply 能正常返回到 drain。
// 这只释放执行标记和命令引用，不吞掉框架内部 panic，也不承诺回滚动作状态。
func (r *ActionRunner) finishExecution() {
	for _, cmd := range r.deferred[r.head:] {
		if cmd.op == opAdvance {
			if unit := r.groups[cmd.group]; unit != nil {
				unit.advancing = false
			}
		}
	}
	clear(r.deferred)
	r.deferred = r.deferred[:0]
	r.head = 0
	r.executing = false
	r.applyingOuter = false
	r.runaway = false
}

// drain 按序执行延后命令。命令执行中引出的回调再发起的命令排在
// 队尾，同样在这里执行；执行总数超过预算时截停（abortDeferred）。返回属于最外层调用的错误。
func (r *ActionRunner) drain() (err error) {
	steps := 0
	for r.head < len(r.deferred) {
		if steps >= r.maxDeferredSteps {
			return errors.Join(err, r.abortDeferred())
		}
		steps++
		cmd := r.deferred[r.head]
		r.deferred[r.head] = runnerCommand{}
		r.head++
		r.applyingOuter = cmd.outer
		applyErr := r.apply(cmd)
		switch {
		case applyErr == nil:
		case cmd.outer:
			err = errors.Join(err, applyErr)
		case cmd.op != opAdvance:
			// 推进命令自己已经报告了排队项的启动失败，这里不重复。
			r.report(fmt.Errorf("actionflow: deferred %s: %w", cmd.op, applyErr))
		}
	}
	return err
}

// abortDeferred 丢弃预算之外的命令。已交出 ID 的 Start / Enqueue 各发一次取消状态的
// OnEnded；runaway 置位期间回调里的新变更直接返回 ErrDeferredRunaway，丢弃过程本身不会
// 再引出命令。被丢弃的推进命令会让对应组的队列停在原处，直到下一次变更再推进。
func (r *ActionRunner) abortDeferred() error {
	r.runaway = true
	dropped := 0
	reason := ActionReason{Message: ErrDeferredRunaway.Error(), Err: ErrDeferredRunaway,
		Result: ActionResult{Status: ActionStatusCanceled, Reason: ErrDeferredRunaway.Error()}}
	for r.head < len(r.deferred) {
		cmd := r.deferred[r.head]
		r.deferred[r.head] = runnerCommand{}
		r.head++
		dropped++
		if cmd.op == opAdvance {
			if unit := r.groups[cmd.group]; unit != nil {
				unit.advancing = false
			}
			continue
		}
		if cmd.entry != nil {
			r.ended(cmd.entry, reason)
		}
	}
	err := fmt.Errorf("%w: budget %d exhausted, %d command(s) dropped", ErrDeferredRunaway, r.maxDeferredSteps, dropped)
	r.report(err)
	return err
}

// apply 执行一条命令。执行期间 executing 恒为真，回调发起的变更只会进延后队列，所以这里
// 的每一步之后当前位都还是自己刚设置的值，不需要事后比对。
func (r *ActionRunner) apply(cmd runnerCommand) error {
	switch cmd.op {
	case opStart:
		return r.applyStart(cmd)
	case opEnqueue:
		unit := r.group(cmd.group)
		unit.next = append(unit.next, cmd.entry)
		r.queued(cmd.entry)
		if unit.cur == nil && unit.freezes == 0 {
			r.scheduleAdvance(unit, cmd.group, time.Time{})
		}
		return nil
	case opTick:
		return r.applyTick(cmd.group, cmd.now)
	case opEnd:
		unit := r.groups[cmd.group]
		if unit == nil || unit.cur == nil {
			return nil
		}
		return r.finish(unit, cmd.force, cmd.reason, true)
	case opEndAll:
		var errs []error
		for _, unit := range r.orderedGroups() {
			if unit.cur != nil {
				errs = append(errs, r.finish(unit, cmd.force, cmd.reason, false))
			}
			r.discardQueued(unit, nil, cmd.reason)
		}
		return errors.Join(errs...)
	case opClearQueue:
		for _, unit := range r.orderedGroups() {
			r.discardQueued(unit, nil, NewActionReason("queue cleared"))
		}
		return nil
	case opClearMission:
		var errs []error
		for _, unit := range r.orderedGroups() {
			if unit.cur != nil && unit.cur.MissionID == cmd.missionID {
				errs = append(errs, r.finish(unit, cmd.force, cmd.reason, false))
			}
			r.discardQueued(unit, func(entry *actionEntry) bool { return entry.MissionID == cmd.missionID }, cmd.reason)
		}
		return errors.Join(errs...)
	case opRecover:
		unit := r.group(cmd.group)
		if unit.freezes == 0 {
			return ErrActionGroupNotFrozen
		}
		unit.freezes--
		if unit.freezes > 0 {
			return nil
		}
		r.freezeChanged(cmd.group, false)
		if unit.cur == nil {
			r.scheduleAdvance(unit, cmd.group, cmd.now)
		}
		return nil
	case opEndAction:
		return r.applyEndAction(cmd)
	case opInterrupt:
		unit := r.groups[cmd.group]
		if unit == nil || unit.cur == nil || unit.freezes == 0 {
			return nil
		}
		reason := ActionReason{Message: "action group frozen", Err: ErrActionGroupFrozen,
			Result: ActionResult{Status: ActionStatusCanceled, Reason: "action group frozen"}}
		return r.finish(unit, true, reason, false)
	case opAdvance:
		return r.applyAdvance(cmd)
	default:
		return fmt.Errorf("actionflow: unknown runner command %d", cmd.op)
	}
}

// applyStart 启动一个已构建的动作，替换当前动作。延后的 Start 已把 ID 交出，执行时组被
// 冻结也要发 OnEnded 收尾。
//
// 被替换动作的 Cancel 失败（panic）只经 OnError 报告，不阻止新动作启动：finish 已经把旧动作结束（清当前位、发
// OnEnded），若在这里整体返回，新动作既不启动也没有 OnEnded——回调里交出的 ID 永远没有结论，直接调用时组空着、
// 队列也不推进。Start 的错误只表示请求的动作没有在运行。
//
// 判定顺序：冻结 → 跨组冲突（任一 BlockIncoming 拒绝，什么都不改）→ 同组当前动作的 Replacer
// （Keep：新动作以取消结束；Reject：拒绝）→ 取消要抢占的其他组的当前动作 → 取代本组当前动作 → 启动。被拒绝的
// 新动作：直接调用时返回错误、不发 OnEnded（与冻结一致，调用方没拿到 ID）；延后的（ID 已交出）发一次失败状态的
// OnEnded。
func (r *ActionRunner) applyStart(cmd runnerCommand) error {
	unit := r.group(cmd.group)
	entry := cmd.entry
	refuse := func(err error) error {
		if !cmd.outer {
			r.ended(entry, NewActionErrorReason(err))
		}
		return err
	}
	if unit.freezes > 0 {
		return refuse(ErrActionGroupFrozen)
	}
	preempt, err := r.crossGroup(entry)
	if err != nil {
		return refuse(err)
	}
	if unit.cur != nil {
		switch r.replaceDecision(unit.cur, entry, cmd.now) {
		case ReplaceKeep:
			r.ended(entry, ActionReason{Message: "kept current action", Result: ActionResult{Status: ActionStatusCanceled, Reason: "kept current action"}})
			return nil
		case ReplaceReject:
			return refuse(ErrReplaceRejected)
		}
	}
	r.preempt(preempt, entry)
	if unit.cur != nil {
		replaced := unit.cur.ID
		if err := r.finish(unit, true, NewActionReason("replaced by next action"), false); err != nil {
			r.report(fmt.Errorf("actionflow: replaced action %d: %w", replaced, err))
		}
	}
	return r.start(unit, entry, cmd.now)
}

// crossGroup 按 Conflict 判定其他组的当前动作（组号升序）：返回要抢占的组；任一组 BlockIncoming 时返回
// ErrBlockedByGroup（不抢占任何组）。
func (r *ActionRunner) crossGroup(incoming *actionEntry) ([]*actionGroupState, error) {
	if r.conflict == nil {
		return nil, nil
	}
	var preempt []*actionGroupState
	for _, unit := range r.orderedGroups() {
		if unit.cur == nil || unit.cur.Group == incoming.Group {
			continue
		}
		switch r.callConflict(unit.cur.ActionSnapshot, incoming.ActionSnapshot) {
		case BlockIncoming:
			return nil, fmt.Errorf("%w: group %d action %d (kind %d)", ErrBlockedByGroup, unit.cur.Group, unit.cur.ID, unit.cur.Kind)
		case PreemptRunning:
			preempt = append(preempt, unit)
		}
	}
	return preempt, nil
}

// preempt 取消被抢占的组的当前动作：取消状态，此时不推进那一组的队列——抢占方随后启动，排队项若现在启动，跨组
// 冲突会让它先启动再被挡住或互相抢占。被抢占组记下抢占方，抢占方结束（任何方式：完成、被结束、被
// 取代、启动失败）时由 resumePreempted 推进一次，排队项保留、照常成为当前动作，按 ID 等结果的调用方
// （ai.TaskflowAction、PlanMission）不会永久挂住（打断只停下正在做的事，排队的计划去留由业务决定）。
func (r *ActionRunner) preempt(units []*actionGroupState, by *actionEntry) {
	for _, unit := range units {
		if unit.cur == nil {
			continue
		}
		unit.preemptedBy = by.ID
		by.preempted = true
		victim := unit.cur.ID
		reason := NewActionReason(fmt.Sprintf("preempted by action %d", by.ID))
		if err := r.finish(unit, true, reason, false); err != nil {
			r.report(fmt.Errorf("actionflow: preempted action %d: %w", victim, err))
		}
	}
}

func (r *ActionRunner) callConflict(running, incoming ActionSnapshot) (conflict Conflict) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.report(fmt.Errorf("actionflow: conflict rule panic: %v", recovered))
			conflict = NoConflict
		}
	}()
	return r.conflict(running, incoming)
}

// replaceDecision 问当前动作是否同意被 next 取代（未实现 Replacer 即同意）。
func (r *ActionRunner) replaceDecision(cur, next *actionEntry, now time.Time) (decision ReplaceDecision) {
	replacer, ok := cur.Action.(Replacer)
	if !ok {
		return ReplaceAllow
	}
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			r.report(fmt.Errorf("actionflow: action %d replace check panic: %v", cur.ID, recovered))
			decision = ReplaceAllow
		}
	}()
	return replacer.ReplaceBy(ctx, next.ActionSnapshot)
}

// applyEndAction 按 ID 结束当前或排队的动作。只有最外层调用自己的命令在找不到时返回错误：延后执行时动作可能已经
// 在这之前结束，不是错误。
func (r *ActionRunner) applyEndAction(cmd runnerCommand) error {
	for _, unit := range r.orderedGroups() {
		if unit.cur != nil && unit.cur.ID == cmd.actionID {
			return r.finish(unit, cmd.force, cmd.reason, true)
		}
		for _, entry := range unit.next {
			if entry != nil && entry.ID == cmd.actionID {
				r.discardQueued(unit, func(e *actionEntry) bool { return e.ID == cmd.actionID }, cmd.reason)
				return nil
			}
		}
	}
	if cmd.outer {
		return fmt.Errorf("%w: %d", ErrActionNotFound, cmd.actionID)
	}
	return nil
}

func (r *ActionRunner) applyTick(group ActionGroup, now time.Time) error {
	unit := r.groups[group]
	if unit == nil || unit.freezes > 0 || unit.cur == nil || unit.cur.Action == nil {
		return nil
	}
	done, result, err := r.callTick(unit.cur.Action, now)
	if err != nil {
		result = ActionResult{Status: ActionStatusFailed, Reason: err.Error()}
		done = true
		r.report(err)
	}
	if !done {
		return nil
	}
	if result.Status == ActionStatusIdle {
		result.Status = ActionStatusSuccess
	}
	return r.finish(unit, false, NewActionResultReason(result), true)
}

// applyAdvance 启动组的队首（一次只试一个）。启动失败时排队项已收到 OnEnded 并经 OnError
// 报告，再登记一次推进接着试下一个——排在失败回调发起的命令之后。
func (r *ActionRunner) applyAdvance(cmd runnerCommand) error {
	unit := r.groups[cmd.group]
	if unit == nil {
		return nil
	}
	unit.advancing = false
	if unit.cur != nil || unit.freezes > 0 || len(unit.next) == 0 {
		return nil
	}
	next := unit.next[0]
	copy(unit.next, unit.next[1:])
	unit.next[len(unit.next)-1] = nil
	unit.next = unit.next[:len(unit.next)-1]
	// 排队项成为当前动作同样受跨组冲突约束（同组此时空闲，不涉及 Replacer）。
	preempt, err := r.crossGroup(next)
	if err == nil {
		r.preempt(preempt, next)
		err = r.start(unit, next, cmd.now)
	} else {
		r.ended(next, NewActionErrorReason(err))
	}
	if err == nil {
		return nil
	}
	r.report(fmt.Errorf("actionflow: queued action %d start: %w", next.ID, err))
	r.scheduleAdvance(unit, cmd.group, cmd.now)
	return err
}

// scheduleAdvance 在延后队列末尾登记一次队列推进（每组至多一条，不受 MaxDeferredCommands
// 限制，总量不超过组数）。推进归属于正在执行的命令：最外层调用直接引出的推进，错误返回
// 给最外层调用方。
func (r *ActionRunner) scheduleAdvance(unit *actionGroupState, group ActionGroup, now time.Time) {
	if unit.advancing || r.runaway {
		return
	}
	unit.advancing = true
	r.deferred = append(r.deferred, runnerCommand{op: opAdvance, group: group, now: now, outer: r.applyingOuter})
}

// discardQueued 把一个组里命中 match（nil 表示全部）的排队动作移出队列，再为每个发一次
// OnEnded。排队动作已经发过 OnQueued、ID 也交给了 Enqueue 的调用方（ai.TaskflowAction 等
// 这个 ID 的结束），静默丢弃会让等待方永远等不到结果。它们从未
// 启动，所以不调 Cancel、不发激活切换，结束状态一律是取消。Hook 里发起的新排队在延后
// 队列里，丢弃完才入队，不被这次丢弃误伤。
func (r *ActionRunner) discardQueued(unit *actionGroupState, match func(*actionEntry) bool, reason ActionReason) {
	if unit == nil || len(unit.next) == 0 {
		return
	}
	var kept, dropped []*actionEntry
	for _, entry := range unit.next {
		switch {
		case entry == nil:
		case match == nil || match(entry):
			dropped = append(dropped, entry)
		default:
			kept = append(kept, entry)
		}
	}
	unit.next = kept
	if len(dropped) == 0 {
		return
	}
	message := reason.Message
	if message == "" && reason.Err != nil {
		message = reason.Err.Error()
	}
	canceled := ActionReason{Message: message, Err: reason.Err, Result: ActionResult{Status: ActionStatusCanceled, Reason: message}}
	for _, entry := range dropped {
		r.ended(entry, canceled)
	}
}

func (r *ActionRunner) build(kind ActionKind, param any, missionID int64, group ActionGroup) (*actionEntry, error) {
	if r.nextID == math.MaxInt64 {
		return nil, ErrActionIDExhausted
	}
	action, err := r.registry.BuildAction(kind, param)
	if err != nil {
		return nil, err
	}
	r.nextID++
	entry := &actionEntry{ActionSnapshot: ActionSnapshot{ID: r.nextID, MissionID: missionID, Kind: kind, Group: group, Action: action}}
	return entry, nil
}

func (r *ActionRunner) resolveGroup(kind ActionKind) (group ActionGroup, ok bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("actionflow: group resolver panic for kind %d: %v", kind, recovered)
		}
	}()
	group, ok = r.groupForKind(kind)
	return group, ok, nil
}

// start 把 entry 装到当前位：进入切换 → 动作 Start。Start 失败时调 Cancel、离开切换、清当前
// 位，并发一次失败状态的 OnEnded——进入切换的动作恰好一次离开、一次结束，直接 Start 与
// 排队 / 延后启动一致。Start 报告启动即完成时按 Tick 结束同样收尾（不调 Cancel，结果为空算成功，推进队列）。
// 回调里的变更都在延后队列，这期间当前位一直是 entry。
func (r *ActionRunner) start(unit *actionGroupState, entry *actionEntry, now time.Time) error {
	if unit == nil || entry == nil || entry.Action == nil {
		return ErrBuilderNil
	}
	unit.cur = entry
	r.transition(entry, true)
	done, result, err := r.callStart(entry.Action, now)
	if err == nil && done {
		if result.Status == ActionStatusIdle {
			result.Status = ActionStatusSuccess
		}
		return r.finish(unit, false, NewActionResultReason(result), true)
	}
	if err == nil {
		return nil
	}
	cancelErr := r.callCancel(entry.Action, now, "start failed")
	unit.cur = nil
	r.transition(entry, false)
	err = errors.Join(err, cancelErr)
	r.ended(entry, NewActionErrorReason(err))
	r.resumePreempted(entry)
	return err
}

// finish 结束组的当前动作：需要时调 Cancel → 清当前位 → 离开切换 → OnEnded；startNext
// 时登记一次队列推进（排在结束回调发起的命令之后）。
func (r *ActionRunner) finish(unit *actionGroupState, cancel bool, reason ActionReason, startNext bool) error {
	entry := unit.cur
	var err error
	if cancel && entry.Action != nil {
		if reason.Result.Status == ActionStatusIdle {
			reason.Result = ActionResult{Status: ActionStatusCanceled, Reason: reason.Message}
		}
		err = r.callCancel(entry.Action, time.Time{}, reason.Message)
	}
	unit.cur = nil
	r.transition(entry, false)
	r.ended(entry, reason)
	if startNext && unit.freezes == 0 {
		r.scheduleAdvance(unit, entry.Group, time.Time{})
	}
	r.resumePreempted(entry)
	return err
}

// resumePreempted 在 entry 不再是当前动作时，推进被它抢占、仍记着它的组的队列（见 preempt）。推进排在延后队列末尾，
// 在本次结束引出的命令之后执行，照常经冻结与跨组冲突判定：组此时被冻结的等 Recover 推进；业务已经在这组启动了别的
// 动作的不动（applyAdvance 只推进空闲的组）；此刻另一组的动作挡住排队项时，排队项按现有规则以失败结束。
func (r *ActionRunner) resumePreempted(entry *actionEntry) {
	if !entry.preempted {
		return
	}
	entry.preempted = false
	for _, group := range r.sortedGroupKeys() {
		unit := r.groups[group]
		if unit.preemptedBy != entry.ID {
			continue
		}
		unit.preemptedBy = 0
		if unit.cur == nil && unit.freezes == 0 && len(unit.next) > 0 {
			r.scheduleAdvance(unit, group, time.Time{})
		}
	}
}

func (r *ActionRunner) group(group ActionGroup) *actionGroupState {
	unit := r.groups[group]
	if unit == nil {
		unit = &actionGroupState{}
		r.groups[group] = unit
	}
	return unit
}

func (r *ActionRunner) sortedGroupKeys() []ActionGroup {
	keys := make([]ActionGroup, 0, len(r.groups))
	for group := range r.groups {
		keys = append(keys, group)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (r *ActionRunner) orderedGroups() []*actionGroupState {
	keys := r.sortedGroupKeys()
	groups := make([]*actionGroupState, 0, len(keys))
	for _, group := range keys {
		groups = append(groups, r.groups[group])
	}
	return groups
}

func (r *ActionRunner) acquireContext(now time.Time) (ctx *ActionContext) {
	if now.IsZero() {
		now = fctx.Now() // 业务时间：本次执行冻结的 fctx.Now
	}
	ctx = r.contextPool.Get().(*ActionContext)
	*ctx = ActionContext{Now: now}
	if r.hooks.PopulateContext != nil || r.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("actionflow: action context hook panic: %v", recovered))
				*ctx = ActionContext{Now: now}
			}
		}()
		if r.hooks.PopulateContext != nil {
			r.hooks.PopulateContext(ctx, now)
		} else if candidate := r.hooks.Context(now); candidate != nil {
			*ctx = *candidate
		}
	}
	return ctx
}

func (r *ActionRunner) releaseContext(ctx *ActionContext) {
	if ctx == nil {
		return
	}
	*ctx = ActionContext{}
	r.contextPool.Put(ctx)
}

func (r *ActionRunner) callStart(action Action, now time.Time) (bool, ActionResult, error) {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionStart(action, ctx)
}

func (r *ActionRunner) callTick(action Action, now time.Time) (bool, ActionResult, error) {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionTick(action, ctx)
}

func (r *ActionRunner) callCancel(action Action, now time.Time, reason string) error {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionCancel(action, ctx, reason)
}

func (r *ActionRunner) transition(entry *actionEntry, active bool) {
	if r.hooks.OnTransition != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("actionflow: action transition hook panic: %v", recovered))
			}
		}()
		r.hooks.OnTransition(entry.ActionSnapshot, active)
	}
}
func (r *ActionRunner) queued(entry *actionEntry) {
	if r.hooks.OnQueued != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("actionflow: action queued hook panic: %v", recovered))
			}
		}()
		r.hooks.OnQueued(entry.ActionSnapshot)
	}
}
func (r *ActionRunner) ended(entry *actionEntry, reason ActionReason) {
	if r.hooks.OnEnded != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("actionflow: action ended hook panic: %v", recovered))
			}
		}()
		r.hooks.OnEnded(entry.ActionSnapshot, reason)
	}
}
func (r *ActionRunner) freezeChanged(group ActionGroup, frozen bool) {
	if r.hooks.OnFreeze != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("actionflow: freeze hook panic: %v", recovered))
			}
		}()
		r.hooks.OnFreeze(group, frozen)
	}
}
func (r *ActionRunner) report(err error) {
	if err != nil && r.hooks.OnError != nil {
		defer func() { _ = recover() }()
		r.hooks.OnError(err)
	}
}

func callUpdate(fn func(Action) error, action Action) (err error) {
	defer recoverActionPanic("update", &err)
	return fn(action)
}
func callActionStart(action Action, ctx *ActionContext) (done bool, result ActionResult, err error) {
	defer recoverActionPanic("start", &err)
	return action.Start(ctx)
}
func callActionTick(action Action, ctx *ActionContext) (done bool, result ActionResult, err error) {
	defer recoverActionPanic("tick", &err)
	done, result = action.Tick(ctx)
	return done, result, nil
}
func callActionCancel(action Action, ctx *ActionContext, reason string) (err error) {
	defer recoverActionPanic("cancel", &err)
	action.Cancel(ctx, reason)
	return nil
}
func recoverActionPanic(operation string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("actionflow: action %s panic: %v", operation, recovered)
	}
}
