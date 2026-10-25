package movement

import (
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
	// Period 是 tick 周期（必须不短于 heartbeat.Periods 的最短档，按档向下取）；0 取 DefaultMovePeriod。
	Period time.Duration
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
	// Period 同 MoveToParam.Period。
	Period time.Duration
}

// RegisterActions 把 MoveTo 与 Follow 两个动作以业务选定的种类注册进 actionflow 注册表；种类到组的映射在业务的
// ActionRunnerConfig.GroupForKind 里（通常放进同一个“移动”组，新的移动动作取代旧的）。
//
// 移动由动作驱动（参考 ssr）：动作所在组由实体的 actionflow.BeatRunner 按 Period 挂心跳；Start 在实体的 Move 上
// 设定这次移动并按 0 时长核对一次（已经到达 / 目标已不在时启动即完成，不挂心跳），之后每拍 Tick 调
// Move.Advance(ctx.Elapsed)，移动结束即动作结束——Arrived 为成功，Replaced 为取消，
// 其余（被阻挡、离开场景、目标消失、写位置被拒）为失败，原因是 StopReason。动作被取消（含被同组新动作取代）时
// 停掉自己那次移动。实体实现 MoveOwner。
func RegisterActions(registry *actionflow.Registry, moveTo, follow actionflow.ActionKind) error {
	if err := registry.RegisterAction(moveTo, func(param any) (actionflow.Action, error) {
		p, ok := param.(MoveToParam)
		if !ok {
			return nil, fmt.Errorf("movement: move-to action wants MoveToParam, got %T", param)
		}
		return &moveAction{kind: moveTo, moveTo: &p, period: p.Period}, nil
	}); err != nil {
		return err
	}
	return registry.RegisterAction(follow, func(param any) (actionflow.Action, error) {
		p, ok := param.(FollowParam)
		if !ok {
			return nil, fmt.Errorf("movement: follow action wants FollowParam, got %T", param)
		}
		return &moveAction{kind: follow, follow: &p, period: p.Period}, nil
	})
}

// moveAction 是 MoveTo / Follow 的共同实现：一次动作对应 Move 上的一次移动（token）。
type moveAction struct {
	kind   actionflow.ActionKind
	moveTo *MoveToParam
	follow *FollowParam
	period time.Duration

	move  *Move
	token uint64
}

func (a *moveAction) Kind() actionflow.ActionKind { return a.kind }

// TickInterval 是动作所在组的心跳周期（BeatRunner 按它挂心跳）。
func (a *moveAction) TickInterval() time.Duration {
	if a.period > 0 {
		return a.period
	}
	return DefaultMovePeriod
}

// Start 在实体的 Move 上设定这次移动，随即按 0 时长核对一次（Start 即完成）：已经在终点（路径上的点都是
// 当前格）、跟随的目标已在 Keep 以内且 StopWhenReached、目标已消失或不在本场景时，动作不进入运行，直接以 Advance
// 的结论结束，不占心跳。
func (a *moveAction) Start(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult, error) {
	owner, ok := ctx.Owner.(MoveOwner)
	if !ok || owner.MoveComponent() == nil {
		return false, actionflow.ActionResult{}, fmt.Errorf("movement: action owner %T has no move component", ctx.Owner)
	}
	a.move = owner.MoveComponent()
	var err error
	if a.moveTo != nil {
		a.token, err = a.move.MoveTo(a.moveTo.Path, a.moveTo.Speed)
	} else {
		a.token, err = a.move.Follow(a.follow.Target, a.follow.Speed, a.follow.Keep, a.follow.StopWhenReached)
	}
	if err != nil {
		return false, actionflow.ActionResult{}, err
	}
	done, result := actionResult(a.move.Advance(a.token, 0))
	return done, result, nil
}

// Tick 按这一拍的 Elapsed 推进移动；不是心跳 tick（Elapsed 为 0）时只核对移动是否还在。
func (a *moveAction) Tick(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult) {
	return actionResult(a.move.Advance(a.token, ctx.Elapsed))
}

// actionResult 把 Move 的结论换成动作结论：Arrived 成功，Replaced 取消，其余失败（原因是 StopReason）。
func actionResult(result Result) (bool, actionflow.ActionResult) {
	switch result.Reason {
	case 0:
		return false, actionflow.ActionResult{}
	case Arrived:
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusSuccess, Reason: Arrived.String()}
	case Replaced:
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusCanceled, Reason: Replaced.String()}
	default:
		reason := result.Reason.String()
		if result.Err != nil {
			reason += ": " + result.Err.Error()
		}
		return true, actionflow.ActionResult{Status: actionflow.ActionStatusFailed, Reason: reason}
	}
}

func (a *moveAction) Cancel(*actionflow.ActionContext, string) {
	if a.move != nil {
		a.move.Stop(a.token)
	}
}
