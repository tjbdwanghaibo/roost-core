package actionflow

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	coreflow "github.com/tjbdwanghaibo/roost-core/actionflow"
)

var (
	ErrActionGroupInvalid = errors.New("taskflow: action group is invalid")
	ErrActionGroupFrozen  = errors.New("taskflow: action group is frozen")
	ErrActionIDExhausted  = errors.New("taskflow: action id exhausted")
	ErrReentrantMutation  = errors.New("taskflow: reentrant mutation changed current action")
)

type ActionSnapshot struct {
	ID        int64
	MissionID int64
	Kind      coreflow.ActionKind
	Group     coreflow.ActionGroup
	Action    coreflow.Action
}

type ActionRunnerHooks struct {
	// PopulateContext is the allocation-free context hook. Context remains for
	// compatibility and is copied into runner-owned callback-scoped storage.
	PopulateContext func(*coreflow.ActionContext, time.Time)
	Context         func(time.Time) *coreflow.ActionContext
	OnQueued        func(ActionSnapshot)
	OnTransition    func(ActionSnapshot, bool)
	OnEnded         func(ActionSnapshot, coreflow.ActionReason)
	OnError         func(error)
}

type ActionRunnerConfig struct {
	Registry     *Registry
	GroupForKind func(coreflow.ActionKind) (coreflow.ActionGroup, bool)
	Hooks        ActionRunnerHooks
}

type actionEntry struct{ ActionSnapshot }
type actionGroupState struct {
	frozen bool
	cur    *actionEntry
	next   []*actionEntry
}

// ActionRunner is intentionally lock-free. The owning entity must serialize
// every call with its Entity mutex, including heartbeat-driven Tick calls.
type ActionRunner struct {
	registry     *Registry
	groupForKind func(coreflow.ActionKind) (coreflow.ActionGroup, bool)
	hooks        ActionRunnerHooks
	nextID       int64
	groups       map[coreflow.ActionGroup]*actionGroupState
	contextPool  sync.Pool
}

func NewActionRunner(config ActionRunnerConfig) (*ActionRunner, error) {
	if config.Registry == nil || config.GroupForKind == nil {
		return nil, ErrActionGroupInvalid
	}
	runner := &ActionRunner{
		registry:     config.Registry,
		groupForKind: config.GroupForKind,
		hooks:        config.Hooks,
		groups:       make(map[coreflow.ActionGroup]*actionGroupState),
	}
	runner.contextPool.New = func() any { return new(coreflow.ActionContext) }
	return runner, nil
}

func (r *ActionRunner) Start(kind coreflow.ActionKind, param any, missionID int64, now time.Time) (int64, error) {
	group, ok, err := r.resolveGroup(kind)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: kind=%d", ErrActionGroupInvalid, kind)
	}
	unit := r.group(group)
	if unit.frozen {
		return 0, ErrActionGroupFrozen
	}
	entry, err := r.build(kind, param, missionID, group)
	if err != nil {
		return 0, err
	}
	if unit.cur != nil {
		if err := r.finish(unit, unit.cur, true, coreflow.NewActionReason("replaced by next action"), false); err != nil {
			return 0, err
		}
		if unit.cur != nil {
			return 0, ErrReentrantMutation
		}
	}
	if err := r.start(unit, entry, now, false); err != nil {
		return 0, err
	}
	return entry.ID, nil
}

func (r *ActionRunner) Enqueue(kind coreflow.ActionKind, param any, missionID int64) (int64, error) {
	group, ok, err := r.resolveGroup(kind)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: kind=%d", ErrActionGroupInvalid, kind)
	}
	entry, err := r.build(kind, param, missionID, group)
	if err != nil {
		return 0, err
	}
	unit := r.group(group)
	unit.next = append(unit.next, entry)
	r.queued(entry)
	if unit.cur == nil && !unit.frozen {
		if err := r.startNext(unit, time.Time{}); err != nil {
			return entry.ID, err
		}
	}
	return entry.ID, nil
}

func (r *ActionRunner) Current(group coreflow.ActionGroup) coreflow.Action {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return nil
	}
	return unit.cur.Action
}

func (r *ActionRunner) CurrentSnapshot(group coreflow.ActionGroup) (ActionSnapshot, bool) {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return ActionSnapshot{}, false
	}
	return unit.cur.ActionSnapshot, true
}

func (r *ActionRunner) QueueLength(group coreflow.ActionGroup) int {
	unit := r.groups[group]
	if unit == nil {
		return 0
	}
	return len(unit.next)
}

func (r *ActionRunner) Pending(group coreflow.ActionGroup) []ActionSnapshot {
	unit := r.groups[group]
	if unit == nil || len(unit.next) == 0 {
		return nil
	}
	pending := make([]ActionSnapshot, 0, len(unit.next))
	for _, entry := range unit.next {
		if entry != nil {
			pending = append(pending, entry.ActionSnapshot)
		}
	}
	return pending
}

