// Package ai defines entity-aware AI strategy boundaries. Scheduling and
// concrete decision algorithms are supplied by adapters and roost-kit.
package ai

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/actionflow"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

type Context struct {
	Owner      entity.IThreadSafeEntity
	ActionList actionflow.ActionList
	Now        time.Time
}

// Strategy 是一个 AI 决策策略，由 Controller 调用。
//
// Init 里不要发起动作：SetStrategy 先 Init 新策略、成功后才 EndActions 结束现有动作（“Init
// 失败保留旧策略”的事务式顺序），Init 里发起的动作会被一并结束。在第一次 Tick 里发起
// （N10 O-T3，维护者第十二轮决定保持这个顺序）。
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
// Shutdown 不调 EndActions（N10 O-T4，维护者第十二轮决定保持）：替换时在途动作由
// EndActions 结束，Shutdown 时没有这一步。要在 Shutdown 时结束在途动作，在 Stop 里自己结束
// （BehaviorStrategy.Stop 重置树，配了 OnInterrupt 的节点会收尾），或由业务在 Shutdown 之前
// 调 ActionList 的 EndCurMission / EndAllAction。否则 Shutdown 之后动作继续跑，结束通知
// 没有策略接收。
type StoppableStrategy interface {
	Stop(ctx *Context, reason string)
}
