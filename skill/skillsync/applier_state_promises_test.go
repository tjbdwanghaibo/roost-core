package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

// U-0148 · C2 · gap map core `skill/skillsync` 6/20：准入在纪元切换进行中 / 同流在途 / 有在途时切纪元
// 三种状态下报 ErrApplyInProgress；状态与呈现记录的形状（全量却无快照、增量却无 delta、重置却无
// reset）报 ErrPacketShape。前三条只在另一次 Apply 尚未提交时可观察，这里直接注入内部状态。
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
