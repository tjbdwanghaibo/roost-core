package remoteentity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-kit/internal/mongofake"
)

func newRemoteMongoFake() *mongofake.Client { return mongofake.NewClient() }

// lookupTxField reads one field of a stored transaction document, so the test
// asserts what the committer actually persisted rather than a fake's bookkeeping.
func lookupTxField(t *testing.T, coll *mongofake.Collection, id string, field string) (any, bool) {
	t.Helper()
	doc, ok := coll.Lookup(id)
	if !ok {
		return nil, false
	}
	value, present := doc[field]
	return value, present
}

func TestMongoCommitterCASIdempotencySnapshotAndOutbox(t *testing.T) {
	const kind entity.EntityKind = 198
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(991, kind)
	if err != nil {
		t.Fatal(err)
	}
	var tx entity.RemoteTransactionID
	tx[15] = 1
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	data := []byte("state")
	commit := entity.RemoteCommit{
		TransactionID: tx, EntityID: id, Kind: kind, BaseVersion: 0, NextVersion: 1,
		MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "players", ID: id, Version: 1, Data: data}},
		Snapshots: []entity.RemoteSnapshotRecord{{Key: key, BaseVersion: 0, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data)}},
	}
	mongo := newRemoteMongoFake()
	store := NewMongoCommitter(mongo, "control", 7, 0)
	if err := store.EnsureRemoteStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	txCollection := mongo.Collection("control", remoteTxCollection)
	if len(txCollection.Indexes) != 2 || !txCollection.Indexes[1].Sparse || !txCollection.Indexes[1].RecreateOnConflict || txCollection.Indexes[1].TTL <= 0 {
		t.Fatalf("unsafe transaction TTL indexes: %+v", txCollection.Indexes)
	}
	first, err := store.CommitRemote(context.Background(), commit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CommitRemote(context.Background(), commit)
	if err != nil || second != first {
		t.Fatalf("idempotent receipt=%+v first=%+v err=%v", second, first, err)
	}
	altered := commit.Clone()
	altered.Mutations[0].Data = []byte("different")
	if _, err := store.CommitRemote(context.Background(), altered); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("transaction id content collision error=%v", err)
	}
	pending, err := store.PendingRemoteCommits(context.Background(), 10)
	if err != nil || len(pending) != 1 || len(pending[0].Commits) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	_, expiresBeforePublish := lookupTxField(t, txCollection, tx.String(), "expires_at")
	if expiresBeforePublish {
		t.Fatal("unpublished outbox transaction must not expire")
	}
	snapshot, ok, err := store.LoadRemoteSnapshot(context.Background(), key, entity.RemoteReadLinearizable, 1)
	if err != nil || !ok || string(snapshot.Payload.BytesCopy()) != "state" {
		t.Fatalf("snapshot=%+v ok=%v err=%v", snapshot, ok, err)
	}
	if err := store.MarkRemoteCommitPublished(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	_, expires := lookupTxField(t, txCollection, tx.String(), "expires_at")
	if !expires {
		t.Fatal("published transaction must receive expires_at")
	}
	pending, err = store.PendingRemoteCommits(context.Background(), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after ack=%+v err=%v", pending, err)
	}

	stale := commit.Clone()
	stale.TransactionID[14] = 2
	if _, err := store.CommitRemote(context.Background(), stale); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("stale error=%v", err)
	}
	duplicate := commit.Clone()
	duplicate.TransactionID[13] = 3
	if _, err := store.CommitRemoteBatch(context.Background(), []entity.RemoteCommit{duplicate, duplicate}); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("duplicate entity batch error=%v", err)
	}
}
