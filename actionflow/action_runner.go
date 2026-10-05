package actionflow

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

var (
	ErrActionGroupInvalid = errors.New("taskflow: action group is invalid")
	ErrActionGroupFrozen  = errors.New("taskflow: action group is frozen")
	ErrActionIDExhausted  = errors.New("taskflow: action id exhausted")
	// ErrReentrantMutation 现在只由 MissionRunner 返回（任务启动 / 结束途中的重入）。
	// ActionRunner 自 B7 起把回调里的变更延后执行，不再返回它；保留导出名以兼容调用方。
	ErrReentrantMutation = errors.New("taskflow: reentrant mutation changed current action")
	// ErrDeferredQueueFull：一次最外层调用里，回调发起的待执行命令超过
	// ActionRunnerConfig.MaxDeferredCommands。被拒的 Start / Enqueue 不分配 ID。
	ErrDeferredQueueFull = errors.New("taskflow: deferred command queue is full")
	// ErrDeferredRunaway：回调互相触发，一次最外层调用执行的延后命令超过
	// ActionRunnerConfig.MaxDeferredSteps。剩余命令被丢弃，已交出 ID 的动作各收到一次
	// 取消状态的 OnEnded；截停期间回调里的新变更直接返回本错误。
	ErrDeferredRunaway = errors.New("taskflow: deferred commands kept triggering more commands")
)

const (
	defaultMaxDeferredCommands = 64
	defaultMaxDeferredSteps    = 1024
)

type ActionSnapshot struct {
	ID        int64
	MissionID int64
	Kind      ActionKind
	Group     ActionGroup
	Action    Action
}

type ActionRunnerHooks struct {
	// PopulateContext is the allocation-free context hook. Context remains for
	// compatibility and is copied into runner-owned callback-scoped storage.
	PopulateContext func(*ActionContext, time.Time)
	Context         func(time.Time) *ActionContext
	OnQueued        func(ActionSnapshot)
	OnTransition    func(ActionSnapshot, bool)
	OnEnded         func(ActionSnapshot, ActionReason)
	OnError         func(error)
}

type ActionRunnerConfig struct {
	Registry     *Registry
	GroupForKind func(ActionKind) (ActionGroup, bool)
	Hooks        ActionRunnerHooks
	// MaxDeferredCommands 限制一次最外层调用里同时待执行的延后命令数（回调发起的变更），
	// 0 取默认 64。超出时变更方法返回 ErrDeferredQueueFull。
	MaxDeferredCommands int
	// MaxDeferredSteps 限制一次最外层调用总共执行的延后命令数，防止回调无限互相触发；
	// 0 取默认 1024。超出时最外层调用返回 ErrDeferredRunaway。
	MaxDeferredSteps int
}

type actionEntry struct{ ActionSnapshot }
type actionGroupState struct {
	frozen bool
	cur    *actionEntry
	next   []*actionEntry
	// advancing 表示延后队列里已有一条“推进本组队列”的内部命令，避免重复登记。
	advancing bool
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
	// outer 表示命令属于最外层调用自己（它本身，或它直接引出的队列推进）：错误返回给
	// 最外层调用方。回调发起的命令及其引出的推进错误经 OnError 报告——发起它的回调早已
	// 拿到 nil 返回。
	outer bool
}

