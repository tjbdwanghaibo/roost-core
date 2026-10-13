// 记录编解码（codec7）：头部 / Put / Patch / Delete / Remote 提交往返、无界字段与截断拒绝、旧格式与未知格式拒绝且不消费日志。

package nestwal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 条目上限与截断检查保证坏记录不能越过编解码边界。

func TestEncodeRejectsUnboundedUnsetPathsAndHeaders(t *testing.T) {
	record := canonicalRecord(dataengine.MutationPatch)
	unset := make([]string, maxEntryCount+1)
	for i := range unset {
		unset[i] = "f" + strconv.Itoa(i)
	}
	record.Mutations[0].Patch.Unset = unset
	if _, err := encodeRecord(record); err == nil || !strings.Contains(err.Error(), "too many unset paths") {
		t.Fatalf("unset paths over the limit: err=%v", err)
	}

	record = canonicalRecord(dataengine.MutationPut)
	headers := make(map[string]string, maxEntryCount+1)
	for i := 0; i <= maxEntryCount; i++ {
		headers[strconv.Itoa(i)] = ""
	}
	record.Effects[0].Headers = headers
	if _, err := encodeRecord(record); err == nil || !strings.Contains(err.Error(), "too many effect headers") {
		t.Fatalf("headers over the limit: err=%v", err)
	}
}

// 每一个可能的截断点都必须以错误结束：读侧任何一个字段吞掉 EOF 继续，都会
// 把半条记录当成完整记录回放。
func TestDecodeRejectsEveryTruncationPoint(t *testing.T) {
	raw, err := encodeRecord(canonicalRecord(dataengine.MutationPut))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecord(raw); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(raw); n++ {
		if _, err := decodeRecord(raw[:n]); err == nil {
			t.Fatalf("%d/%d bytes decoded without error", n, len(raw))
		}
	}
}

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
		Durability: corenest.DurabilityPipelined.Record(),
		Mutations:  []corenest.EntityMutation{mutation},
		Effects: []corenest.Effect{{
			ID: "effect-1", Topic: "hero.changed", Key: "7", Payload: []byte{1, 2},
			Headers: map[string]string{"z": "last", "a": "first"}, AvailableAt: 456,
		}},
		Receipts: []dataengine.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{3}, Payload: []byte{4}, ExpiresAt: 789}},
	}
}

