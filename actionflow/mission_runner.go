package actionflow

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"math"
	"time"
)

var (
	ErrMissionRunning     = errors.New("taskflow: mission already running")
	ErrMissionNotRunning  = errors.New("taskflow: mission not running")
	ErrMissionIDExhausted = errors.New("taskflow: mission id exhausted")
)

type MissionRunnerHooks struct {
	Now          func() time.Time
	Context      func(time.Time) *MissionContext
	ClearActions func(int64, ActionReason) error
	OnState      func(bool)
	OnChanged    func(MissionInfo)
	OnEnded      func(Mission, ActionReason)
	OnError      func(error)
}

type MissionRunnerConfig struct {
	Registry    *Registry
	DefaultKind MissionKind
	Hooks       MissionRunnerHooks
	// MaxDeferredCommands 限制一次最外层调用里同时待执行的延后命令数（回调发起的变更），
	// 0 取默认 64。超出时变更方法返回（或经 OnError 报告）ErrDeferredQueueFull。
	MaxDeferredCommands int
	// MaxDeferredSteps 限制一次最外层调用总共执行的延后命令数，防止回调无限互相触发；
	// 0 取默认 1024。超出时剩余命令丢弃，最外层调用返回 / 报告 ErrDeferredRunaway。
	MaxDeferredSteps int
}

// missionOp 是 MissionRunner 的一种变更。公开的变更方法都先组成一条命令交给 submit：
// 不在回调里就立即执行，在回调里就进延后队列。
type missionOp uint8

const (
	missionOpStart missionOp = iota + 1
	missionOpEnd
	missionOpTick
	missionOpActionEnd
)

func (op missionOp) String() string {
	switch op {
	case missionOpStart:
		return "start mission"
	case missionOpEnd:
		return "end mission"
	case missionOpTick:
		return "mission tick"
	case missionOpActionEnd:
		return "mission action end"
	default:
		return "unknown"
	}
}

type missionCommand struct {
	op       missionOp
	kind     MissionKind
	param    any
	reason   ActionReason
	now      time.Time
	actionID int64
	action   ActionKind
}

// MissionRunner 与 ActionRunner 一样刻意无锁，由持实体锁的一方串行调用。
//
// 回调里的变更延后执行（维护者 2026-10-06 第五轮决定，与 ActionRunner / B7 一致）：任务的
// Start / Tick / OnActionEnd / End / CanReplaceBy、任务构建器，以及 Now / Context / OnState /
// OnChanged / OnEnded / ClearActions / OnError 钩子都在某次最外层调用“执行中”运行；这期间对
// 本 runner 的 StartMission、CancelMission、EndCurMission、Tick、OnActionEnd 不立即生效，
// 而是按发起顺序进延后队列，最外层调用做完自己那一步后依次执行（执行中引出的回调再发起的
// 命令接着排在后面），全部执行完才返回。判定只在 submit 一处；任一回调运行期间当前任务都
// 不会被换掉，之前的 starting / ending 守卫与 ErrReentrantMutation 分支已删除。
//
//   - 回调里的 StartMission / CancelMission 返回 nil（Deferring() 为真即表示被延后）；参数
//     校验（任务类型为 0、CancelMission 时没有任务）仍立即返回错误。延后命令执行时的错误
//     （ErrMissionRunning、构建失败、Start 失败等）发起方已拿不到，经 OnError 报告
//     （`taskflow: deferred start mission: ...`）；最外层调用自己的错误照旧返回。
//   - 延后命令按“执行时的当前任务”解释，等价于回调返回后立刻按序调用：回调里先
//     StartMission 再 EndCurMission，结束的是新任务。
//   - 读方法（CurMission、InMission、MissionInfo 等）在回调里读到的是尚未执行延后命令的状态。
//   - 队列有界（MaxDeferredCommands → ErrDeferredQueueFull），一次最外层调用执行的命令总数
//     有界（MaxDeferredSteps → ErrDeferredRunaway），回调互相触发不会无限执行；之后照常可用。
//   - 全部回调 panic 都恢复成错误；执行状态在 submit 的 defer 里复位，不会卡在“延后中”。
//   - Reset 不是变更命令，立即生效，只用于复用对象，不要在回调里调用。
type MissionRunner struct {
	registry    *Registry
	defaultKind MissionKind
	hooks       MissionRunnerHooks
	nextID      int64
	cur         Mission
	lastInfo    MissionInfo
	lastEndTime time.Time

	maxDeferred      int
	maxDeferredSteps int
	// executing 为真表示某次最外层调用正在执行（含它触发的全部回调），是“回调期间”的唯一
	// 判定点。
	executing bool
	// runaway 为真表示本次最外层调用已超出执行预算，正在丢弃剩余命令。
	runaway  bool
	deferred []missionCommand
	head     int
}

