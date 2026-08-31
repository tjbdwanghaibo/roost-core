package nestwal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func canonicalRecord(kind dataengine.MutationKind) corenest.CommitRecord {
	var id corenest.TransactionID
	id[15] = 91
	mutation := corenest.EntityMutation{
		Key:             dataengine.DocumentKey{Database: "game", Scope: dataengine.DatabaseServer, Resource: "heroes", ID: 7},
		Kind:            kind,
		ExpectedVersion: 4,
		NextVersion:     5,
		Mask:            3,
		Schema:          2,
		Codec:           "bson-v2",
	}
	switch kind {
	case dataengine.MutationPut:
		mutation.Data = []byte{5, 0, 0, 0, 0}
	case dataengine.MutationPatch:
		mutation.Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: int32(9)}})
		mutation.Patch.Unset = []string{"title"}
	}
	return corenest.CommitRecord{
		ID:         id,
		Handler:    "hero.level_up",
		RequestID:  "request-91",
		CreatedAt:  123,
		Durability: corenest.DurabilityPipelined,
		Mutations:  []corenest.EntityMutation{mutation},
		Effects: []corenest.Effect{{
			ID: "effect-1", Topic: "hero.changed", Key: "7", Payload: []byte{1, 2},
			Headers: map[string]string{"z": "last", "a": "first"}, AvailableAt: 456,
		}},
		Receipts: []dataengine.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{3}, Payload: []byte{4}, ExpiresAt: 789}},
	}
}

func TestCodecV1DecodesLegacyFullAsCanonicalPut(t *testing.T) {
	record := canonicalRecord(dataengine.MutationPut)
	record.Effects[0].AvailableAt = 0
	record.Receipts = nil
	raw, err := encodeRecordVersion(record, WriterVersionV1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutation := got.Mutations[0]
	if mutation.Kind != dataengine.MutationPut || mutation.Key != record.Mutations[0].Key || mutation.ExpectedVersion != 4 || mutation.NextVersion != 5 {
		t.Fatalf("decoded v1 mutation=%+v", mutation)
	}
	if mutation.EntityID != 0 || mutation.Resource != "" || mutation.Version != 0 {
		t.Fatalf("v1 compatibility fields escaped reader: %+v", mutation)
	}
}

func TestCodecV2PatchRoundTrip(t *testing.T) {
	want := canonicalRecord(dataengine.MutationPatch)
	raw, err := encodeRecordVersion(want, WriterVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mutations) != 1 || got.Mutations[0].Kind != dataengine.MutationPatch || string(got.Mutations[0].Patch.SetBSON) != string(want.Mutations[0].Patch.SetBSON) {
		t.Fatalf("patch round trip=%+v", got.Mutations)
	}
	if len(got.Mutations[0].Patch.Unset) != 1 || got.Mutations[0].Patch.Unset[0] != "title" {
		t.Fatalf("unset=%v", got.Mutations[0].Patch.Unset)
	}
	if got.Effects[0].AvailableAt != 456 || len(got.Receipts) != 1 || got.Receipts[0].ExpiresAt != 789 || string(got.Receipts[0].Digest) != string([]byte{3}) {
		t.Fatalf("effect/receipt round trip: effects=%+v receipts=%+v", got.Effects, got.Receipts)
	}
}

func TestCodecV2DeleteCarriesNoPayload(t *testing.T) {
	want := canonicalRecord(dataengine.MutationDelete)
	want.Effects = nil
	want.Receipts = nil
	raw, err := encodeRecordVersion(want, WriterVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	mutation := got.Mutations[0]
	if mutation.Kind != dataengine.MutationDelete || len(mutation.Data) != 0 || !mutation.Patch.Empty() {
		t.Fatalf("delete=%+v", mutation)
	}
}

func TestCodecV1RejectsV2OnlyRecord(t *testing.T) {
	if _, err := encodeRecordVersion(canonicalRecord(dataengine.MutationPatch), WriterVersionV1); !errors.Is(err, ErrWriterVersionUnsupported) {
		t.Fatalf("err=%v, want ErrWriterVersionUnsupported", err)
	}
}

func TestCodecRejectsUnknownRecordVersion(t *testing.T) {
	raw, err := encodeRecordVersion(canonicalRecord(dataengine.MutationPatch), WriterVersionV2)
	if err != nil {
		t.Fatal(err)
	}
	raw[0], raw[1] = 0xff, 0xff
	if _, err := decodeRecord(raw); !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("err=%v, want ErrUnsupportedRecordVersion", err)
	}
}

func TestWALWriterVersionV2ReplaysPatch(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.WriterVersion = WriterVersionV2
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	if _, err := w.Append(context.Background(), canonicalRecord(dataengine.MutationPatch)); err != nil {
		t.Fatal(err)
	}
	var got corenest.CommitRecord
	if err := w.Replay(context.Background(), func(_ corenest.CommitFence, record corenest.CommitRecord) error {
		got = record
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got.Mutations) != 1 || got.Mutations[0].Kind != dataengine.MutationPatch {
		t.Fatalf("replayed=%+v", got)
	}
}

func TestCodecUnknownVersionDoesNotAdvanceCheckpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, segmentName(1)), encodeFrame([]byte{0xff, 0xff}), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(testOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	called := false
	err = w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("replay err=%v", err)
	}
	if called || w.checkpoint.fence != (corenest.CommitFence{}) {
		t.Fatalf("called=%v checkpoint=%+v", called, w.checkpoint.fence)
	}
}
