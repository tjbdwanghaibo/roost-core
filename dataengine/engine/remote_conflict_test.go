package engine

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestRemoteCreationConflictFencesProjectorAndKeepsWAL(t *testing.T) {
	ensureDataEngineRemoteRepositoryEntity()
	id, err := entity.BuildEntityID(9821, dataEngineRemoteRepositoryKind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, heroes := newMongoStoreTest(t)
	remote := remoteentity.NewMongoCommitter(client, "remote", 3, 0)
	if err := store.SetRemoteProjection(remote, &mongoRemoteProjectionFake{}); err != nil {
		t.Fatal(err)
	}
	if err := client.Collection("remote", "_remote_entity_meta").Seed(bson.M{"_id": id, "_ver": int64(1), "_marker_epoch": int64(1), "_route_epoch": int64(1), "_lock_fence": int64(0)}); err != nil {
		t.Fatal(err)
	}
	record := projectorRecord(42, false)
	commit := entity.RemoteCommit{TransactionID: entity.RemoteTransactionID(record.ID), EntityID: id, Kind: dataEngineRemoteRepositoryKind, BaseVersion: 0, NextVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1}
	commit.Mutations = []entity.RemoteDataMutation{{Database: "game", Collection: "guild", ID: id, Version: 1, Data: []byte("new state")}}
	record.Mutations = append(record.Mutations, coredata.Mutation{
		Key: coredata.DocumentKey{Resource: "remote_entity", ID: id}, Kind: coredata.MutationPut,
		ExpectedVersion: 0, NextVersion: 1, Schema: 1, Codec: "remote", Remote: &commit,
	})
	p, wal := stoppedProjectorWithRecords(t, store, []coredata.CommitRecord{record}, 0)
	// OnFatal 异步投递（RR-20260926-17 复核补修）；用带缓冲 channel 计数。
	failures := make(chan error, 2)
	p.opts.OnFatal = func(err error) { failures <- err }
	for range 2 {
		count, err := p.ReplayPass(context.Background())
		if count != 0 || !errors.Is(err, ErrProjectionConflict) || !errors.Is(err, fmongo.ErrVersionConflict) {
			t.Fatalf("count=%d err=%v", count, err)
		}
	}
	awaitChan(t, failures, "the fatal notification")
	if len(failures) != 0 {
		t.Fatalf("fatal notified %d extra times, want once", len(failures))
	}
	if err := p.Commit(context.Background(), projectorRecord(43, false)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("admitted after fatal conflict: %v", err)
	}
	if heroes.Len() != 0 {
		t.Fatal("ordinary mutation escaped failed remote transaction")
	}
	assertWALReplayCount(t, wal, 1)
}
