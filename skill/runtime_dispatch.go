package skill

import (
	"errors"
	"log/slog"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

var ErrRuntimeCapacityExceeded = errors.New("skill: runtime retention capacity exceeded")

// MetricPassiveDispatchRejected 计事件派发时被永久拒绝的被动候选（label reason：host_capability = 路由给出的被动
// Program 不在 Host 能力表里；rejected = 其他入队错误）。
const MetricPassiveDispatchRejected = "skill.passive.dispatch_rejected.total"

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

// dispatchEvent 把一个 Host 事件或外部事件交给被动路由。
//
// 候选被拒（路由给出的被动 Program 不在 Host 能力表里，或其他入队错误）是这个候选的永久结果，重试同一个事件不会变好：
// 记一条 passive_suppressed（Result 是原因）、计 MetricPassiveDispatchRejected、写一条 Warn，其余候选照常入队，事件照常
// 前进（RR-20261006-55）。之前第一个出错的候选让整个派发返回错误：collectHostEvents 不推进 eventCursor，之后每次
// Advance 都在同一个事件上报同一个错，Runtime 永久卡住，排在前面已入队的候选每次重试再入队一次。被拒的被动 Program
// 在它自己扣任何费用之前被拒（enqueuePassive 的准入核对），B3 ③ 对它成立。
//
// 唯一返回的错误是根事件表满且都被引用（ErrRuntimeCapacityExceeded）：这时还没有候选入队，重试同一个事件不会重复入队；
// 引用根的施法 / 衍生物结束后表就腾出来，这是容量（RootEventLimit）问题，按原有约定留在原处、由 tick 上的
// collectHostEvents 重试并报给 Advance 的调用方（runtime_event_test.go 的 TestCollectHostEventsDoesNotAdvanceOrCompactFailedEvent）。
// 施法路径上的 drainHostEvents 遇到它只停下、不返回错误，见那里。
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
		_, err := runtime.enqueuePassive(candidate.Program, event, owner, candidate.Ability)
		switch {
		case err == nil, errors.Is(err, ErrCastInputInvalid):
			// ErrCastInputInvalid：不是被动 Program、没有 owner，是路由的正常过滤，不告警（原有行为）。
		case errors.Is(err, ErrHostCapabilityMissing):
			runtime.rejectPassiveCandidateLocked(event, candidate.Program, owner, "host_capability", err)
		default:
			runtime.rejectPassiveCandidateLocked(event, candidate.Program, owner, "rejected", err)
		}
	}
	return nil
}

// rejectPassiveCandidateLocked 记录一个被拒的被动候选：passive_suppressed（与 proc 策略的抑制同一种事件，Result 是原因）、
// 指标与 Warn 日志。
func (runtime *Runtime) rejectPassiveCandidateLocked(event EventContext, program *Program, owner EntityID, reason string, cause error) {
	metrics.IncCounter(MetricPassiveDispatchRejected, metrics.Labels{"reason": reason}, 1)
	programID := ""
	if program != nil {
		programID = program.id
	}
	slog.Default().Warn("skill: passive candidate rejected while dispatching an event; the event moves on without it",
		"reason", reason, "event_id", event.EventID, "root_event_id", event.RootEventID, "source_skill", event.SkillID,
		"program", programID, "owner", owner, "error", cause)
	runtime.appendRuntimeEvent(RuntimeEvent{Tick: runtime.currentTick, Kind: "passive_suppressed", Entity: owner, Context: EventContext{RootEventID: event.RootEventID, ParentEventID: event.EventID, Owner: owner, SkillID: programID, Result: reason}})
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