// ActionRunner is intentionally lock-free. The owning entity must serialize
// every call with its Entity mutex, including heartbeat-driven Tick calls.
//
// 回调里的变更延后执行（维护者决定 B7，方向 b）：动作的 Start / Tick / Cancel 和
// OnQueued / OnTransition / OnEnded 钩子都在某次最外层调用“执行中”运行；这期间对本 runner
// 的 Start、Enqueue、Tick、End、EndAll、ClearQueue、ClearMission、Recover 不立即生效，而是
// 按发起顺序进延后队列，最外层调用做完自己那一步后依次执行（执行中引出的回调再发起的
// 命令接着排在后面），全部执行完才返回。所以任一回调运行期间当前位、队列都不会被改，
// 不需要在每个回调点之后事后比对（U-0100 / NC-122 那组 ErrReentrantMutation 分支已删除）。
//
//   - 回调里的 Start / Enqueue 立即构建动作并返回已分配的 ID、错误为 nil（Deferring() 为真
//     即表示本次调用被延后）。拿到 ID 的动作一定有结论：启动后照常结束；延后执行时已无法
//     启动（组已冻结、被截停）也发一次失败 / 取消状态的 OnEnded，ai.TaskflowAction 这类按 ID
//     等结束的调用方不会挂住。
//   - 延后命令按“组 / 任务”而非发起时的快照解释，等价于回调返回后立刻按序调用。
//   - Freeze 只置标志、不触发回调，始终立即生效；Update 在回调里立即对当前动作调用 fn。
//     读方法（Current、QueueLength、Pending 等）在回调里读到的是尚未执行延后命令的状态。
//   - 队列有界（MaxDeferredCommands → ErrDeferredQueueFull），一次最外层调用执行的命令
//     总数有界（MaxDeferredSteps → ErrDeferredRunaway），回调互相触发不会无限执行。
type ActionRunner struct {
	registry     *Registry
	groupForKind func(ActionKind) (ActionGroup, bool)
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

func NewActionRunner(config ActionRunnerConfig) (*ActionRunner, error) {
	if config.Registry == nil || config.GroupForKind == nil {
		return nil, ErrActionGroupInvalid
	}
	runner := &ActionRunner{
		registry:         config.Registry,
		groupForKind:     config.GroupForKind,
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
		if unit := r.groups[group]; unit != nil && unit.frozen {
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

func (r *ActionRunner) Current(group ActionGroup) Action {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return nil
	}
	return unit.cur.Action
}

func (r *ActionRunner) CurrentSnapshot(group ActionGroup) (ActionSnapshot, bool) {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return ActionSnapshot{}, false
	}
	return unit.cur.ActionSnapshot, true
}

func (r *ActionRunner) QueueLength(group ActionGroup) int {
	unit := r.groups[group]
	if unit == nil {
		return 0
	}
	return len(unit.next)
}

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
// fn 里的变更在 fn 返回后执行。
func (r *ActionRunner) Update(group ActionGroup, fn func(Action) error) error {
	if fn == nil {
		return nil
	}
	unit := r.groups[group]
	if unit == nil || unit.cur == nil || unit.cur.Action == nil {
		return nil
	}
	if r.executing {
		return fn(unit.cur.Action)
	}
	r.executing = true
	err := fn(unit.cur.Action)
	return errors.Join(err, r.drain())
}

func (r *ActionRunner) Tick(group ActionGroup, now time.Time) error {
	return r.submit(runnerCommand{op: opTick, group: group, now: now})
}

func (r *ActionRunner) End(group ActionGroup, force bool, reason ActionReason) error {
	return r.submit(runnerCommand{op: opEnd, group: group, force: force, reason: reason})
}

// EndAll 结束每个组的当前动作并丢弃排队项。回调（如 OnEnded 里推进任务）随之发起的
// 变更在全部结束之后按序执行，所以 EndAll 返回时可能已有回调启动的新动作在运行（O-A1）。
func (r *ActionRunner) EndAll(force bool, reason ActionReason) error {
	return r.submit(runnerCommand{op: opEndAll, force: force, reason: reason})
}

// ClearQueue 丢弃全部排队动作；每个被丢弃的动作发一次 OnEnded（取消，RR-20261005-NC-121）。
func (r *ActionRunner) ClearQueue() {
	if err := r.submit(runnerCommand{op: opClearQueue}); err != nil {
		r.report(err)
	}
}

func (r *ActionRunner) ClearMission(missionID int64, cancel bool, reason ActionReason) error {
	if missionID == 0 {
		return nil
	}
	return r.submit(runnerCommand{op: opClearMission, missionID: missionID, force: cancel, reason: reason})
}

func (r *ActionRunner) Freeze(group ActionGroup) { r.group(group).frozen = true }
func (r *ActionRunner) Frozen(group ActionGroup) bool {
	unit := r.groups[group]
	return unit != nil && unit.frozen
}

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
	cmd.outer = true
	r.applyingOuter = true
	err := r.apply(cmd)
	return errors.Join(err, r.drain())
}

// drain 按序执行延后命令，然后结束本次最外层调用。命令执行中引出的回调再发起的命令排在
// 队尾，同样在这里执行；执行总数超过预算时截停（abortDeferred）。返回属于最外层调用的错误。
func (r *ActionRunner) drain() (err error) {
	defer func() {
		r.deferred = r.deferred[:0]
		r.head = 0
		r.executing = false
		r.applyingOuter = false
		r.runaway = false
	}()
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
			r.report(fmt.Errorf("taskflow: deferred %s: %w", cmd.op, applyErr))
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
		if unit.cur == nil && !unit.frozen {
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
		unit.frozen = false
		if unit.cur == nil {
			r.scheduleAdvance(unit, cmd.group, cmd.now)
		}
		return nil
	case opAdvance:
		return r.applyAdvance(cmd)
	default:
		return fmt.Errorf("taskflow: unknown runner command %d", cmd.op)
	}
}

// applyStart 启动一个已构建的动作，替换当前动作。延后的 Start 已把 ID 交出，执行时组被
// 冻结也要发 OnEnded 收尾。
func (r *ActionRunner) applyStart(cmd runnerCommand) error {
	unit := r.group(cmd.group)
	entry := cmd.entry
	if unit.frozen {
		if !cmd.outer {
			r.ended(entry, NewActionErrorReason(ErrActionGroupFrozen))
		}
		return ErrActionGroupFrozen
	}
	if unit.cur != nil {
		if err := r.finish(unit, true, NewActionReason("replaced by next action"), false); err != nil {
			return err
		}
	}
	return r.start(unit, entry, cmd.now)
}

func (r *ActionRunner) applyTick(group ActionGroup, now time.Time) error {
	unit := r.groups[group]
	if unit == nil || unit.frozen || unit.cur == nil || unit.cur.Action == nil {
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
	if unit.cur != nil || unit.frozen || len(unit.next) == 0 {
		return nil
	}
	next := unit.next[0]
	copy(unit.next, unit.next[1:])
	unit.next[len(unit.next)-1] = nil
	unit.next = unit.next[:len(unit.next)-1]
	err := r.start(unit, next, cmd.now)
	if err == nil {
		return nil
	}
	r.report(fmt.Errorf("taskflow: queued action %d start: %w", next.ID, err))
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
// 这个 ID 的结束），静默丢弃会让等待方永远等不到结果（RR-20261005-NC-121）。它们从未
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
			err = fmt.Errorf("taskflow: group resolver panic for kind %d: %v", kind, recovered)
		}
	}()
	group, ok = r.groupForKind(kind)
	return group, ok, nil
}

