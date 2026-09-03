package nestwal

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

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
	record := corenest.CommitRecord{ID: txID, Durability: corenest.DurabilityStrict, Mutations: []corenest.EntityMutation{{EntityID: entityID, Resource: "remote_entity", Version: 5, Codec: "remote", Remote: &commit}}}
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
	record := corenest.CommitRecord{ID: txID, Durability: corenest.DurabilityStrict, Mutations: []corenest.EntityMutation{{EntityID: entityID, Resource: "remote_entity", Version: 6, Codec: "remote", Remote: &commit}}}
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
