package ai

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

var (
	ErrStrategyRejected = errors.New("ai: current strategy rejects replacement")
	ErrStrategyInit     = errors.New("ai: strategy initialization failed")
	ErrReentrantSwitch  = errors.New("ai: reentrant strategy switch")
)

type ControllerHooks struct {
	Now        func() time.Time
	Context    func(time.Time) *Context
	EndActions func(coreflow.ActionReason)
	OnChanged  func(previous, current Strategy)
	OnError    func(error)
}

// Controller hosts one strategy and provides transactional replacement:
// a failing new Init leaves the previous strategy installed and active.
// Calls are externally serialized by the owning Entity mutex.
type Controller struct {
	hooks     ControllerHooks
	strategy  Strategy
	frozen    bool
	switching bool
}

func NewController(hooks ControllerHooks) *Controller { return &Controller{hooks: hooks} }

// SetStrategy 换上 next。顺序是：旧策略同意被替换 → next.Init（失败则 Stop next、
// 保留旧策略，这是“事务式替换”）→ EndActions 结束现有动作 → Stop 旧策略 → 发布。
// EndActions 在 Init 成功之后才跑，通常接 ActionList.EndAllAction，所以 Init 里就发起的
// 动作也会被一并结束，而且切换期间的结束通知不送达策略：策略应在第一次 Tick 里发起
// 动作，不要在 Init 里发起（BehaviorStrategy.Init 只重置树）。
func (c *Controller) SetStrategy(next Strategy) error {
	if c == nil {
		return ErrStrategyInit
	}
	if c.switching {
		return ErrReentrantSwitch
	}
	canStop, err := callStrategyCanStop(c.strategy, next)
	if err != nil {
		return err
	}
	if c.strategy != nil && !canStop {
		return ErrStrategyRejected
	}
	if sameStrategy(next, c.strategy) {
		return nil
	}
	c.switching = true
	defer func() { c.switching = false }()
	ctx := c.context(c.now())
	if next != nil {
		if err := callStrategyInit(next, ctx); err != nil {
			stopErr := callStrategyStop(next, ctx, "initialization failed")
			return errors.Join(fmt.Errorf("%w: %s: %v", ErrStrategyInit, strategyName(next), err), stopErr)
		}
	}
	previous := c.strategy
	if previous != nil && c.hooks.EndActions != nil {
		if err := c.endActions(coreflow.NewActionReason("strategy replaced")); err != nil {
			c.report(err)
		}
	}
	if err := callStrategyStop(previous, ctx, "strategy replaced"); err != nil {
		c.report(err)
	}
	c.strategy = next
	c.changed(previous, next)
	return nil
}

func (c *Controller) Strategy() Strategy {
	if c == nil {
		return nil
	}
	return c.strategy
}
func (c *Controller) Freeze() {
	if c != nil {
		c.frozen = true
	}
}
func (c *Controller) Recover() {
	if c != nil {
		c.frozen = false
	}
}
func (c *Controller) Frozen() bool { return c != nil && c.frozen }

func (c *Controller) Tick(now time.Time) {
	if !c.ready() {
		return
	}
	if now.IsZero() {
		now = c.now()
	}
	if err := callStrategyTick(c.strategy, c.context(now), now); err != nil {
		c.report(err)
	}
}

// OnActionEnd 把一次动作结束交给当前策略。冻结期间照常送达（见 notifiable）。
func (c *Controller) OnActionEnd(id int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	if !c.notifiable() {
		return
	}
	if err := callStrategyActionEnd(c.strategy, c.context(c.now()), id, kind, reason); err != nil {
		c.report(err)
	}
}

// OnMissionEnd 把一次任务结束交给当前策略。冻结期间照常送达（见 notifiable）。
func (c *Controller) OnMissionEnd(mission coreflow.Mission, reason coreflow.ActionReason) {
	if !c.notifiable() {
		return
	}
	if err := callStrategyMissionEnd(c.strategy, c.context(c.now()), mission, reason); err != nil {
		c.report(err)
	}
}
func (c *Controller) Shutdown(reason string) {
	if c == nil {
		return
	}
	c.switching = true
	defer func() { c.switching = false }()
	previous := c.strategy
	c.strategy = nil
	if err := callStrategyStop(previous, c.context(c.now()), reason); err != nil {
		c.report(err)
	}
	if previous != nil {
		c.changed(previous, nil)
	}
}

