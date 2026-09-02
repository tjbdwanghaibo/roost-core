// Package ai defines entity-aware AI strategy boundaries. Scheduling and
// concrete decision algorithms are supplied by adapters and cube-kit.
package ai

import (
	"time"

	"github.com/tjbdwanghaibo/cube-core/actionflow"
	"github.com/tjbdwanghaibo/cube-core/entity"
)

type Context struct {
	Owner      entity.IThreadSafeEntity
	ActionList actionflow.ActionList
	Now        time.Time
}

type Strategy interface {
	Name() string
	Init(ctx *Context) error
	Tick(ctx *Context, now time.Time)
	OnActionEnd(ctx *Context, actionID int64, kind actionflow.ActionKind, reason actionflow.ActionReason)
	OnMissionEnd(ctx *Context, mission actionflow.Mission, reason actionflow.ActionReason)
	CanStopByNext(next Strategy) bool
}

type StoppableStrategy interface {
	Stop(ctx *Context, reason string)
}
