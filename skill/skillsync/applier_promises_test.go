package skillsync

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
	"github.com/tjbdwanghaibo/roost-core/skill"
)

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
