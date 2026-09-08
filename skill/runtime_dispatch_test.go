package skill

import (
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
