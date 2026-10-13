package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// 排队任务上限（维护者 2026-10-07 F08-H+2 ①）：未执行的被动激活与 QueueExternalEvent 排的外部事件占住根，
// 合法配置下根事件表不会被占满——不论从哪个入口排任务，dispatchEvent 的兜底分支都不触发：
//   - QueueExternalEvent 满了返回 ErrQueuedTasksFull；
//   - 被动候选满了按“被拒”处理（passive_suppressed，Result = queue_full），事件照常前进；
//   - ActivatePassive 满了返回 ErrQueuedTasksFull；
//   - 能力覆盖到期任务与已结束施法残留的任务不读写根账本，不钉住根。

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
	// 覆盖上限独立于根事件上限：12 个不同根的覆盖都在 5 tick 内有效。
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

// 根事件表的结构性保证：RootEventLimit 必须大于同时被引用的根数上界 MaxActiveCasts + MaxOwnedSpawns +
// MaxStopPendingSpawns + MaxQueuedTasks（rootEventReferenceBound），违反时 NewRuntime 直接 panic（维护者 F08-H+2 ③ 决定保持）、
// RestoreRuntime 返回错误，并点出全部选项与当前值。兜底分支只作防御，见 runtime_dispatch_test.go 的
// TestCollectHostEventsSkipsEventWhenEveryRootIsPinned；合法配置下不触发见本文件的排队任务上限用例。

func newRuntimeRecover(host Host, options RuntimeOptions) (runtime *Runtime, recovered any) {
	defer func() { recovered = recover() }()
	return NewRuntime(host, options), nil
}

func TestNewRuntimeRejectsRootEventLimitNotAboveReferencedRootBound(t *testing.T) {
	cases := []struct {
		name    string
		options RuntimeOptions
		reject  bool
	}{
		{name: "defaults", options: RuntimeOptions{}},
		{name: "explicit small but above bound", options: RuntimeOptions{RootEventLimit: 5, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1}},
		{name: "equal to bound", options: RuntimeOptions{RootEventLimit: 4, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1}, reject: true},
		{name: "above the old bound but not counting the default queue", options: RuntimeOptions{RootEventLimit: 4, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1}, reject: true},
		{name: "small limit with default casts", options: RuntimeOptions{RootEventLimit: 64}, reject: true},
		{name: "default limit with raised casts", options: RuntimeOptions{MaxActiveCasts: 8192}, reject: true},
		{name: "default limit with raised queue", options: RuntimeOptions{MaxQueuedTasks: 4096}, reject: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			runtime, recovered := newRuntimeRecover(nil, testCase.options)
			if !testCase.reject {
				if recovered != nil || runtime == nil {
					t.Fatalf("NewRuntime(%+v) panicked: %v", testCase.options, recovered)
				}
				return
			}
			if recovered == nil {
				t.Fatalf("NewRuntime accepted RootEventLimit %d with MaxActiveCasts %d / MaxOwnedSpawns %d / MaxStopPendingSpawns %d; a full root table where every root is referenced must be impossible under a legal configuration",
					testCase.options.RootEventLimit, testCase.options.MaxActiveCasts, testCase.options.MaxOwnedSpawns, testCase.options.MaxStopPendingSpawns)
			}
			err, ok := recovered.(error)
			if !ok || !errors.Is(err, ErrRuntimeLimitsInvalid) {
				t.Fatalf("panic value = %#v, want an error wrapping ErrRuntimeLimitsInvalid", recovered)
			}
			// 每个选项连同补齐默认值之后的当前值一起出现。
			effective := newRuntimeCore(nil, testCase.options).options
			for option, value := range map[string]int{"RootEventLimit": effective.RootEventLimit, "MaxActiveCasts": effective.MaxActiveCasts, "MaxOwnedSpawns": effective.MaxOwnedSpawns, "MaxStopPendingSpawns": effective.MaxStopPendingSpawns, "MaxQueuedTasks": effective.MaxQueuedTasks} {
				if want := option + " (" + strconv.Itoa(value) + ")"; !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestRestoreRuntimeRejectsCheckpointWhoseRootEventLimitIsNotAboveTheBound(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	checkpoint, err := runtime.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint.Payload, &fields); err != nil {
		t.Fatal(err)
	}
	fields["root_event_limit"] = json.RawMessage("64")
	if checkpoint.Payload, err = json.Marshal(fields); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(checkpoint.Payload)
	checkpoint.Checksum = hex.EncodeToString(digest[:])
	resolver := ProgramResolverFunc(func(string, string) (*Program, error) { return program, nil })
	restored, err := RestoreRuntime(host, RuntimeOptions{}, checkpoint, resolver)
	if err == nil || restored != nil {
		t.Fatalf("RestoreRuntime accepted root_event_limit 64 with max_active_casts 4096 (err = %v)", err)
	}
	if !errors.Is(err, ErrRuntimeLimitsInvalid) || !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("restore error = %v, want ErrRuntimeLimitsInvalid and ErrCheckpointCorrupt", err)
	}
	if !strings.Contains(err.Error(), "RootEventLimit") || !strings.Contains(err.Error(), "MaxActiveCasts") || !strings.Contains(err.Error(), "MaxQueuedTasks") {
		t.Fatalf("restore error %q does not name the options", err)
	}
}
