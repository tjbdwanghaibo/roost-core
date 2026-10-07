package skill

// 排队任务上限（RR-20261006-55 后续二，维护者 2026-10-07 F08-H+2 ①）：未执行的被动激活与 QueueExternalEvent 排的外部
// 事件占住根，数量之前没有上限，合法配置下也能把根事件表占满、走到 dispatchEvent 的兜底分支（跳过事件、计
// skill.root_event.capacity_dropped.total）。
//
// 承诺：合法配置下根事件表不会被占满——不论从哪个入口排任务，兜底分支都不触发：
//   - QueueExternalEvent 满了返回 ErrQueuedTasksFull；
//   - 被动候选满了按“被拒”处理（passive_suppressed，Result = queue_full），事件照常前进；
//   - ActivatePassive 满了返回 ErrQueuedTasksFull；
//   - 能力覆盖到期任务与已结束施法残留的任务不读写根账本，不钉住根。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
)

// queuedBoundOptions 是一组合法的小配置：RootEventLimit 8 > MaxActiveCasts 1 + MaxOwnedSpawns 1 + MaxStopPendingSpawns 1
// + MaxQueuedTasks 4。
func queuedBoundOptions() RuntimeOptions {
	return RuntimeOptions{RootEventLimit: 8, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 4}
}

// everyEventPassiveRouter 把每个事件都路由给同一个候选。
type everyEventPassiveRouter struct{ candidate PassiveCandidate }

func (router everyEventPassiveRouter) Candidates(EventContext) []PassiveCandidate {
	return []PassiveCandidate{router.candidate}
}

// appendDistinctRootEvents 在宿主里追加根 1..roots 各一个事件，再追加一个根为 roots+1 的探针事件。
func appendDistinctRootEvents(host *MemoryHost, roots EventID) EventID {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	for root := EventID(1); root <= roots; root++ {
		host.appendContextEventLocked("damage_resolved", 2, 0, EventContext{EventID: root, RootEventID: root, Owner: 1, Source: 2})
	}
	probe := roots + 1
	host.appendContextEventLocked("damage_resolved", 2, 0, EventContext{EventID: probe, RootEventID: probe, Owner: 1, Source: 2})
	return probe
}

func assertRootTableNeverFull(t *testing.T, runtime *Runtime, dropped int64, probe EventID) {
	t.Helper()
	if got := counterValue(MetricRootEventCapacityDropped) - dropped; got != 0 {
		t.Fatalf("%s grew by %d under a legal configuration (RootEventLimit %d); the queue entry must reject before the root table fills",
			MetricRootEventCapacityDropped, got, runtime.options.RootEventLimit)
	}
	if _, tracked := runtime.rootEventCounts[probe]; !tracked {
		t.Fatalf("probe root %d was not tracked; the event skipped passive routing", probe)
	}
}

func TestQueueExternalEventRejectsAtTheQueueBoundInsteadOfFillingTheRootTable(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{}, MemoryHostOptions{CompactEvents: true})
	runtime := NewRuntime(host, queuedBoundOptions())
	accepted := 0
	for root := EventID(1); root <= 8; root++ {
		err := runtime.QueueExternalEvent(EventContext{EventID: 100 + root, RootEventID: root, Tick: 50})
		switch {
		case err == nil:
			accepted++
		case !errors.Is(err, ErrQueuedTasksFull):
			t.Fatalf("QueueExternalEvent(root %d) = %v, want nil or ErrQueuedTasksFull", root, err)
		}
	}
	if accepted != 4 {
		t.Fatalf("accepted %d external events, want 4 (MaxQueuedTasks)", accepted)
	}
	dropped := counterValue(MetricRootEventCapacityDropped)
	probe := appendDistinctRootEvents(host, 8)
	runtime.collectHostEvents()
	assertRootTableNeverFull(t, runtime, dropped, probe)
	// 排队的任务执行掉之后名额释放。
	if err := runtime.Advance(50); err != nil {
		t.Fatal(err)
	}
	if err := runtime.QueueExternalEvent(EventContext{EventID: 200, RootEventID: 200, Tick: 60}); err != nil {
		t.Fatalf("QueueExternalEvent after the queue drained = %v", err)
	}
}

