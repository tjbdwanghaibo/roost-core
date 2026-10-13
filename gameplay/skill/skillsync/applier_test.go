package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
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

func mintedManifest(t *testing.T, observer syncstream.Observer) (syncstream.Packet, *syncstream.History) {
	t.Helper()
	projector, _ := NewProjector(1)
	history := syncstream.NewHistory(syncstream.HistoryOptions{SchemaVersion: 1})
	plan := skill.PresentationPlan{Identity: skill.ProgramIdentityView{PresentationDigest: "visual-v1"}}
	manifest, err := projector.ManifestPacket(observer, 11, plan)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = history.Append(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, history
}

// NewApplier refuses a schema it cannot serve; a zero schema would make
// every packet's version check vacuous.
func TestNewApplierRefusesUnservableSchemas(t *testing.T) {
	if _, err := NewApplier(ApplierOptions{}); !errors.Is(err, ErrSchemaVersionRequired) {
		t.Fatalf("schema 0 = %v", err)
	}
	if _, err := NewApplier(ApplierOptions{SchemaVersion: 3, SupportedSchema: SchemaRange{Min: 1, Max: 2}}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("schema outside the supported range = %v", err)
	}
}

// Admission is the applier's contract with the transport: every packet that
// could corrupt the replica's view is refused before any consumer sees it.
// Each rule is pinned with a packet that is valid except for that rule.
func TestApplierAdmissionRefusesEachMalformedPacket(t *testing.T) {
	observer := syncstream.Observer{ID: 3, Scope: "match"}
	manifest, _ := mintedManifest(t, observer)
	fresh := func(t *testing.T) *Applier {
		t.Helper()
		consumer := &recordingConsumer{}
		applier, err := NewApplier(ApplierOptions{Observer: observer, SchemaVersion: 1, SupportedSchema: SchemaRange{Min: 1, Max: 2}, Manifest: consumer, State: consumer, Presentation: consumer})
		if err != nil {
			t.Fatal(err)
		}
		return applier
	}
	cases := []struct {
		name   string
		mutate func(*syncstream.Packet)
		want   error
	}{
		{"epoch zero", func(p *syncstream.Packet) { p.Epoch = 0 }, ErrEpochMismatch},
		{"schema outside the supported range", func(p *syncstream.Packet) { p.SchemaVersion = 9 }, ErrSchemaMismatch},
		{"supported schema but no migrator", func(p *syncstream.Packet) { p.SchemaVersion = 2 }, ErrSchemaMigratorRequired},
		{"blank topic", func(p *syncstream.Packet) { p.Stream.Topic = "" }, ErrPacketShape},
		{"zero sequence", func(p *syncstream.Packet) { p.Sequence = 0 }, ErrPacketShape},
		{"first packet is a delta", func(p *syncstream.Packet) { p.Full = false }, ErrEpochMismatch},
		{"full packet with a base sequence", func(p *syncstream.Packet) { p.BaseSequence = 1 }, ErrPacketShape},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			packet := manifest.Clone()
			tc.mutate(&packet)
			if _, err := fresh(t).Apply(packet); !errors.Is(err, tc.want) {
				t.Fatalf("Apply = %v, want %v", err, tc.want)
			}
		})
	}
	// The valid packet is admitted by the same applier configuration.
	if result, err := fresh(t).Apply(manifest.Clone()); err != nil || !result.Applied {
		t.Fatalf("valid manifest = %#v, %v", result, err)
	}
}

// The record inside the packet must agree with the packet: same schema, a
// manifest topic carries a manifest record, and the manifest's presentation
// digest matches the plan it describes.
func TestApplierRecordRefusesHeaderAndDigestDisagreement(t *testing.T) {
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
	rewrite := func(t *testing.T, packet syncstream.Packet, mutate func(map[string]any)) syncstream.Packet {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal(packet.Payload, &body); err != nil {
			t.Fatal(err)
		}
		mutate(body)
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		packet.Payload = raw
		return packet
	}
	schema := rewrite(t, manifest.Clone(), func(b map[string]any) { b["schema_version"] = 2 })
	if _, err := fresh(t).Apply(schema); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("record schema disagrees with packet = %v", err)
	}
	kind := rewrite(t, manifest.Clone(), func(b map[string]any) { b["kind"] = string(RecordStateFull) })
	if _, err := fresh(t).Apply(kind); !errors.Is(err, ErrPacketShape) {
		t.Fatalf("manifest topic with a state record = %v", err)
	}
	digest := rewrite(t, manifest.Clone(), func(b map[string]any) { b["presentation_digest"] = "someone-else" })
	if _, err := fresh(t).Apply(digest); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("manifest digest disagreeing with its plan = %v", err)
	}
}