func NewMissionRunner(config MissionRunnerConfig) (*MissionRunner, error) {
	if config.Registry == nil {
		return nil, ErrMissionBuilderNotFound
	}
	runner := &MissionRunner{
		registry:         config.Registry,
		defaultKind:      config.DefaultKind,
		hooks:            config.Hooks,
		lastInfo:         MissionInfo{CurrentStep: -1},
		maxDeferred:      config.MaxDeferredCommands,
		maxDeferredSteps: config.MaxDeferredSteps,
	}
	if runner.maxDeferred <= 0 {
		runner.maxDeferred = defaultMaxDeferredCommands
	}
	if runner.maxDeferredSteps <= 0 {
		runner.maxDeferredSteps = defaultMaxDeferredSteps
	}
	return runner, nil
}

// Deferring 报告此刻对本 runner 的变更调用是否会被延后（正处在某次调用的回调里）。
func (r *MissionRunner) Deferring() bool { return r != nil && r.executing }

// StartMission 启动一个任务；已有任务时先问它 CanReplaceBy，允许才结束旧任务、启动新任务。
// 在回调里调用时延后执行并返回 nil，执行时的错误经 OnError 报告。
func (r *MissionRunner) StartMission(kind MissionKind, param any) error {
	if r == nil {
		return ErrMissionBuilderNotFound
	}
	if kind == 0 {
		kind = r.defaultKind
	}
	if kind == 0 {
		return ErrKindInvalid
	}
	return r.submit(missionCommand{op: missionOpStart, kind: kind, param: param})
}

// CancelMission 以取消原因结束当前任务；没有任务时返回 ErrMissionNotRunning。在回调里调用
// 时延后执行并返回 nil。
func (r *MissionRunner) CancelMission(reason string) error {
	if r == nil || r.cur == nil {
		return ErrMissionNotRunning
	}
	if reason == "" {
		reason = "canceled"
	}
	return r.submit(missionCommand{op: missionOpEnd, reason: NewActionReason(reason)})
}

func (r *MissionRunner) InMission() bool {
	return r != nil && r.cur != nil && r.cur.Status() == MissionStatusRunning
}
func (r *MissionRunner) CurrentAction() ActionKind {
	if r == nil || r.cur == nil {
		return 0
	}
	return r.cur.MissionInfo().CurrentAction
}
func (r *MissionRunner) CurMission() Mission {
	if r == nil {
		return nil
	}
	return r.cur
}
func (r *MissionRunner) CurrentMissionID() int64 {
	if r == nil || r.cur == nil {
		return 0
	}
	return r.cur.ID()
}
func (r *MissionRunner) MissionInfo() MissionInfo {
	if r == nil {
		return MissionInfo{CurrentStep: -1}
	}
	if r.cur != nil {
		return r.cur.MissionInfo()
	}
	return r.lastInfo
}

// OnActionEnd 把一次动作结束交给当前任务（接线：ActionRunner 的 OnEnded）。任务结束途中
// 送来的结束在任务结束后执行，那时已没有任务，被忽略。
func (r *MissionRunner) OnActionEnd(actionID int64, kind ActionKind, reason ActionReason) {
	if r == nil {
		return
	}
	r.submitWithoutResult(missionCommand{op: missionOpActionEnd, actionID: actionID, action: kind, reason: reason})
}

// EndCurMission 结束当前任务（没有任务时什么也不做）。
func (r *MissionRunner) EndCurMission(reason ActionReason) {
	if r == nil {
		return
	}
	r.submitWithoutResult(missionCommand{op: missionOpEnd, reason: reason})
}

func (r *MissionRunner) Tick(now time.Time) {
	if r == nil {
		return
	}
	r.submitWithoutResult(missionCommand{op: missionOpTick, now: now})
}

// Reset 立即清空当前任务与最近信息，不结束任务、不调用回调；只用于复用对象。
func (r *MissionRunner) Reset() {
	r.cur = nil
	r.lastInfo = MissionInfo{CurrentStep: -1}
}

// submit 是变更的唯一入口：回调期间（executing）进延后队列，否则作为最外层调用立即执行，
// 并在返回前执行完全部延后命令。执行状态在 defer 里复位：即使有未预料的 panic 穿出，
// runner 也不会停在“延后中”、之后的变更全被排进一个没人执行的队列（NC-242 的教训）。
func (r *MissionRunner) submit(cmd missionCommand) error {
	if r.executing {
		if err := r.deferrable(); err != nil {
			return err
		}
		r.deferred = append(r.deferred, cmd)
		return nil
	}
	r.executing = true
	defer r.endExecution()
	err := r.apply(cmd)
	return errors.Join(err, r.drain())
}