func TestPassiveCandidatesPastTheQueueBoundAreRejectedNotDropped(t *testing.T) {
	passive, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `[]`, `[]`))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, func() RuntimeOptions {
		options := queuedBoundOptions()
		options.PassiveRouter = everyEventPassiveRouter{candidate: PassiveCandidate{Program: passive, Owner: 1}}
		return options
	}())
	dropped := counterValue(MetricRootEventCapacityDropped)
	rejected := counterValue(MetricPassiveDispatchRejected)
	probe := appendDistinctRootEvents(host, 8)
	runtime.collectHostEvents()
	assertRootTableNeverFull(t, runtime, dropped, probe)
	suppressed := 0
	for _, event := range runtime.RuntimeEvents() {
		if event.Kind == "passive_suppressed" && event.Context.Result == "queue_full" {
			suppressed++
		}
	}
	// 根 1..4 的候选入队；根 5..8 与探针根 9 的候选被拒（探针根先淘汰根 5 进表，候选照样排不进）。
	if suppressed != 5 {
		t.Fatalf("queue_full suppressions = %d, want 5", suppressed)
	}
	if got := counterValue(MetricPassiveDispatchRejected) - rejected; got != 5 {
		t.Fatalf("%s grew by %d, want 5", MetricPassiveDispatchRejected, got)
	}
	if _, err := runtime.ActivatePassive(passive, EventContext{EventID: 300, RootEventID: 300, Owner: 1}); !errors.Is(err, ErrQueuedTasksFull) {
		t.Fatalf("ActivatePassive at the queue bound = %v, want ErrQueuedTasksFull", err)
	}
	if _, tracked := runtime.rootEventCounts[300]; tracked {
		t.Fatal("a rejected ActivatePassive tracked its root")
	}
}

func TestAbilityOverlayExpiriesDoNotPinRoots(t *testing.T) {
	environment := abilityTestEnvironment()
	program := compileAbilityTestSkill(t, environment, "overlay", `{"mode":"tap"}`, 0, `{"flow":"finish"}`)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, queuedBoundOptions())
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	handle := runtime.abilityByProgram[skillStateKey{Caster: 1, Skill: program.id}]
	// 覆盖数没有配置上限：12 个不同根的覆盖都在 5 tick 内有效。
	for root := EventID(1); root <= 12; root++ {
		if _, err := runtime.ModifyAbilityState(1, handle, "enabled", "set", BoolRuntimeValue(false), 5, EventContext{EventID: 100 + root, RootEventID: root}); err != nil {
			t.Fatal(err)
		}
	}
	dropped := counterValue(MetricRootEventCapacityDropped)
	probe := appendDistinctRootEvents(host, 8)
	runtime.collectHostEvents()
	assertRootTableNeverFull(t, runtime, dropped, probe)
}

// TestTasksOfEndedCastsDoNotPinRoots：已结束施法残留的任务执行时是空操作（phase token 已推进或状态是终态），不再在
// 这个根下产生事件，不钉住根；未结束施法的任务与施法同根，已由施法本身计入 MaxActiveCasts。
func TestTasksOfEndedCastsDoNotPinRoots(t *testing.T) {
	program, environment := compileRuntimeJSON(t, spawnLifecycleSkill("ended", spawnLifecycleActive, `{"flow":"wait","ticks":10,"then":{"flow":"finish"}}`))
	runtime := NewRuntime(runtimeTestHost(environment), queuedBoundOptions())
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	cast := runtime.casts[castID]
	root := cast.eventContext.RootEventID
	staleToken := cast.phaseToken
	if err := runtime.Cancel(castID); err != nil {
		t.Fatal(err)
	}
	// 上一个 phase token 的残留任务（Cancel 只撤当前 token 的任务）。
	runtime.scheduler.Push(scheduledTask{DueTick: 20, Sequence: 1 << 40, Payload: &flowContinuationTask{CastID: castID, PhaseToken: staleToken}})
	if runtime.rootEventReferencedLocked(root) {
		t.Fatalf("root %d is pinned by a stale task of an ended cast; such tasks are no-ops and fall outside every configured bound", root)
	}
}

