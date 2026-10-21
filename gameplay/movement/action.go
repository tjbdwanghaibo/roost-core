package movement

import (
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// MoveToParam 是 MoveTo 动作的参数。
type MoveToParam struct {
	// Path 是途经点（不含当前位置），最后一个是终点。寻路在发起动作之前做。
	Path []spatial.Point
	// Speed 是每秒格数。
	Speed float64
}

// FollowParam 是 Follow 动作的参数。
type FollowParam struct {
	Target int64
	Speed  float64
	// Keep 是与目标保持的距离（格）。
	Keep int64
	// StopWhenReached 为 true 时走到 Keep 以内即成功结束（“走到目标身边”）；否则一直跟随，直到目标消失、离开场景
	// 或动作被取消 / 取代。
	StopWhenReached bool
}

// RegisterActions 把 MoveTo 与 Follow 两个动作以业务选定的种类注册进 actionflow 注册表；种类到组的映射在业务的
// ActionRunnerConfig.GroupForKind 里（通常放进同一个“移动”组，新的移动动作取代旧的）。
//
// 两个动作都由事件推进（TickInterval 为 0）：Start 让实体的 Move 组件开始移动，Move 自己的心跳推进位置；移动结束时
// Move 回调，动作经 ActionContext.Waker 让 runner 马上 tick 它所在的组，Tick 交出结果——
// Arrived 为成功，Replaced 为取消，其余（被阻挡、离开场景、目标消失、写位置被拒）为失败，原因是 StopReason。
// 动作被取消（含被同组新动作取代）时停掉自己那次移动。所以需要 actionflow.BeatRunner（提供 Waker），实体实现
// MoveOwner。
func RegisterActions(registry *actionflow.Registry, moveTo, follow actionflow.ActionKind) error {
	if err := registry.RegisterAction(moveTo, func(param any) (actionflow.Action, error) {
		p, ok := param.(MoveToParam)
		if !ok {
			return nil, fmt.Errorf("movement: move-to action wants MoveToParam, got %T", param)
		}
		return &moveAction{kind: moveTo, moveTo: &p}, nil
	}); err != nil {
		return err
	}
	return registry.RegisterAction(follow, func(param any) (actionflow.Action, error) {
		p, ok := param.(FollowParam)
		if !ok {
			return nil, fmt.Errorf("movement: follow action wants FollowParam, got %T", param)
		}
		return &moveAction{kind: follow, follow: &p}, nil
	})
}

// moveAction 是 MoveTo / Follow 的共同实现：一次动作对应 Move 上的一次移动（token）。
type moveAction struct {
	kind   actionflow.ActionKind
	moveTo *MoveToParam
	follow *FollowParam

	move   *Move
	token  uint64
	waker  actionflow.Waker
	done   bool
	result Result
}

func (a *moveAction) Kind() actionflow.ActionKind { return a.kind }

// TickInterval 为 0：由 Move 的心跳推进，runner 不为它挂心跳。
func (a *moveAction) TickInterval() time.Duration { return 0 }

func (a *moveAction) Start(ctx *actionflow.ActionContext) error {
	owner, ok := ctx.Owner.(MoveOwner)
	if !ok || owner.MoveComponent() == nil {
		return fmt.Errorf("movement: action owner %T has no move component", ctx.Owner)
	}
	if ctx.Waker == nil {
		return errors.New("movement: move actions need an actionflow.BeatRunner (no waker in the action context)")
	}
	a.move, a.waker = owner.MoveComponent(), ctx.Waker
	var err error
	if a.moveTo != nil {
		a.token, err = a.move.MoveTo(a.moveTo.Path, a.moveTo.Speed, a.ended)
	} else {
		a.token, err = a.move.Follow(a.follow.Target, a.follow.Speed, a.follow.Keep, a.follow.StopWhenReached, a.ended)
	}
	return err
}

// ended 是 Move 的 Done：记下结论，让 runner 马上 tick 这个动作的组（在 Move 的心跳回调里，实体锁与事务内）。
func (a *moveAction) ended(token uint64, result Result) {
	if token != a.token || a.done {
		return
	}
	a.done, a.result = true, result
	_ = a.waker.Wake(a) // 动作已不是当前（被取代途中）时 Wake 什么也不做
}

func (a *moveAction) Tick(*actionflow.ActionContext) (bool, actionflow.ActionResult) {
	if !a.done {
		return false, actionflow.ActionResult{}
	}
	switch a.result.Reason {
	case Arrived:
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusSuccess, Reason: Arrived.String()}
	case Replaced:
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusCanceled, Reason: Replaced.String()}
	default:
		reason := a.result.Reason.String()
		if a.result.Err != nil {
			reason += ": " + a.result.Err.Error()
		}
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusFailed, Reason: reason}
	}
}

func (a *moveAction) Cancel(*actionflow.ActionContext, string) {
	if a.move != nil && !a.done {
		a.move.Stop(a.token)
	}
}