// submitWithoutResult 用于没有返回值的变更（EndCurMission、Tick、OnActionEnd）：回调里被拒
// （队列满、已截停）时经 OnError 报告；最外层调用的截停已由 abortDeferred 报告，执行本身的
// 错误在各步骤里报告，不重复。
func (r *MissionRunner) submitWithoutResult(cmd missionCommand) {
	inCallback := r.executing
	if err := r.submit(cmd); err != nil && inCallback {
		r.report(fmt.Errorf("taskflow: %s: %w", cmd.op, err))
	}
}

func (r *MissionRunner) deferrable() error {
	if r.runaway {
		return ErrDeferredRunaway
	}
	if len(r.deferred)-r.head >= r.maxDeferred {
		return ErrDeferredQueueFull
	}
	return nil
}

// drain 按序执行延后命令。命令执行中引出的回调再发起的命令排在队尾，同样在这里执行；
// 执行总数超过预算时截停。延后命令的错误发起方已拿不到，一律经 OnError 报告。
func (r *MissionRunner) drain() error {
	steps := 0
	for r.head < len(r.deferred) {
		if steps >= r.maxDeferredSteps {
			return r.abortDeferred()
		}
		steps++
		cmd := r.deferred[r.head]
		r.deferred[r.head] = missionCommand{}
		r.head++
		if err := r.apply(cmd); err != nil {
			r.report(fmt.Errorf("taskflow: deferred %s: %w", cmd.op, err))
		}
	}
	return nil
}

// abortDeferred 丢弃预算之外的命令并报告。任务命令没有预先交出的 ID，丢弃不需要补发结束；
// runaway 置位期间（含 OnError 回调里）新的变更直接返回 ErrDeferredRunaway。
func (r *MissionRunner) abortDeferred() error {
	r.runaway = true
	dropped := len(r.deferred) - r.head
	clear(r.deferred[r.head:])
	r.head = len(r.deferred)
	err := fmt.Errorf("%w: budget %d exhausted, %d command(s) dropped", ErrDeferredRunaway, r.maxDeferredSteps, dropped)
	r.report(err)
	return err
}

func (r *MissionRunner) endExecution() {
	clear(r.deferred)
	r.deferred = r.deferred[:0]
	r.head = 0
	r.executing = false
	r.runaway = false
}

// apply 执行一条命令。执行期间 executing 恒为真，回调发起的变更只会进延后队列，所以每一步
// 之后当前任务都还是这里刚设置的值，不需要事后比对。
func (r *MissionRunner) apply(cmd missionCommand) error {
	switch cmd.op {
	case missionOpStart:
		return r.applyStart(cmd.kind, cmd.param)
	case missionOpEnd:
		r.applyEnd(cmd.reason)
		return nil
	case missionOpTick:
		r.applyTick(cmd.now)
		return nil
	case missionOpActionEnd:
		r.applyActionEnd(cmd.actionID, cmd.action, cmd.reason)
		return nil
	default:
		return fmt.Errorf("taskflow: unknown mission command %d", cmd.op)
	}
}

func (r *MissionRunner) applyStart(kind MissionKind, param any) error {
	if r.cur != nil {
		canReplace, err := callMissionCanReplace(r.cur, kind, param)
		if err != nil {
			return err
		}
		if !canReplace {
			return ErrMissionRunning
		}
	}
	if r.nextID == math.MaxInt64 {
		return ErrMissionIDExhausted
	}
	next, err := r.registry.BuildMission(kind)
	if err != nil {
		return err
	}
	if r.cur != nil {
		r.applyEnd(NewActionReason("replaced by next mission"))
	}
	r.nextID++
	if err := callMissionSetRuntime(next, r.nextID, r); err != nil {
		return err
	}
	r.cur = next
	r.state(true)
	r.changed(next.MissionInfo())
	ctx := r.context(r.now())
	if startErr := callMissionStart(next, ctx, param); startErr != nil {
		reason := NewActionErrorReason(startErr)
		endErr := callMissionEnd(next, ctx, reason)
		r.lastInfo = next.MissionInfo()
		r.cur = nil
		r.state(false)
		clearErr := r.clearActions(next.ID(), reason)
		r.changed(r.lastInfo)
		r.ended(next, reason)
		return errors.Join(startErr, endErr, clearErr)
	}
	r.update()
	return nil
}

func (r *MissionRunner) applyActionEnd(actionID int64, kind ActionKind, reason ActionReason) {
	if r.cur == nil {
		return
	}
	if err := callMissionActionEnd(r.cur, r.context(r.now()), actionID, kind, reason); err != nil {
		r.report(err)
		r.applyEnd(NewActionErrorReason(err))
		return
	}
	r.update()
}