// TestLegalConfigurationNeverReachesTheRootTableFallback 从所有排任务的入口一起压：合法配置下兜底分支一次也不触发。
// 兜底分支本身仍保留作防御（TestCollectHostEventsSkipsEventWhenEveryRootIsPinned 绕过入口直接塞任务验证它）。
func TestLegalConfigurationNeverReachesTheRootTableFallback(t *testing.T) {
	passive, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `[]`, `[]`))
	host := runtimeTestHost(environment)
	options := queuedBoundOptions()
	options.PassiveRouter = everyEventPassiveRouter{candidate: PassiveCandidate{Program: passive, Owner: 1}}
	runtime := NewRuntime(host, options)
	dropped := counterValue(MetricRootEventCapacityDropped)
	next := EventID(1)
	for tick := Tick(1); tick <= 40; tick++ {
		for index := 0; index < 3; index++ {
			_ = runtime.QueueExternalEvent(EventContext{EventID: 10000 + next, RootEventID: 10000 + next, Tick: tick + Tick(index)})
			_, _ = runtime.ActivatePassive(passive, EventContext{EventID: 20000 + next, RootEventID: 20000 + next, Owner: 1, Tick: tick + 2})
			host.mutex.Lock()
			host.appendContextEventLocked("damage_resolved", 2, 0, EventContext{EventID: next, RootEventID: next, Owner: 1, Source: 2})
			host.mutex.Unlock()
			next++
		}
		if err := runtime.Advance(tick); err != nil {
			t.Fatalf("Advance(%d) = %v", tick, err)
		}
		if queued := runtime.queuedTaskCountLocked(); queued > options.MaxQueuedTasks {
			t.Fatalf("tick %d: %d queued tasks exceed MaxQueuedTasks %d", tick, queued, options.MaxQueuedTasks)
		}
	}
	if got := counterValue(MetricRootEventCapacityDropped) - dropped; got != 0 {
		t.Fatalf("%s grew by %d under a legal configuration; the fallback must not trigger in normal operation", MetricRootEventCapacityDropped, got)
	}
}

// TestCheckpointCarriesTheQueueBound：max_queued_tasks 随 checkpoint 走（版本 9），恢复后排队任务数照样计数；排程里的
// 排队任务超过它、或缺这个字段的 checkpoint 按 corrupt 拒绝。
func TestCheckpointCarriesTheQueueBound(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, queuedBoundOptions())
	for root := EventID(1); root <= 3; root++ {
		if err := runtime.QueueExternalEvent(EventContext{EventID: 100 + root, RootEventID: root, Tick: 50}); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if restored.options.MaxQueuedTasks != 4 || restored.queuedTaskCountLocked() != 3 {
		t.Fatalf("restored MaxQueuedTasks = %d, queued = %d; want 4 and 3", restored.options.MaxQueuedTasks, restored.queuedTaskCountLocked())
	}
	if err := restored.QueueExternalEvent(EventContext{EventID: 104, RootEventID: 4, Tick: 50}); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueueExternalEvent(EventContext{EventID: 105, RootEventID: 5, Tick: 50}); !errors.Is(err, ErrQueuedTasksFull) {
		t.Fatalf("fifth queued event after restore = %v, want ErrQueuedTasksFull", err)
	}
	for _, testCase := range []struct {
		name  string
		value json.RawMessage
	}{
		{name: "queued tasks above the bound", value: json.RawMessage("2")},
		{name: "missing bound", value: nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(checkpoint.Payload, &fields); err != nil {
				t.Fatal(err)
			}
			if testCase.value == nil {
				delete(fields, "max_queued_tasks")
			} else {
				fields["max_queued_tasks"] = testCase.value
			}
			tampered := checkpoint
			if tampered.Payload, err = json.Marshal(fields); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(tampered.Payload)
			tampered.Checksum = hex.EncodeToString(digest[:])
			if _, err := RestoreRuntime(host, RuntimeOptions{}, tampered, resolver); !errors.Is(err, ErrCheckpointCorrupt) {
				t.Fatalf("RestoreRuntime = %v, want ErrCheckpointCorrupt", err)
			}
		})
	}
}
