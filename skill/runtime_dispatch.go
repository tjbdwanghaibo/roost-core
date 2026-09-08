package skill

import (
	"errors"
	"sort"
)

var ErrRuntimeCapacityExceeded = errors.New("skill: runtime retention capacity exceeded")

func (runtime *Runtime) QueueExternalEvent(event EventContext) error {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	if runtime.host == nil {
		return ErrProgramInvariant
	}
	if event.EventID == 0 || event.Tick < runtime.currentTick || event.WorldRevision > runtime.host.CurrentRevision() {
		return ErrRevisionUnavailable
	}
	return runtime.scheduleSystem(event.Tick, &externalEventTask{Event: cloneEventContext(event)})
}

func (runtime *Runtime) dispatchEvent(event EventContext) error {
	root := event.RootEventID
	if root == 0 {
		root = event.EventID
		event.RootEventID = root
	}
	if err := runtime.trackRootEventLocked(root); err != nil {
		return err
	}
	if runtime.options.PassiveRouter == nil {
		return nil
	}
	candidates := append([]PassiveCandidate(nil), runtime.options.PassiveRouter.Candidates(cloneEventContext(event))...)
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].Owner != candidates[right].Owner {
			return candidates[left].Owner < candidates[right].Owner
		}
		if candidates[left].Ability != candidates[right].Ability {
			return candidates[left].Ability < candidates[right].Ability
		}
		leftDigest, rightDigest := "", ""
		if candidates[left].Program != nil {
			leftDigest = candidates[left].Program.identity.gameplayDigest
		}
		if candidates[right].Program != nil {
			rightDigest = candidates[right].Program.identity.gameplayDigest
		}
		return leftDigest < rightDigest
	})
	for _, candidate := range candidates {
		owner := candidate.Owner
		if owner == 0 {
			owner = event.Owner
		}
		if _, err := runtime.enqueuePassive(candidate.Program, event, owner, candidate.Ability); err != nil && err != ErrCastInputInvalid {
			return err
		}
	}
	return nil
}

func (runtime *Runtime) RuntimeEvents() []RuntimeEvent {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return cloneRuntimeEvents(runtime.runtimeEvents)
}

func (runtime *Runtime) RuntimeEventDropped() uint64 {
	runtime.mutex.Lock()
	defer runtime.mutex.Unlock()
	return runtime.runtimeEventDropped
}
