package skill

import (
	"reflect"
	"testing"
)

// RR-20261007-14：测试实际付费召唤、清理与回放，能力表相等不足以证明包装器完整。
func TestRecordingReplayPreserveOwnedSummonLifecycle(t *testing.T) {
	program := summonCostingProgram(t)
	inner := runtimeTestHost(DefaultCompileEnvironment())
	recording := NewRecordingHost(inner)
	runtime := NewRuntime(recording, RuntimeOptions{MatchSeed: fixedTestSeed(47)})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatalf("wrapped summon failed after payment: err=%v mana=%d", err, inner.ResourceForTest(1, "mana"))
	}
	if len(inner.OwnedEntities(1)) != 1 {
		t.Fatal("wrapped summon did not create owned entity")
	}
	if err := runtime.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if len(inner.OwnedEntities(1)) != 0 {
		t.Fatal("wrapped shutdown did not clean owned entity")
	}
	replay := NewReplayHost(inner.AuthorityIdentity(), recording.Records())
	restored := NewRuntime(replay, RuntimeOptions{MatchSeed: fixedTestSeed(47)})
	if _, err := restored.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := replay.AssertComplete(); err != nil {
		t.Fatal(err)
	}
}

type optionalProbeHost struct {
	*MemoryHost
	compacted                       EventCursor
	relations, snapshots, positions int
}

func (h *optionalProbeHost) CompactEventsThrough(cursor EventCursor) { h.compacted = cursor }
func (h *optionalProbeHost) AbilityOwnerRelation(EntityID, EntityID) (string, bool) {
	h.relations++
	return "ally", true
}
func (h *optionalProbeHost) SkillPersistentStateSnapshot() []PersistentStateSnapshot {
	h.snapshots++
	return []PersistentStateSnapshot{{}}
}
func (h *optionalProbeHost) ResolveInputPosition(InputPositionRequest) (Position, bool) {
	h.positions++
	return Position{X: 7, Y: 9}, true
}

func TestRecordingReplayForwardOptionalWorldViews(t *testing.T) {
	inner := &optionalProbeHost{MemoryHost: runtimeTestHost(DefaultCompileEnvironment())}
	recording := NewRecordingHost(inner)
	check := func(host Host) {
		t.Helper()
		host.(HostEventCompactor).CompactEventsThrough(42)
		if relation, ok := host.(AbilityRelationProvider).AbilityOwnerRelation(1, 2); !ok || relation != "ally" {
			t.Fatal("relation lost")
		}
		if states := host.(RuntimeStateExtensionProvider).SkillPersistentStateSnapshot(); len(states) != 1 {
			t.Fatal("persistent state lost")
		}
		if position, ok := host.(InputPositionResolver).ResolveInputPosition(InputPositionRequest{Caster: 1}); !ok || !reflect.DeepEqual(position, Position{X: 7, Y: 9}) {
			t.Fatal("resolved position lost")
		}
	}
	check(recording)
	if inner.compacted != 42 || inner.relations != 1 || inner.snapshots != 1 || inner.positions != 1 {
		t.Fatal("underlying optional methods not called")
	}
	replay := NewReplayHost(inner.AuthorityIdentity(), recording.Records())
	check(replay)
	if err := replay.AssertComplete(); err != nil {
		t.Fatal(err)
	}
}
