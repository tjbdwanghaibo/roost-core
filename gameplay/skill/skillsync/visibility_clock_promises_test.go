package skillsync

import (
	"encoding/json"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

// RR-20261007-07：拒绝 VisibilityClock 必须同时约束快照、增量与线包 Header；
// 未识别的 mutation 没有审核过的字段投影，不得沿缺省放行分支泄漏。
func TestVisibilityClockAndUnknownMutationFailClosed(t *testing.T) {
	observer := syncstream.Observer{ID: 7}
	policy := EntityVisibilityPolicy{
		Visible: func(syncstream.Observer, skill.EntityID) (bool, error) { return true, nil },
		FieldVisible: func(_ syncstream.Observer, field VisibilityField, _ string) (bool, error) {
			return field != VisibilityClock, nil
		},
	}
	projector, _ := NewProjector(1)
	mutation, allowed, err := policy.FilterStateMutation(observer, skill.StateMutation{Kind: skill.StateMutationCastRemove, Sequence: 1, Caster: 1, Tick: 12, WorldRevision: 21})
	if err != nil || !allowed {
		t.Fatalf("visible mutation rejected: %v", err)
	}
	packet, err := projector.StateDeltaPacket(observer, 1, mutation)
	if err != nil {
		t.Fatal(err)
	}
	var state StateRecord
	if err := json.Unmarshal(packet.Payload, &state); err != nil {
		t.Fatal(err)
	}
	if state.Header.Tick != 0 || state.Header.WorldRevision != 0 || state.Delta.Tick != 0 || state.Delta.WorldRevision != 0 {
		t.Error("state delta or header leaked denied clock")
	}
	event, allowed, err := policy.FilterPresentation(observer, skill.PresentationEvent{Kind: skill.PresentationCast, Sequence: 2, Source: 1, Tick: 12, WorldRevision: 21})
	if err != nil || !allowed {
		t.Fatalf("visible presentation rejected: %v", err)
	}
	packet, err = projector.PresentationPacket(observer, 1, event)
	if err != nil {
		t.Fatal(err)
	}
	var presentation PresentationRecord
	if err := json.Unmarshal(packet.Payload, &presentation); err != nil {
		t.Fatal(err)
	}
	if presentation.Header.Tick != 0 || presentation.Header.WorldRevision != 0 || presentation.Event.Tick != 0 || presentation.Event.WorldRevision != 0 {
		t.Error("presentation or header leaked denied clock")
	}
	if _, allowed, err := policy.FilterStateMutation(observer, skill.StateMutation{Kind: "future_kind", Sequence: 3}); err != nil || allowed {
		t.Errorf("unknown mutation allowed=%v err=%v", allowed, err)
	}
	runtime, _ := visibilityRuntime(t, skill.RuntimeOptions{})
	if err := runtime.Advance(12); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(CoordinatorOptions{Runtime: runtime, History: syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1}), Publisher: &recordingPublisher{}, Projector: projector, Visibility: policy})
	if err != nil {
		t.Fatal(err)
	}
	reset, err := coordinator.presentationReset(observer)
	if err != nil || reset.Tick != 0 || reset.WorldRevision != 0 {
		t.Errorf("presentation reset leaked denied clock: %+v %v", reset, err)
	}
}