// start 把 entry 装到当前位：进入切换 → 动作 Start。Start 失败时调 Cancel、离开切换、清当前
// 位，并发一次失败状态的 OnEnded——进入切换的动作恰好一次离开、一次结束，直接 Start 与
// 排队 / 延后启动一致（O-A2）。回调里的变更都在延后队列，这期间当前位一直是 entry。
func (r *ActionRunner) start(unit *actionGroupState, entry *actionEntry, now time.Time) error {
	if unit == nil || entry == nil || entry.Action == nil {
		return ErrBuilderNil
	}
	unit.cur = entry
	r.transition(entry, true)
	err := r.callStart(entry.Action, now)
	if err == nil {
		return nil
	}
	cancelErr := r.callCancel(entry.Action, now, "start failed")
	unit.cur = nil
	r.transition(entry, false)
	err = errors.Join(err, cancelErr)
	r.ended(entry, NewActionErrorReason(err))
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
	if startNext && !unit.frozen {
		r.scheduleAdvance(unit, entry.Group, time.Time{})
	}
	return err
}

func (r *ActionRunner) group(group ActionGroup) *actionGroupState {
	unit := r.groups[group]
	if unit == nil {
		unit = &actionGroupState{}
		r.groups[group] = unit
	}
	return unit
}

func (r *ActionRunner) orderedGroups() []*actionGroupState {
	keys := make([]ActionGroup, 0, len(r.groups))
	for group := range r.groups {
		keys = append(keys, group)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	groups := make([]*actionGroupState, 0, len(keys))
	for _, group := range keys {
		groups = append(groups, r.groups[group])
	}
	return groups
}

func (r *ActionRunner) acquireContext(now time.Time) (ctx *ActionContext) {
	if now.IsZero() {
		now = time.Now()
	}
	ctx = r.contextPool.Get().(*ActionContext)
	*ctx = ActionContext{Now: now}
	if r.hooks.PopulateContext != nil || r.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action context hook panic: %v", recovered))
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

func (r *ActionRunner) callStart(action Action, now time.Time) error {
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
				r.report(fmt.Errorf("taskflow: action transition hook panic: %v", recovered))
			}
		}()
		r.hooks.OnTransition(entry.ActionSnapshot, active)
	}
}
func (r *ActionRunner) queued(entry *actionEntry) {
	if r.hooks.OnQueued != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action queued hook panic: %v", recovered))
			}
		}()
		r.hooks.OnQueued(entry.ActionSnapshot)
	}
}
func (r *ActionRunner) ended(entry *actionEntry, reason ActionReason) {
	if r.hooks.OnEnded != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action ended hook panic: %v", recovered))
			}
		}()
		r.hooks.OnEnded(entry.ActionSnapshot, reason)
	}
}
func (r *ActionRunner) report(err error) {
	if err != nil && r.hooks.OnError != nil {
		defer func() { _ = recover() }()
		r.hooks.OnError(err)
	}
}

func callActionStart(action Action, ctx *ActionContext) (err error) {
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
		*err = fmt.Errorf("taskflow: action %s panic: %v", operation, recovered)
	}
}
