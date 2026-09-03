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
