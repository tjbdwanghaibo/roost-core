package skill

// RR-20261006-55（F08-H+ H1）：Host 事件派发给被动路由时出错，事件流不能停住，扣过费的施法不能被删。
//
// 旧行为：collectHostEvents / drainHostEvents 先 dispatchEvent 再推进 eventCursor。dispatchEvent 出错（PassiveRouter
// 给出一个 Host 能力表覆盖不了的被动 Program，enqueuePassive 准入失败；或根事件表满）时 cursor 停在这个事件前，之后每次
// Advance 都在同一个事件上报同一个错，Runtime 永久卡住；同一事件里排在前面、已入队的候选每次重试再入队一次。施法付费路径
// payCostList 先 PayCosts 再 drainHostEvents：付费产生的事件派发失败时 cast 还没记 costsPaid，未提交的启动被删、ID 回收，
// 费用已扣不退——B3 ③“在扣费之前拒绝”被绕过。
//
// 承诺：派发里的拒绝是这个候选（或这个事件的被动路由）的永久结果，不会因为重试而变好——记一条 passive_suppressed
// （或 event_dispatch_dropped）、计指标、写一条 Warn，事件照常前进；付费之后不再有能让施法失败的派发步骤，付了费的
// 施法照常运行。

import (
	"errors"
	"testing"
)

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