func (r *ActionRunner) Update(group coreflow.ActionGroup, fn func(coreflow.Action) error) error {
	if fn == nil {
		return nil
	}
	unit := r.groups[group]
	if unit == nil || unit.cur == nil || unit.cur.Action == nil {
		return nil
	}
	return fn(unit.cur.Action)
}

func (r *ActionRunner) Tick(group coreflow.ActionGroup, now time.Time) error {
	unit := r.groups[group]
	if unit == nil || unit.frozen || unit.cur == nil || unit.cur.Action == nil {
		return nil
	}
	entry := unit.cur
	done, result, err := r.callTick(entry.Action, now)
	if err != nil {
		result = coreflow.ActionResult{Status: coreflow.ActionStatusFailed, Reason: err.Error()}
		done = true
		r.report(err)
	}
	if unit.cur != entry {
		return ErrReentrantMutation
	}
	if !done {
		return nil
	}
	if result.Status == coreflow.ActionStatusIdle {
		result.Status = coreflow.ActionStatusSuccess
	}
	return r.finish(unit, entry, false, coreflow.NewActionResultReason(result), true)
}

func (r *ActionRunner) End(group coreflow.ActionGroup, force bool, reason coreflow.ActionReason) error {
	unit := r.groups[group]
	if unit == nil || unit.cur == nil {
		return nil
	}
	return r.finish(unit, unit.cur, force, reason, true)
}

func (r *ActionRunner) EndAll(force bool, reason coreflow.ActionReason) error {
	var errs []error
	for _, unit := range r.orderedGroups() {
		if unit.cur != nil {
			if err := r.finish(unit, unit.cur, force, reason, false); err != nil {
				errs = append(errs, err)
			}
		}
		unit.next = nil
	}
	return errors.Join(errs...)
}

func (r *ActionRunner) ClearQueue() {
	for _, unit := range r.orderedGroups() {
		unit.next = nil
	}
}

func (r *ActionRunner) ClearMission(missionID int64, cancel bool, reason coreflow.ActionReason) error {
	if missionID == 0 {
		return nil
	}
	var errs []error
	for _, unit := range r.orderedGroups() {
		if unit.cur != nil && unit.cur.MissionID == missionID {
			if err := r.finish(unit, unit.cur, cancel, reason, false); err != nil {
				errs = append(errs, err)
			}
		}
		dst := unit.next[:0]
		for _, next := range unit.next {
			if next != nil && next.MissionID != missionID {
				dst = append(dst, next)
			}
		}
		for index := len(dst); index < len(unit.next); index++ {
			unit.next[index] = nil
		}
		unit.next = dst
	}
	return errors.Join(errs...)
}

func (r *ActionRunner) Freeze(group coreflow.ActionGroup) { r.group(group).frozen = true }
func (r *ActionRunner) Frozen(group coreflow.ActionGroup) bool {
	unit := r.groups[group]
	return unit != nil && unit.frozen
}

func (r *ActionRunner) Recover(group coreflow.ActionGroup, now time.Time) error {
	unit := r.group(group)
	unit.frozen = false
	if unit.cur == nil {
		return r.startNext(unit, now)
	}
	return nil
}

func (r *ActionRunner) build(kind coreflow.ActionKind, param any, missionID int64, group coreflow.ActionGroup) (*actionEntry, error) {
	if r.nextID == math.MaxInt64 {
		return nil, ErrActionIDExhausted
	}
	action, err := r.registry.BuildAction(kind, param)
	if err != nil {
		return nil, err
	}
	r.nextID++
	entry := &actionEntry{ActionSnapshot: ActionSnapshot{ID: r.nextID, MissionID: missionID, Kind: kind, Group: group, Action: action}}
	return entry, nil
}

func (r *ActionRunner) resolveGroup(kind coreflow.ActionKind) (group coreflow.ActionGroup, ok bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("taskflow: group resolver panic for kind %d: %v", kind, recovered)
		}
	}()
	group, ok = r.groupForKind(kind)
	return group, ok, nil
}

func (r *ActionRunner) start(unit *actionGroupState, entry *actionEntry, now time.Time, queued bool) error {
	if unit == nil || entry == nil || entry.Action == nil {
		return ErrBuilderNil
	}
	unit.cur = entry
	r.transition(entry, true)
	if unit.cur != entry {
		return ErrReentrantMutation
	}
	err := r.callStart(entry.Action, now)
	if unit.cur != entry {
		return ErrReentrantMutation
	}
	if err == nil {
		return nil
	}
	cancelErr := r.callCancel(entry.Action, now, "start failed")
	unit.cur = nil
	r.transition(entry, false)
	err = errors.Join(err, cancelErr)
	if queued {
		reason := coreflow.NewActionErrorReason(err)
		r.ended(entry, reason)
		r.report(fmt.Errorf("taskflow: queued action %d start: %w", entry.ID, err))
		return errors.Join(err, r.startNext(unit, now))
	}
	return err
}

