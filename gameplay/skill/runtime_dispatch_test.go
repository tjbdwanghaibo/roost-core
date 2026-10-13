package skill

import (
	"errors"
	"strings"
	"testing"
)

type fixedPassiveRouter struct{ candidates []PassiveCandidate }

func (router fixedPassiveRouter) Candidates(EventContext) []PassiveCandidate {
	return append([]PassiveCandidate(nil), router.candidates...)
}

func TestQueueExternalEventRoutesWithoutRecursiveActivation(t *testing.T) {
	program, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `[]`, `[]`))
	host := runtimeTestHost(environment)
	router := fixedPassiveRouter{candidates: []PassiveCandidate{{Program: program, Owner: 1, Ability: 9}}}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: router})
	event := EventContext{EventID: 40, RootEventID: 40, Tick: 0, WorldRevision: host.CurrentRevision(), Owner: 1, Source: 2}
	if err := runtime.QueueExternalEvent(event); err != nil {
		t.Fatal(err)
	}
	if runtime.CastCount() != 0 {
		t.Fatal("external event activated recursively")
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("queued event = %v casts=%d", err, runtime.CastCount())
	}
}

func TestRouterCandidatesAreNormalizedBeforeEnqueue(t *testing.T) {
	base, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `[]`, `[]`))
	otherJSON := strings.Replace(passiveSkillJSON(2, `[]`, `[]`), "skill.test.passive", "skill.test.passive.other", 1)
	other, diagnostics := Compile(mustParseJSON(t, otherJSON), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	router := fixedPassiveRouter{candidates: []PassiveCandidate{
		{Program: other, Owner: 2, Ability: 2},
		{Program: base, Owner: 1, Ability: 3},
		{Program: other, Owner: 1, Ability: 1},
	}}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: router})
	event := EventContext{EventID: 50, RootEventID: 50, Tick: 0, WorldRevision: host.CurrentRevision(), Owner: 1, Source: 3}
	if err := runtime.QueueExternalEvent(event); err != nil || runtime.Advance(0) != nil {
		t.Fatal("dispatch failed")
	}
	activated := make([]RuntimeEvent, 0)
	for _, item := range runtime.RuntimeEvents() {
		if item.Kind == "passive_activated" {
			activated = append(activated, item)
		}
	}
	if len(activated) != 3 || activated[0].Entity != 1 || activated[0].Context.SkillID != other.id || activated[1].Entity != 1 || activated[1].Context.SkillID != base.id || activated[2].Entity != 2 {
		t.Fatalf("activation order = %#v", activated)
	}
}

// Host 事件派发给被动路由时出错，事件流不能停住，扣过费的施法不能被删：派发里的拒绝（Host 能力表覆盖不了的被动
// Program、根事件表满）是这个候选或这个事件被动路由的永久结果，记一条 passive_suppressed（或 event_dispatch_dropped）、
// 计指标、写一条 Warn，事件照常前进、已入队的候选不重复入队；付费之后不再有能让施法失败的派发步骤，付了费的施法照常运行。
// 历史：RR-20261006-55。

// noSummonTableHost 如实声明“没有召唤物”：带 summon 的被动 Program 在准入处被拒。
type noSummonTableHost struct{ *MemoryHost }

func (host *noSummonTableHost) HostCapabilities() HostCapabilityTable {
	table := host.MemoryHost.HostCapabilities()
	table.Summon = false
	return table
}

// eventPassiveRouter 只路由一个事件 ID。
type eventPassiveRouter struct {
	event      EventID
	candidates []PassiveCandidate
}

func (router eventPassiveRouter) Candidates(event EventContext) []PassiveCandidate {
	if event.EventID != router.event {
		return nil
	}
	return append([]PassiveCandidate(nil), router.candidates...)
}

