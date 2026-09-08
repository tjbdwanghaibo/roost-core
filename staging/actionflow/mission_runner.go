package actionflow

import (
	"errors"
	"fmt"
	"math"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

var (
	ErrMissionRunning     = errors.New("taskflow: mission already running")
	ErrMissionNotRunning  = errors.New("taskflow: mission not running")
	ErrMissionIDExhausted = errors.New("taskflow: mission id exhausted")
)

type MissionRunnerHooks struct {
	Now          func() time.Time
	Context      func(time.Time) *coreflow.MissionContext
	ClearActions func(int64, coreflow.ActionReason) error
	OnState      func(bool)
	OnChanged    func(coreflow.MissionInfo)
	OnEnded      func(coreflow.Mission, coreflow.ActionReason)
	OnError      func(error)
}

type MissionRunnerConfig struct {
	Registry    *Registry
	DefaultKind coreflow.MissionKind
	Hooks       MissionRunnerHooks
}

type MissionRunner struct {
	registry    *Registry
	defaultKind coreflow.MissionKind
	hooks       MissionRunnerHooks
	nextID      int64
	cur         coreflow.Mission
	lastInfo    coreflow.MissionInfo
	ending      bool
	starting    bool
	lastEndTime time.Time
}

func NewMissionRunner(config MissionRunnerConfig) (*MissionRunner, error) {
	if config.Registry == nil {
		return nil, ErrMissionBuilderNotFound
	}
	return &MissionRunner{registry: config.Registry, defaultKind: config.DefaultKind, hooks: config.Hooks, lastInfo: coreflow.MissionInfo{CurrentStep: -1}}, nil
}

func (r *MissionRunner) StartMission(kind coreflow.MissionKind, param any) error {
	if r == nil || r.starting || r.ending {
		return ErrReentrantMutation
	}
	r.starting = true
	defer func() { r.starting = false }()
	if kind == 0 {
		kind = r.defaultKind
	}
	if kind == 0 {
		return ErrKindInvalid
	}
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
		r.EndCurMission(coreflow.NewActionReason("replaced by next mission"))
		if r.cur != nil {
			return ErrReentrantMutation
		}
	}
	r.nextID++
	if setter, ok := next.(coreflow.MissionRuntimeSetter); ok {
		setter.SetRuntime(r.nextID, r)
	}
	r.cur = next
	r.state(true)
	r.changed(next.MissionInfo())
	if r.cur != next {
		return ErrReentrantMutation
	}
	ctx := r.context(r.now())
	startErr := callMissionStart(next, ctx, param)
	if r.cur != next {
		if r.cur == nil {
			return startErr
		}
		return errors.Join(startErr, ErrReentrantMutation)
	}
	if startErr != nil {
		r.ending = true
		reason := coreflow.NewActionErrorReason(startErr)
		endErr := callMissionEnd(next, ctx, reason)
		r.ending = false
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

func (r *MissionRunner) CancelMission(reason string) error {
	if r == nil || r.cur == nil {
		return ErrMissionNotRunning
	}
	if reason == "" {
		reason = "canceled"
	}
	r.EndCurMission(coreflow.NewActionReason(reason))
	return nil
}

func (r *MissionRunner) InMission() bool {
	return r != nil && r.cur != nil && r.cur.Status() == coreflow.MissionStatusRunning
}
func (r *MissionRunner) CurrentAction() coreflow.ActionKind {
	if r == nil || r.cur == nil {
		return 0
	}
	return r.cur.MissionInfo().CurrentAction
}
func (r *MissionRunner) CurMission() coreflow.Mission {
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
func (r *MissionRunner) MissionInfo() coreflow.MissionInfo {
	if r == nil {
		return coreflow.MissionInfo{CurrentStep: -1}
	}
	if r.cur != nil {
		return r.cur.MissionInfo()
	}
	return r.lastInfo
}

func (r *MissionRunner) OnActionEnd(actionID int64, kind coreflow.ActionKind, reason coreflow.ActionReason) {
	if r == nil || r.cur == nil || r.ending {
		return
	}
	cur := r.cur
	if err := callMissionActionEnd(cur, r.context(r.now()), actionID, kind, reason); err != nil {
		r.report(err)
		if r.cur == cur {
			r.EndCurMission(coreflow.NewActionErrorReason(err))
		}
		return
	}
	if r.cur == cur {
		r.update()
	}
}

func (r *MissionRunner) EndCurMission(reason coreflow.ActionReason) {
	if r == nil || r.cur == nil || r.ending {
		return
	}
	r.ending = true
	defer func() { r.ending = false }()
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

func (r *MissionRunner) Tick(now time.Time) {
	if r == nil || r.cur == nil || r.ending {
		return
	}
	cur := r.cur
	if now.IsZero() {
		now = r.now()
	}
	if err := callMissionTick(cur, r.context(now), now); err != nil {
		r.report(err)
		if r.cur == cur {
			r.EndCurMission(coreflow.NewActionErrorReason(err))
		}
		return
	}
	if r.cur == cur {
		r.update()
	}
}

func (r *MissionRunner) Reset() {
	r.cur = nil
	r.lastInfo = coreflow.MissionInfo{CurrentStep: -1}
	r.ending = false
	r.starting = false
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
	return time.Now()
}
func (r *MissionRunner) context(now time.Time) (ctx *coreflow.MissionContext) {
	if now.IsZero() {
		now = r.now()
	}
	ctx = &coreflow.MissionContext{Manager: r, Now: now}
	if r.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: mission context hook panic: %v", recovered))
				ctx = &coreflow.MissionContext{Manager: r, Now: now}
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
func (r *MissionRunner) changed(info coreflow.MissionInfo) {
	if r.hooks.OnChanged != nil {
		defer r.recoverHook("changed")
		r.hooks.OnChanged(info)
	}
}
func (r *MissionRunner) ended(mission coreflow.Mission, reason coreflow.ActionReason) {
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

func (r *MissionRunner) clearActions(missionID int64, reason coreflow.ActionReason) (err error) {
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

func callMissionStart(m coreflow.Mission, ctx *coreflow.MissionContext, param any) (err error) {
	defer recoverMissionPanic("start", &err)
	return m.Start(ctx, param)
}
func callMissionCanReplace(m coreflow.Mission, kind coreflow.MissionKind, param any) (allowed bool, err error) {
	defer recoverMissionPanic("can replace", &err)
	return m.CanReplaceBy(kind, param), nil
}
func callMissionTick(m coreflow.Mission, ctx *coreflow.MissionContext, now time.Time) (err error) {
	defer recoverMissionPanic("tick", &err)
	m.Tick(ctx, now)
	return nil
}
func callMissionActionEnd(m coreflow.Mission, ctx *coreflow.MissionContext, id int64, kind coreflow.ActionKind, reason coreflow.ActionReason) (err error) {
	defer recoverMissionPanic("action end", &err)
	m.OnActionEnd(ctx, id, kind, reason)
	return nil
}
func callMissionEnd(m coreflow.Mission, ctx *coreflow.MissionContext, reason coreflow.ActionReason) (err error) {
	defer recoverMissionPanic("end", &err)
	m.End(ctx, reason)
	return nil
}
func recoverMissionPanic(operation string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("taskflow: mission %s panic: %v", operation, recovered)
	}
}