func (r *MissionRunner) applyEnd(reason ActionReason) {
	if r.cur == nil {
		return
	}
	cur := r.cur
	ctx := r.context(r.now())
	if err := callMissionEnd(cur, ctx, reason); err != nil {
		r.report(err)
	}
	r.lastInfo = cur.MissionInfo()
	r.cur = nil
	r.lastEndTime = r.now()
	r.state(false)
	if err := r.clearActions(cur.ID(), reason); err != nil {
		r.report(err)
	}
	r.changed(r.lastInfo)
	r.ended(cur, reason)
}

func (r *MissionRunner) applyTick(now time.Time) {
	if r.cur == nil {
		return
	}
	if now.IsZero() {
		now = r.now()
	}
	if err := callMissionTick(r.cur, r.context(now), now); err != nil {
		r.report(err)
		r.applyEnd(NewActionErrorReason(err))
		return
	}
	r.update()
}

func (r *MissionRunner) update() {
	if r.cur == nil {
		return
	}
	r.lastInfo = r.cur.MissionInfo()
	r.changed(r.lastInfo)
}
func (r *MissionRunner) now() time.Time {
	if r.hooks.Now != nil {
		var now time.Time
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					r.report(fmt.Errorf("taskflow: mission now hook panic: %v", recovered))
				}
			}()
			now = r.hooks.Now()
		}()
		if !now.IsZero() {
			return now
		}
	}
	return clock.Now() // 行为流的游戏时间是业务时钟（D-L3）
}
func (r *MissionRunner) context(now time.Time) (ctx *MissionContext) {
	if now.IsZero() {
		now = r.now()
	}
	ctx = &MissionContext{Manager: r, Now: now}
	if r.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: mission context hook panic: %v", recovered))
				ctx = &MissionContext{Manager: r, Now: now}
			}
		}()
		if candidate := r.hooks.Context(now); candidate != nil {
			candidate.Manager = r
			ctx = candidate
		}
	}
	return ctx
}
func (r *MissionRunner) state(active bool) {
	if r.hooks.OnState != nil {
		defer r.recoverHook("state")
		r.hooks.OnState(active)
	}
}
func (r *MissionRunner) changed(info MissionInfo) {
	if r.hooks.OnChanged != nil {
		defer r.recoverHook("changed")
		r.hooks.OnChanged(info)
	}
}
func (r *MissionRunner) ended(mission Mission, reason ActionReason) {
	if r.hooks.OnEnded != nil {
		defer r.recoverHook("ended")
		r.hooks.OnEnded(mission, reason)
	}
}
func (r *MissionRunner) report(err error) {
	if err != nil && r.hooks.OnError != nil {
		defer func() { _ = recover() }()
		r.hooks.OnError(err)
	}
}

func (r *MissionRunner) clearActions(missionID int64, reason ActionReason) (err error) {
	if r.hooks.ClearActions == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("taskflow: mission clear-actions hook panic: %v", recovered)
		}
	}()
	return r.hooks.ClearActions(missionID, reason)
}

func (r *MissionRunner) recoverHook(name string) {
	if recovered := recover(); recovered != nil {
		r.report(fmt.Errorf("taskflow: mission %s hook panic: %v", name, recovered))
	}
}

func callMissionStart(m Mission, ctx *MissionContext, param any) (err error) {
	defer recoverMissionPanic("start", &err)
	return m.Start(ctx, param)
}
func callMissionSetRuntime(m Mission, id int64, manager MissionManager) (err error) {
	setter, ok := m.(MissionRuntimeSetter)
	if !ok {
		return nil
	}
	defer recoverMissionPanic("set runtime", &err)
	setter.SetRuntime(id, manager)
	return nil
}
func callMissionCanReplace(m Mission, kind MissionKind, param any) (allowed bool, err error) {
	defer recoverMissionPanic("can replace", &err)
	return m.CanReplaceBy(kind, param), nil
}
func callMissionTick(m Mission, ctx *MissionContext, now time.Time) (err error) {
	defer recoverMissionPanic("tick", &err)
	m.Tick(ctx, now)
	return nil
}
func callMissionActionEnd(m Mission, ctx *MissionContext, id int64, kind ActionKind, reason ActionReason) (err error) {
	defer recoverMissionPanic("action end", &err)
	m.OnActionEnd(ctx, id, kind, reason)
	return nil
}
func callMissionEnd(m Mission, ctx *MissionContext, reason ActionReason) (err error) {
	defer recoverMissionPanic("end", &err)
	m.End(ctx, reason)
	return nil
}
func recoverMissionPanic(operation string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("taskflow: mission %s panic: %v", operation, recovered)
	}
}