func uncoveredPassiveProgram(t *testing.T) *Program {
	t.Helper()
	activation := `{"type":"passive_on_damaged","cooldown_scope":"caster","event_filter":{"required_tags":[],"excluded_tags":[],"elements":[],"damage_types":[],"results":[]},"proc_policy":{"max_depth":2,"allow_self_trigger":false,"once_per_root_event":false}}`
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10}},{"flow":"finish"}]}`
	program, _ := compileRuntimeJSON(t, spawnLifecycleSkill("uncovered", activation, flow))
	return program
}

func TestUncoverablePassiveCandidateDoesNotStallTheEventStream(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &noSummonTableHost{MemoryHost: runtimeTestHost(environment)}
	uncovered := uncoveredPassiveProgram(t)
	covered, _ := compileRuntimeJSON(t, passiveSkillJSON(2, `[]`, `[]`))
	router := eventPassiveRouter{event: 70, candidates: []PassiveCandidate{{Program: covered, Owner: 1}, {Program: uncovered, Owner: 1}}}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: router})
	// 一个带 EventID 的 Host 事件：伤害结算。
	host.mutex.Lock()
	host.appendContextEventLocked("damage_resolved", 2, 0, EventContext{EventID: 70, RootEventID: 70, Owner: 1, Source: 2})
	host.mutex.Unlock()
	for tick := Tick(1); tick <= 4; tick++ {
		if err := runtime.Advance(tick); err != nil {
			t.Errorf("Advance(%d) = %v; a candidate the host cannot cover must not stall the runtime", tick, err)
		}
	}
	if runtime.eventCursor == 0 {
		t.Fatal("event cursor did not move past the dispatched event")
	}
	activated, suppressed := 0, 0
	for _, event := range runtime.RuntimeEvents() {
		switch {
		case event.Kind == "passive_activated" && event.Context.SkillID == covered.id:
			activated++
		case event.Kind == "passive_suppressed" && event.Context.SkillID == uncovered.id && event.Context.Result == "host_capability":
			suppressed++
		}
	}
	if activated != 1 || suppressed != 1 {
		t.Fatalf("covered candidate activated %d times, uncovered suppressed %d times; want 1 and 1 (one event, dispatched once)", activated, suppressed)
	}
	if count := runtime.rootEventCounts[70]; count != 1 {
		t.Fatalf("root 70 counted %d times, want 1 (no re-dispatch)", count)
	}
}

func TestCostPaymentDispatchFailureDoesNotDropAPaidCast(t *testing.T) {
	environment := DefaultCompileEnvironment()
	host := &noSummonTableHost{MemoryHost: runtimeTestHost(environment)}
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}`
	program, diagnostics := Compile(mustParseJSON(t, hostCapabilitySkill("paid", `[{"resource":"mana","amount":10}]`, flow)), environment)
	requireNoErrors(t, diagnostics)
	router := fixedPassiveRouter{candidates: []PassiveCandidate{{Program: uncoveredPassiveProgram(t), Owner: 1}}}
	runtime := NewRuntime(host, RuntimeOptions{PassiveRouter: router})
	castID, err := runtime.Activate(program, CastInput{Caster: 1})
	if err != nil {
		t.Errorf("Activate = %v; dispatching the cost event must not fail a cast that already paid", err)
	}
	if mana := host.ResourceForTest(1, "mana"); mana != 90 {
		t.Errorf("mana = %d, want 90 (paid once)", mana)
	}
	if cast, ok := runtime.InspectCast(castID); !ok || cast.Status == CastFailed {
		t.Fatalf("cast = %#v found=%v, want the paid cast kept and running to completion", cast, ok)
	}
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil && !errors.Is(err, ErrCasterBusy) {
		t.Fatalf("second Activate = %v", err)
	}
}

func TestDerivedEventPreservesRootAndSetsParent(t *testing.T) {
	root := newRootEvent(1)
	child := deriveEvent(root, 2)
	grandchild := deriveEvent(child, 3)
	if grandchild.RootEventID != root.EventID || grandchild.ParentEventID != child.EventID {
		t.Fatalf("broken causal identity: %#v", grandchild)
	}
}

func TestEventContextCopiesSortsAndDeduplicatesTags(t *testing.T) {
	tags := []GameplayTagHandle{3, 1, 3, 2}
	event := newRootEvent(1).WithGameplayTags(tags)
	tags[0] = 99
	want := []GameplayTagHandle{1, 2, 3}
	got := event.GameplayTags()
	if len(got) != len(want) {
		t.Fatalf("tags = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("tags = %v", got)
		}
	}
	got[0] = 99
	if event.GameplayTags()[0] != 1 {
		t.Fatal("event exposed mutable tag storage")
	}
}

