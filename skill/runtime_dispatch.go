package skill

import (
	"errors"
	"log/slog"
	"sort"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

var ErrRuntimeCapacityExceeded = errors.New("skill: runtime retention capacity exceeded")

// MetricRootEventCapacityDropped 计因根事件表满、且表里每个根都还被引用而跳过被动路由的事件（RR-20261006-55 后续）。
const MetricRootEventCapacityDropped = "skill.root_event.capacity_dropped.total"

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

// dispatchEvent 把一个 Host 事件或外部事件交给被动路由。它不返回错误：事件总是前进，派发不出去的部分只告警。
//
// 候选被拒（路由给出的被动 Program 不在 Host 能力表里，或其他入队错误）是这个候选的永久结果，重试同一个事件不会变好：
// 记一条 passive_suppressed（Result 是原因）、计 MetricPassiveDispatchRejected、写一条 Warn，其余候选照常入队，事件照常
// 前进（RR-20261006-55）。之前第一个出错的候选让整个派发返回错误：collectHostEvents 不推进 eventCursor，之后每次
// Advance 都在同一个事件上报同一个错，Runtime 永久卡住，排在前面已入队的候选每次重试再入队一次。被拒的被动 Program
// 在它自己扣任何费用之前被拒（enqueuePassive 的准入核对），B3 ③ 对它成立。
//
// 根事件表满、且表里每个根都还被引用（trackRootEventLocked 返回 ErrRuntimeCapacityExceeded）：跳过这个事件的被动路由，
// 计 MetricRootEventCapacityDropped、写一条 Error，事件照常前进（RR-20261006-55 后续，维护者选 A）。施法与衍生物占不满
// 这张表（validateRootEventLimit），能占满它的只有排程任务钉住的根（rootEventReferenceBound 的说明），例如同一 tick 里
// 大量不同根的事件各自排了被动激活。之前这里返回错误、collectHostEvents 停在这个事件上原地重试，而引用根的施法要靠
// 排程任务推进才能结束、排程任务在同一次 Advance 里排在事件派发之后，Runtime 会一直停到调用方 Cancel / Interrupt /
// Release / RemoveProgram 腾出表。
func (runtime *Runtime) dispatchEvent(event EventContext) {
	root := event.RootEventID
	if root == 0 {
		root = event.EventID
		event.RootEventID = root
	}
	if err := runtime.trackRootEventLocked(root); err != nil {
		metrics.IncCounter(MetricRootEventCapacityDropped, nil, 1)
		slog.Default().Error("skill: root event table is full and every root is still referenced; the event skips passive routing and moves on",
			"event_id", event.EventID, "root_event_id", root, "source_skill", event.SkillID,
			"root_event_limit", runtime.options.RootEventLimit, "root_events", len(runtime.rootEventCounts), "error", err)
		return
	}
	if runtime.options.PassiveRouter == nil {
		return
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
