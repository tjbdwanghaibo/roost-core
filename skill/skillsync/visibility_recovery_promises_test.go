package skillsync

// RR-20261005-NC-114：presentation reset（游标过期的 Flush 与 Recover 两条路径）必须和增量 presentation 一样经过
// observer 的 VisibilityPolicy。旧实现把 Runtime.PresentationSnapshot() 原样投影：不可见施法者的持续表现、
// 其目标与坐标都发给了本应看不到它的 observer。
//
// RR-20261005-NC-115：同一份状态经快照下发与经增量下发必须得到相同的可见集合。旧实现三处口径不一：
// ability 快照按空 handle 问 FieldVisible、增量按具体 handle 问；cast / spawn remove 不带归属实体、
// persistent remove 不检查 Binding，三类 remove 对不可见实体一律放行。

import (
	"encoding/json"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/skill"
	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

const visibleWaitSkill = `{"schema":"roost.skill/v2","id":"skill.test.sync.visible_wait","name":"Visible Wait","description":"A cast with a continuing cast visual.","presentation":{"icon_keywords":["flare","blade","spark"],"cast":{"category":"cast","theme":"default","elements":["default"]}},"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"wait","ticks":5,"then":{"flow":"finish"}}}}]}`

func visibilityRuntime(t *testing.T, options skill.RuntimeOptions) (*skill.Runtime, *skill.Program) {
	t.Helper()
	environment := skill.DefaultCompileEnvironment()
	definition, err := skill.Parse([]byte(visibleWaitSkill))
	if err != nil {
		t.Fatal(err)
	}
	program, diagnostics := skill.Compile(definition, environment)
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == skill.DiagnosticError {
			t.Fatalf("compile: %+v", diagnostic)
		}
	}
	host := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	for _, id := range []skill.EntityID{1, 2} {
		host.UpsertEntity(skill.MemoryEntity{ID: id, Alive: true, Health: 100, MaxHealth: 100})
	}
	return skill.NewRuntime(host, options), program
}

// hideEntityOne 是观察者看不到实体 1 的策略。
var hideEntityOne = EntityVisibilityPolicy{Visible: func(_ syncstream.Observer, entity skill.EntityID) (bool, error) { return entity != 1, nil }, RedactSpatial: true}

func publishedResets(t *testing.T, packets []syncstream.Packet) []skill.ActivePresentation {
	t.Helper()
	var active []skill.ActivePresentation
	found := false
	for _, packet := range packets {
		if packet.Stream.Topic != TopicPresentation {
			continue
		}
		var record PresentationRecord
		if err := json.Unmarshal(packet.Payload, &record); err != nil {
			t.Fatal(err)
		}
		if record.Reset != nil {
			found = true
			active = append(active, record.Reset.Recovery.Active...)
		}
	}
	if !found {
		t.Fatal("no presentation reset was published")
	}
	return active
}

func assertResetHidesEntityOne(t *testing.T, path string, active []skill.ActivePresentation) {
	t.Helper()
	for _, entry := range active {
		if entry.Anchor.Source == 1 || entry.Anchor.Target == 1 || entry.Anchor.Position != nil || entry.Anchor.Direction != nil {
			t.Errorf("%s reset leaked to an observer that cannot see entity 1: %+v", path, entry)
		}
	}
}

func TestPresentationResetFromRecoverHonoursVisibility(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	if _, err := runtime.Start(program, skill.CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1}), Publisher: publisher, Projector: projector, Visibility: hideEntityOne})
	if err != nil {
		t.Fatal(err)
	}
	observer := syncstream.Observer{ID: 7}
	if _, err := coordinator.Recover(syncstream.ResyncRequest{Observer: observer, Stream: syncstream.Stream{Topic: TopicPresentation, Key: 1}, SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	assertResetHidesEntityOne(t, "Recover", publishedResets(t, publisher.packets))
}

func TestPresentationResetFromExpiredCursorHonoursVisibility(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{PresentationLimit: 1})
	if _, err := runtime.Start(program, skill.CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(program, skill.CastInput{Caster: 2, Target: 1}); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	projector, _ := NewProjector(1)
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1}), Publisher: publisher, Projector: projector, Visibility: hideEntityOne})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Flush(syncstream.Observer{ID: 7}, 1); err != nil {
		t.Fatal(err)
	}
	active := publishedResets(t, publisher.packets)
	assertResetHidesEntityOne(t, "expired-cursor Flush", active)
	if len(active) != 1 || active[0].Anchor.Source != 2 {
		t.Errorf("reset = %+v, want only entity 2's cast (its target redacted)", active)
	}
}

func TestStateSnapshotAndDeltasHideTheSameAbility(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{})
	policy := EntityVisibilityPolicy{
		Visible: func(syncstream.Observer, skill.EntityID) (bool, error) { return true, nil },
		FieldVisible: func(_ syncstream.Observer, field VisibilityField, handle string) (bool, error) {
			return !(field == VisibilityAbilities && handle == "5"), nil
		},
	}
	if err := runtime.RegisterAbility(skill.AbilityRegistration{Owner: 1, Handle: 5, Program: program}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range runtime.StateDeltas(0, 0).Mutations {
		if _, allowed, err := policy.FilterStateMutation(syncstream.Observer{}, mutation); err != nil || allowed {
			t.Fatalf("delta %s handle %d allowed=%v err=%v; the policy hides ability 5", mutation.Kind, mutation.AbilityHandle, allowed, err)
		}
	}
	snapshot, err := policy.FilterStateSnapshot(syncstream.Observer{}, runtime.StateSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Abilities) != 0 {
		t.Errorf("snapshot abilities = %+v: deltas hide ability 5 but the snapshot sends it", snapshot.Abilities)
	}
}

func TestRemoveMutationsOfInvisibleEntitiesAreFiltered(t *testing.T) {
	runtime, program := visibilityRuntime(t, skill.RuntimeOptions{CompletedCastLimit: 1})
	hidden, err := runtime.Start(program, skill.CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(5); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(program, skill.CastInput{Caster: 2, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Advance(10); err != nil {
		t.Fatal(err)
	}
	removed := false
	for _, mutation := range runtime.StateDeltas(0, 0).Mutations {
		if mutation.Kind != skill.StateMutationCastRemove || mutation.CastID != hidden {
			continue
		}
		removed = true
		if _, allowed, err := hideEntityOne.FilterStateMutation(syncstream.Observer{}, mutation); err != nil || allowed {
			t.Errorf("cast_remove of entity 1's cast %d allowed=%v err=%v; its upserts were filtered", hidden, allowed, err)
		}
	}
	if !removed {
		t.Fatalf("the retention bound did not evict cast %d; mutations=%+v", hidden, runtime.StateDeltas(0, 0).Mutations)
	}
	persistent := skill.StateMutation{Sequence: 1, Kind: skill.StateMutationPersistentRemove, StateHandle: skill.StateHandle{GameplayDigest: "g", Slot: 1}, Binding: skill.StateScopeBinding{Owner: 1, Subject: 1}}
	if out, allowed, err := hideEntityOne.FilterStateMutation(syncstream.Observer{}, persistent); err != nil || allowed {
		t.Errorf("persistent_remove bound to invisible entity 1 allowed=%v err=%v binding=%+v", allowed, err, out.Binding)
	}
}