func TestCodecPatchRoundTrip(t *testing.T) {
	want := canonicalRecord(dataengine.MutationPatch)
	raw, err := encodeRecord(want)
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

func TestCodecDeleteCarriesNoPayload(t *testing.T) {
	want := canonicalRecord(dataengine.MutationDelete)
	want.Effects = nil
	want.Receipts = nil
	raw, err := encodeRecord(want)
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

func TestCodecRejectsUnknownRecordVersion(t *testing.T) {
	raw, err := encodeRecord(canonicalRecord(dataengine.MutationPatch))
	if err != nil {
		t.Fatal(err)
	}
	raw[0], raw[1] = 0xff, 0xff
	if _, err := decodeRecord(raw); !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("err=%v, want ErrUnsupportedRecordVersion", err)
	}
}

func TestWALDefaultWriterReplaysPatch(t *testing.T) {
	opts := testOptions(t.TempDir())
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

func TestRemoteCommitCodecRoundTrip(t *testing.T) {
	const kind entity.EntityKind = 125
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	entityID, err := entity.BuildEntityID(123, kind)
	if err != nil {
		t.Fatal(err)
	}
	var txID corenest.TransactionID
	txID[15] = 9
	remoteID := entity.RemoteTransactionID(txID)
	commit := entity.RemoteCommit{
		TransactionID: remoteID, EntityID: entityID, Kind: kind,
		BaseVersion: 4, NextVersion: 5, MarkerEpoch: 2, LockFence: 8, RouteEpoch: 3,
		Schema: 7, Codec: 1, Checksum: 44,
		Mutations: []entity.RemoteDataMutation{{Database: "game", DatabaseScope: 1, Collection: "players", ID: entityID, Version: 5, Mask: 3, Data: []byte("entity")}},
		Snapshots: []entity.RemoteSnapshotRecord{{
			Key:         entity.RemoteSnapshotKey{EntityID: entityID, Kind: kind, Scope: 2},
			BaseVersion: 4, StateVersion: 5, MarkerEpoch: 2, RouteEpoch: 3,
			Schema: 9, Codec: 1, Full: true, Data: []byte("snapshot"), Checksum: entity.RemoteSnapshotChecksum([]byte("snapshot")),
		}},
		Invalidations: []entity.RemoteSnapshotKey{{EntityID: entityID, Kind: kind, Scope: 3}},
	}
	record := corenest.CommitRecord{ID: txID, Durability: corenest.DurabilityStrict.Record(), Mutations: []corenest.EntityMutation{{Key: dataengine.DocumentKey{ID: entityID, Resource: "remote_entity"}, Kind: dataengine.MutationPut, ExpectedVersion: 4, NextVersion: 5, Codec: "remote", Remote: &commit}}}
	raw, err := encodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Mutations) != 1 || decoded.Mutations[0].Remote == nil {
		t.Fatalf("decoded=%+v", decoded)
	}
	got := decoded.Mutations[0].Remote
	if got.LockFence != 8 || got.RouteEpoch != 3 || len(got.Mutations) != 1 || string(got.Mutations[0].Data) != "entity" || len(got.Snapshots) != 1 || string(got.Snapshots[0].Data) != "snapshot" || len(got.Invalidations) != 1 || got.Invalidations[0].Scope != 3 {
		t.Fatalf("remote=%+v", got)
	}
}

func TestRemoteDeleteCommitCodecRoundTrip(t *testing.T) {
	const kind entity.EntityKind = 197
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	entityID, err := entity.BuildEntityID(124, kind)
	if err != nil {
		t.Fatal(err)
	}
	var txID corenest.TransactionID
	txID[15] = 10
	commit := entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID(txID), EntityID: entityID, Kind: kind, Delete: true,
		BaseVersion: 5, NextVersion: 6, MarkerEpoch: 2, LockFence: 8, RouteEpoch: 3,
		Deletes:       []entity.RemoteDataDelete{{Database: "game", DatabaseScope: 1, Collection: "players", ID: entityID}},
		Invalidations: []entity.RemoteSnapshotKey{{EntityID: entityID, Kind: kind, Scope: 1}},
	}
	record := corenest.CommitRecord{ID: txID, Durability: corenest.DurabilityStrict.Record(), Mutations: []corenest.EntityMutation{{Key: dataengine.DocumentKey{ID: entityID, Resource: "remote_entity"}, Kind: dataengine.MutationDelete, ExpectedVersion: 5, NextVersion: 6, Codec: "remote", Remote: &commit}}}
	raw, err := encodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Mutations[0].Remote
	if got == nil || len(got.Deletes) != 1 || got.Deletes[0].Collection != "players" || got.Deletes[0].ID != entityID {
		t.Fatalf("remote delete=%+v", got)
	}
}

func FuzzDecodeRemoteCommitRecord(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 3})
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = decodeRecord(raw)
	})
}

// 旧格式与未知格式必须明确拒绝（ErrCorrupt + ErrUnsupportedRecordVersion），保留原始日志，不消费也不推进确认点。
func TestRetiredFormatsLeaveWALUntouched(t *testing.T) {
	for _, version := range []uint16{5, 6, 0xffff} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := t.TempDir()
			payload, err := encodeRecord(canonicalRecord(dataengine.MutationPut))
			if err != nil {
				t.Fatal(err)
			}
			binary.BigEndian.PutUint16(payload, version)
			raw := encodeFrame(payload)
			path := filepath.Join(dir, segmentName(1))
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			w, err := Open(testOptions(dir))
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close(context.Background())
			called := false
			err = w.Replay(context.Background(), func(corenest.CommitFence, corenest.CommitRecord) error { called = true; return nil })
			if !errors.Is(err, ErrUnsupportedRecordVersion) || !errors.Is(err, ErrCorrupt) {
				t.Fatalf("codec %d: want unsupported version, got %v", version, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if called || w.checkpoint.fence != (corenest.CommitFence{}) || !bytes.Equal(raw, after) {
				t.Fatal("old WAL was consumed or modified")
			}
		})
	}
}

func TestCodecRoundTrip(t *testing.T) {
	want := testRecord(7, corenest.DurabilityStrict)
	raw, err := encodeRecord(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || got.Handler != want.Handler || got.RequestID != want.RequestID || got.Durability != want.Durability {
		t.Fatalf("record header mismatch: got=%+v want=%+v", got, want)
	}
	if len(got.Mutations) != 1 || string(got.Mutations[0].Data) != string(want.Mutations[0].Data) {
		t.Fatalf("mutation mismatch: %+v", got.Mutations)
	}
	if len(got.Effects) != 1 || got.Effects[0].Headers["trace"] != "abc" {
		t.Fatalf("effect mismatch: %+v", got.Effects)
	}
}
