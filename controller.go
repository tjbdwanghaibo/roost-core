package ai

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	coreai "github.com/tjbdwanghaibo/cube-core/ai"
	coreflow "github.com/tjbdwanghaibo/cube-core/taskflow"
)

var (
	ErrStrategyRejected = errors.New("ai: current strategy rejects replacement")
	ErrStrategyInit     = errors.New("ai: strategy initialization failed")
	ErrReentrantSwitch  = errors.New("ai: reentrant strategy switch")
)

type ControllerHooks struct {
	Now        func() time.Time
	Context    func(time.Time) *coreai.Context
	EndActions func(coreflow.ActionReason)
	OnChanged  func(previous, current coreai.Strategy)
	OnError    func(error)
}

// Controller hosts one strategy and provides transactional replacement:
// a failing new Init leaves the previous strategy installed and active.
// Calls are externally serialized by the owning Entity mutex.
type Controller struct {
	hooks     ControllerHooks
	strategy  coreai.Strategy
	frozen    bool
	switching bool
}

func NewController(hooks ControllerHooks) *Controller { return &Controller{hooks: hooks} }

func (c *Controller) SetStrategy(next coreai.Strategy) error {
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

func (c *Controller) Strategy() coreai.Strategy {
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
func (c *Controller) OnActionEnd(id int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	if !c.ready() {
		return
	}
	if err := callStrategyActionEnd(c.strategy, c.context(c.now()), id, kind, reason); err != nil {
		c.report(err)
	}
}
func (c *Controller) OnMissionEnd(mission coreflow.Mission, reason coreflow.ActionReason) {
	if !c.ready() {
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
func (c *Controller) ready() bool { return c != nil && !c.frozen && !c.switching && c.strategy != nil }
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
func (c *Controller) context(now time.Time) (ctx *coreai.Context) {
	if now.IsZero() {
		now = c.now()
	}
	ctx = &coreai.Context{Now: now}
	if c.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.report(fmt.Errorf("ai: context hook panic: %v", recovered))
				ctx = &coreai.Context{Now: now}
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

func (c *Controller) changed(previous, current coreai.Strategy) {
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

func sameStrategy(left, right coreai.Strategy) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	lv, rv := reflect.ValueOf(left), reflect.ValueOf(right)
	return lv.Type() == rv.Type() && lv.Type().Comparable() && lv.Interface() == rv.Interface()
}

func strategyName(strategy coreai.Strategy) (name string) {
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

func callStrategyInit(strategy coreai.Strategy, ctx *coreai.Context) (err error) {
	if strategy == nil {
		return nil
	}
	defer recoverStrategyPanic("init", &err)
	return strategy.Init(ctx)
}
func callStrategyCanStop(strategy, next coreai.Strategy) (allowed bool, err error) {
	if strategy == nil {
		return true, nil
	}
	defer recoverStrategyPanic("can stop", &err)
	return strategy.CanStopByNext(next), nil
}
func callStrategyTick(strategy coreai.Strategy, ctx *coreai.Context, now time.Time) (err error) {
	defer recoverStrategyPanic("tick", &err)
	strategy.Tick(ctx, now)
	return nil
}
func callStrategyActionEnd(strategy coreai.Strategy, ctx *coreai.Context, id int64, kind coreflow.ActionKind, reason coreflow.ActionReason) (err error) {
	defer recoverStrategyPanic("action end", &err)
	strategy.OnActionEnd(ctx, id, kind, reason)
	return nil
}
func callStrategyMissionEnd(strategy coreai.Strategy, ctx *coreai.Context, mission coreflow.Mission, reason coreflow.ActionReason) (err error) {
	defer recoverStrategyPanic("mission end", &err)
	strategy.OnMissionEnd(ctx, mission, reason)
	return nil
}
func callStrategyStop(strategy coreai.Strategy, ctx *coreai.Context, reason string) (err error) {
	if strategy == nil {
		return nil
	}
	stoppable, ok := strategy.(coreai.StoppableStrategy)
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