// 根事件表满且每个根都被排程任务钉住时（施法 / 衍生物钉住的根由 rootEventReferenceBound 保证放得下），跳过这个事件的
// 被动路由、计 skill.root_event.capacity_dropped.total、写一条 Error 日志，事件照常前进、被压缩，后面的事件照常派发。
// 合法配置下排队任务有上限（MaxQueuedTasks），走不到这个兜底分支（TestLegalConfigurationNeverReachesTheRootTableFallback）；
// 兜底仍保留作防御：这里绕过入口准入直接往排程里塞外部事件任务，模拟“排队任务超出上限”这种不该发生的状态。
func TestCollectHostEventsSkipsEventWhenEveryRootIsPinned(t *testing.T) {
	host := NewMemoryHostWithOptions(AuthorityIdentity{}, MemoryHostOptions{CompactEvents: true})
	// 合法配置：RootEventLimit 5 > MaxActiveCasts 1 + MaxOwnedSpawns 1 + MaxStopPendingSpawns 1 + MaxQueuedTasks 1。
	runtime := NewRuntime(host, RuntimeOptions{RootEventLimit: 5, MaxActiveCasts: 1, MaxOwnedSpawns: 1, MaxStopPendingSpawns: 1, MaxQueuedTasks: 1})
	for root := EventID(1); root <= 5; root++ {
		// 之后 tick 的外部事件钉住根 1～5（queuedTaskRoot），宿主事件把它们记进根事件表。绕过 QueueExternalEvent 的准入。
		if err := runtime.scheduleSystem(50, &externalEventTask{Event: EventContext{EventID: 100 + root, RootEventID: root, Tick: 50}}); err != nil {
			t.Fatal(err)
		}
	}
	host.mutex.Lock()
	for root := EventID(1); root <= 5; root++ {
		host.appendContextEventLocked("pinned_root", 0, 0, EventContext{EventID: root, RootEventID: root})
	}
	host.appendContextEventLocked("capacity_probe", 0, 0, EventContext{EventID: 6, RootEventID: 6})
	host.appendContextEventLocked("after_probe", 0, 0, EventContext{EventID: 7, RootEventID: 1})
	host.mutex.Unlock()
	dropped := counterValue(MetricRootEventCapacityDropped)

	runtime.collectHostEvents()
	if runtime.eventCursor != 7 {
		t.Fatalf("event cursor = %d, want 7 (past the skipped event and the one after it); a full root table must not stall the event stream", runtime.eventCursor)
	}
	if events := host.Events(0); len(events) != 0 {
		t.Fatalf("dispatched events were not compacted: %+v", events)
	}
	if got := counterValue(MetricRootEventCapacityDropped) - dropped; got != 1 {
		t.Fatalf("%s grew by %d, want 1", MetricRootEventCapacityDropped, got)
	}
	if _, tracked := runtime.rootEventCounts[6]; tracked {
		t.Fatal("the skipped event's root was tracked")
	}
	if got := runtime.rootEventCounts[1]; got != 2 {
		t.Fatalf("root 1 count = %d, want 2 (the event after the skipped one was dispatched)", got)
	}
}

func TestPassiveActivationQueuesThenAppliesFilterAndOncePerRootLedger(t *testing.T) {
	program, environment := compileRuntimeJSON(t, passiveSkillJSON(2, `["spell"]`, `[]`))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	event := EventContext{EventID: 10, RootEventID: 10, Tick: 0, Owner: 1, Source: 2}.WithGameplayTags([]GameplayTagHandle{1})
	id, err := runtime.ActivatePassive(program, event)
	if err != nil || id == 0 || runtime.CastCount() != 0 {
		t.Fatalf("enqueue = %d %v casts=%d", id, err, runtime.CastCount())
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("activation = %v casts=%d", err, runtime.CastCount())
	}
	if _, err := runtime.ActivatePassive(program, event); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("once-per-root failed: %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "once_per_root")

	missingTag := EventContext{EventID: 11, RootEventID: 11, Owner: 1, Source: 2}
	if _, err := runtime.ActivatePassive(program, missingTag); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("filter failed: %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "filter")
}

func TestPassiveSelfTriggerAndDepthAreBounded(t *testing.T) {
	program, environment := compileRuntimeJSON(t, passiveSkillJSON(1, `[]`, `[]`))
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	self := EventContext{EventID: 20, RootEventID: 20, Owner: 1, Source: 1, SkillID: program.id}
	if _, err := runtime.ActivatePassive(program, self); err != nil {
		t.Fatal(err)
	}
	depth := EventContext{EventID: 21, RootEventID: 21, Owner: 1, Source: 2, ProcDepth: 1}
	if _, err := runtime.ActivatePassive(program, depth); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 0 {
		t.Fatalf("bounded proc = %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "self_trigger")
	assertSuppressionReason(t, runtime.RuntimeEvents(), "max_depth")
}

func TestPassiveActivationPerTickLimitSuppressesWithoutCreatingCast(t *testing.T) {
	input := strings.Replace(passiveSkillJSON(2, `[]`, `[]`), `"once_per_root_event":true`, `"once_per_root_event":false`, 1)
	program, environment := compileRuntimeJSON(t, input)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MaxPassiveActivationsPerTick: 1})
	for id := EventID(30); id < 32; id++ {
		if _, err := runtime.ActivatePassive(program, EventContext{EventID: id, RootEventID: id, Owner: 1, Source: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Advance(0); err != nil || runtime.CastCount() != 1 {
		t.Fatalf("tick bound = %v casts=%d", err, runtime.CastCount())
	}
	assertSuppressionReason(t, runtime.RuntimeEvents(), "max_activations_per_tick")
}

func assertSuppressionReason(t *testing.T, events []RuntimeEvent, reason string) {
	t.Helper()
	for _, event := range events {
		if event.Kind == "passive_suppressed" && event.Context.Result == reason {
			return
		}
	}
	t.Fatalf("missing suppression %q in %#v", reason, events)
}
