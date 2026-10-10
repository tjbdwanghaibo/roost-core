package skillsync

import (
	"encoding/json"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"testing"
)

func TestRejectedFullPacketDoesNotWedgeTheApplier(t *testing.T) {
	observer := syncstream.Observer{ID: 3}
	consumer := &recordingConsumer{}
	applier, err := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, State: consumer})
	if err != nil {
		t.Fatal(err)
	}
	projector, _ := NewProjector(1)
	for _, step := range []struct {
		name  string
		epoch uint64
	}{{"first epoch", 5}, {"epoch switch", 6}} {
		malformed, _ := projector.StateSnapshotPacket(observer, 1, skill.RuntimeStateSnapshot{})
		malformed.Epoch, malformed.Sequence, malformed.BaseSequence = step.epoch, 10, 9
		if _, err := applier.Apply(malformed); !errors.Is(err, ErrPacketShape) {
			t.Fatalf("%s: malformed full = %v, want ErrPacketShape", step.name, err)
		}
		valid, _ := projector.StateSnapshotPacket(observer, 1, skill.RuntimeStateSnapshot{})
		valid.Epoch, valid.Sequence = step.epoch, 11
		result, err := applier.Apply(valid)
		if err != nil || !result.Applied || applier.Epoch() != step.epoch {
			t.Fatalf("%s: valid full after a rejected one = %+v, %v (epoch %d); the rejected packet wedged the applier", step.name, result, err, applier.Epoch())
		}
	}
	if consumer.snapshots != 2 {
		t.Fatalf("snapshots applied = %d, want 2", consumer.snapshots)
	}
}

func TestApplierRefusesConcurrentApplyStatesAndMisshapenRecords(t *testing.T) {
	observer := syncstream.Observer{ID: 3, Scope: "match"}
	manifest, _ := mintedManifest(t, observer)
	fresh := func(t *testing.T) *Applier {
		t.Helper()
		consumer := &recordingConsumer{}
		applier, err := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, Manifest: consumer, State: consumer, Presentation: consumer})
		if err != nil {
			t.Fatal(err)
		}
		return applier
	}
	stateStream := syncstream.Stream{Topic: TopicState, Key: 11}
	packet := func(topic string, full bool, body any) syncstream.Packet {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		p := syncstream.Packet{Observer: observer, Stream: syncstream.Stream{Topic: topic, Key: 11}, Epoch: manifest.Epoch, Sequence: 1, SchemaVersion: 1, Full: full, Payload: raw}
		return p
	}

	pending := fresh(t)
	pending.pendingEpoch = manifest.Epoch
	if _, err := pending.Apply(manifest.Clone()); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("Apply while an epoch switch is pending = %v", err)
	}
	busy := fresh(t)
	busy.inflight[manifest.Stream] = struct{}{}
	if _, err := busy.Apply(manifest.Clone()); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("Apply while the same stream is in flight = %v", err)
	}
	switching := fresh(t)
	switching.epoch = manifest.Epoch + 1
	switching.inflight[stateStream] = struct{}{}
	if _, err := switching.Apply(manifest.Clone()); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("Apply of a new epoch while another stream is in flight = %v", err)
	}

	shapes := fresh(t)
	if _, err := shapes.Apply(manifest.Clone()); err != nil {
		t.Fatal(err)
	}
	if _, err := shapes.Apply(packet(TopicState, true, StateRecord{Header: Header{SchemaVersion: 1, Kind: RecordStateFull}})); !errors.Is(err, ErrPacketShape) {
		t.Fatalf("full state record without a snapshot = %v", err)
	}
	if _, err := shapes.Apply(packet(TopicState, false, StateRecord{Header: Header{SchemaVersion: 1, Kind: RecordStateDelta}})); !errors.Is(err, ErrPacketShape) {
		t.Fatalf("delta state record without a delta = %v", err)
	}
	if _, err := shapes.Apply(packet(TopicPresentation, true, PresentationRecord{Header: Header{SchemaVersion: 1, Kind: RecordPresentationReset}})); !errors.Is(err, ErrPacketShape) {
		t.Fatalf("presentation reset without a reset = %v", err)
	}
	if _, err := shapes.Apply(packet(TopicPresentation, true, PresentationRecord{Header: Header{SchemaVersion: 1, Kind: RecordPresentationReset}, Reset: &PresentationReset{}})); err != nil {
		t.Fatalf("well-formed presentation reset refused: %v", err)
	}
}