func (r *ActionRunner) finish(unit *actionGroupState, entry *actionEntry, cancel bool, reason coreflow.ActionReason, startNext bool) error {
	if unit == nil || entry == nil || unit.cur != entry {
		return nil
	}
	var err error
	if cancel && entry.Action != nil {
		if reason.Result.Status == coreflow.ActionStatusIdle {
			reason.Result = coreflow.ActionResult{Status: coreflow.ActionStatusCanceled, Reason: reason.Message}
		}
		err = r.callCancel(entry.Action, time.Time{}, reason.Message)
	}
	if unit.cur != entry {
		return errors.Join(err, ErrReentrantMutation)
	}
	unit.cur = nil
	r.transition(entry, false)
	r.ended(entry, reason)
	if startNext && !unit.frozen && unit.cur == nil {
		err = errors.Join(err, r.startNext(unit, time.Time{}))
	}
	return err
}

func (r *ActionRunner) startNext(unit *actionGroupState, now time.Time) error {
	for unit != nil && unit.cur == nil && !unit.frozen && len(unit.next) > 0 {
		next := unit.next[0]
		copy(unit.next, unit.next[1:])
		unit.next[len(unit.next)-1] = nil
		unit.next = unit.next[:len(unit.next)-1]
		if err := r.start(unit, next, now, true); err != nil {
			return err
		}
	}
	return nil
}

func (r *ActionRunner) group(group coreflow.ActionGroup) *actionGroupState {
	unit := r.groups[group]
	if unit == nil {
		unit = &actionGroupState{}
		r.groups[group] = unit
	}
	return unit
}

func (r *ActionRunner) orderedGroups() []*actionGroupState {
	keys := make([]coreflow.ActionGroup, 0, len(r.groups))
	for group := range r.groups {
		keys = append(keys, group)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	groups := make([]*actionGroupState, 0, len(keys))
	for _, group := range keys {
		groups = append(groups, r.groups[group])
	}
	return groups
}

func (r *ActionRunner) acquireContext(now time.Time) (ctx *coreflow.ActionContext) {
	if now.IsZero() {
		now = time.Now()
	}
	ctx = r.contextPool.Get().(*coreflow.ActionContext)
	*ctx = coreflow.ActionContext{Now: now}
	if r.hooks.PopulateContext != nil || r.hooks.Context != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action context hook panic: %v", recovered))
				*ctx = coreflow.ActionContext{Now: now}
			}
		}()
		if r.hooks.PopulateContext != nil {
			r.hooks.PopulateContext(ctx, now)
		} else if candidate := r.hooks.Context(now); candidate != nil {
			*ctx = *candidate
		}
	}
	return ctx
}

func (r *ActionRunner) releaseContext(ctx *coreflow.ActionContext) {
	if ctx == nil {
		return
	}
	*ctx = coreflow.ActionContext{}
	r.contextPool.Put(ctx)
}

func (r *ActionRunner) callStart(action coreflow.Action, now time.Time) error {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionStart(action, ctx)
}

func (r *ActionRunner) callTick(action coreflow.Action, now time.Time) (bool, coreflow.ActionResult, error) {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionTick(action, ctx)
}

func (r *ActionRunner) callCancel(action coreflow.Action, now time.Time, reason string) error {
	ctx := r.acquireContext(now)
	defer r.releaseContext(ctx)
	return callActionCancel(action, ctx, reason)
}

func (r *ActionRunner) transition(entry *actionEntry, active bool) {
	if r.hooks.OnTransition != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action transition hook panic: %v", recovered))
			}
		}()
		r.hooks.OnTransition(entry.ActionSnapshot, active)
	}
}
func (r *ActionRunner) queued(entry *actionEntry) {
	if r.hooks.OnQueued != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action queued hook panic: %v", recovered))
			}
		}()
		r.hooks.OnQueued(entry.ActionSnapshot)
	}
}
func (r *ActionRunner) ended(entry *actionEntry, reason coreflow.ActionReason) {
	if r.hooks.OnEnded != nil {
		defer func() {
			if recovered := recover(); recovered != nil {
				r.report(fmt.Errorf("taskflow: action ended hook panic: %v", recovered))
			}
		}()
		r.hooks.OnEnded(entry.ActionSnapshot, reason)
	}
}
func (r *ActionRunner) report(err error) {
	if err != nil && r.hooks.OnError != nil {
		defer func() { _ = recover() }()
		r.hooks.OnError(err)
	}
}

func callActionStart(action coreflow.Action, ctx *coreflow.ActionContext) (err error) {
	defer recoverActionPanic("start", &err)
	return action.Start(ctx)
}
func callActionTick(action coreflow.Action, ctx *coreflow.ActionContext) (done bool, result coreflow.ActionResult, err error) {
	defer recoverActionPanic("tick", &err)
	done, result = action.Tick(ctx)
	return done, result, nil
}
func callActionCancel(action coreflow.Action, ctx *coreflow.ActionContext, reason string) (err error) {
	defer recoverActionPanic("cancel", &err)
	action.Cancel(ctx, reason)
	return nil
}
func recoverActionPanic(operation string, err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("taskflow: action %s panic: %v", operation, recovered)
	}
}
