// Package ai 定义感知实体的 AI 策略边界。调度与具体决策算法由适配层提供。
package ai

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
)

// Context 是策略回调收到的上下文：拥有者实体、动作列表与当前时间。
type Context struct {
	Owner      entity.IThreadSafeEntity
	ActionList actionflow.ActionList
	Now        time.Time
}

// Strategy 是一个 AI 决策策略，由 Controller 调用。
//
// Init 里不要发起动作：SetStrategy 先 Init 新策略、成功后才 EndActions 结束现有动作（“Init
// 失败保留旧策略”的事务式顺序），Init 里发起的动作会被一并结束。在第一次 Tick 里发起
// （这个顺序是有意保持的）。
type Strategy interface {
	Name() string
	Init(ctx *Context) error
	Tick(ctx *Context, now time.Time)
	OnActionEnd(ctx *Context, actionID int64, kind actionflow.ActionKind, reason actionflow.ActionReason)
	OnMissionEnd(ctx *Context, mission actionflow.Mission, reason actionflow.ActionReason)
	CanStopByNext(next Strategy) bool
}

// StoppableStrategy 是需要收尾的策略。Stop 在策略被替换（SetStrategy）或 Controller.Shutdown
// 时调用。
//
// Shutdown 不调 EndActions（有意保持）：替换时在途动作由
// EndActions 结束，Shutdown 时没有这一步。要在 Shutdown 时结束在途动作，在 Stop 里自己结束
// （BehaviorStrategy.Stop 重置树，配了 OnInterrupt 的节点会收尾），或由业务在 Shutdown 之前
// 调 ActionList 的 EndCurMission / EndAllAction。否则 Shutdown 之后动作继续跑，结束通知
// 没有策略接收。
type StoppableStrategy interface {
	Stop(ctx *Context, reason string)
}

// Event 是投给 AI 的一个游戏事件：Kind 由业务定义（被攻击、看见敌人、状态变化、Buff 生效……），Data 是事件
// 内容。框架不定义事件词表（ssr 的约 25 个具体事件是玩法词表，不照搬）。
type Event struct {
	Kind uint16
	Data any
}

// EventStrategy 是接收事件的策略（可选）。BehaviorStrategy 实现它：事件缓冲到下一拍，树里用 EventReceived 叶子或
// BehaviorContext.HasEvent / TakeEvent 读。
type EventStrategy interface {
	OnEvent(ctx *Context, ev Event)
}