// ready 判断能否驱动策略做决策（Tick）：冻结、切换中或没有策略时都不能。
func (c *Controller) ready() bool { return c.notifiable() && !c.frozen }

// notifiable 判断结束通知能否交给当前策略。Freeze 只暂停决策，不暂停记账：动作在冻结
// 期间照样会结束（被业务 EndAllAction、被替换、被取消），策略——尤其 BehaviorStrategy 里
// 等某个动作 ID 的 TaskflowAction 叶子——只能从这条通知得知结果；丢掉它，Recover 之后
// 树会永远停在 Running（RR-20261005-NC-120）。切换策略期间仍然丢弃：那时结束的是旧
// 策略的动作（EndActions），新策略不该收到。策略若在 OnActionEnd 里直接发起新动作，
// 冻结期间也会被调用，需要自己判断时机（BehaviorStrategy 只缓冲到下一次 Tick）。
func (c *Controller) notifiable() bool { return c != nil && !c.switching && c.strategy != nil }
func (c *Controller) now() time.Time {
	if c.hooks.Now != nil {
		var now time.Time
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					c.report(fmt.Errorf("ai: now hook panic: %v", recovered))
				}
			}()
			now = c.hooks.Now()
		}()
		if !now.IsZero() {
			return now
		}
	}
	return time.Now()
}
func (c *Controller) context(now time.Time) (ctx *Context) {
	if now.IsZero() {
		now = c.now()
	}
	ctx = &Context{Now: now}
	if c.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.report(fmt.Errorf("ai: context hook panic: %v", recovered))
				ctx = &Context{Now: now}
			}
		}()
		if candidate := c.hooks.Context(now); candidate != nil {
			ctx = candidate
		}
	}
	return ctx
}
func (c *Controller) report(err error) {
	if err != nil && c.hooks.OnError != nil {
		defer func() { _ = recover() }()
		c.hooks.OnError(err)
	}
}

func (c *Controller) endActions(reason coreflow.ActionReason) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("ai: end-actions hook panic: %v", recovered)
		}
	}()
	c.hooks.EndActions(reason)
	return nil
}

func (c *Controller) changed(previous, current Strategy) {
	if c.hooks.OnChanged == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			c.report(fmt.Errorf("ai: changed hook panic: %v", recovered))
		}
	}()
	c.hooks.OnChanged(previous, current)
}

func sameStrategy(left, right Strategy) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	lv, rv := reflect.ValueOf(left), reflect.ValueOf(right)
	return lv.Type() == rv.Type() && lv.Type().Comparable() && lv.Interface() == rv.Interface()
}

func strategyName(strategy Strategy) (name string) {
	if strategy == nil {
		return "<nil>"
	}
	defer func() {
		if recover() != nil {
			name = "<panic>"
		}
	}()
	return strategy.Name()
}

func callStrategyInit(strategy Strategy, ctx *Context) (err error) {
	if strategy == nil {
		return nil
	}
	defer recoverStrategyPanic("init", &err)
	return strategy.Init(ctx)
}
func callStrategyCanStop(strategy, next Strategy) (allowed bool, err error) {
	if strategy == nil {
		return true, nil
	}
	defer recoverStrategyPanic("can stop", &err)
	return strategy.CanStopByNext(next), nil
}
func callStrategyTick(strategy Strategy, ctx *Context, now time.Time) (err error) {
	defer recoverStrategyPanic("tick", &err)
	strategy.Tick(ctx, now)
	return nil
}
func callStrategyActionEnd(strategy Strategy, ctx *Context, id int64, kind coreflow.ActionKind, reason coreflow.ActionReason) (err error) {
	defer recoverStrategyPanic("action end", &err)
	strategy.OnActionEnd(ctx, id, kind, reason)
	return nil
}
func callStrategyMissionEnd(strategy Strategy, ctx *Context, mission coreflow.Mission, reason coreflow.ActionReason) (err error) {
	defer recoverStrategyPanic("mission end", &err)
	strategy.OnMissionEnd(ctx, mission, reason)
	return nil
}
func callStrategyStop(strategy Strategy, ctx *Context, reason string) (err error) {
	if strategy == nil {
		return nil
	}
	stoppable, ok := strategy.(StoppableStrategy)
	if !ok {
		return nil
	}
	defer recoverStrategyPanic("stop", &err)
	stoppable.Stop(ctx, reason)
	return nil
}
func recoverStrategyPanic(operation string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("ai: strategy %s panic: %v", operation, recovered)
	}
}
