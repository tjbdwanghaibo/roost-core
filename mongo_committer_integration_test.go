//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	kitmongo "github.com/tjbdwanghaibo/roost-kit/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// realCommitter builds a MongoCommitter on the isolated replica set the
// integration environment provides (scripts/integration/dataengine-env.sh).
func realCommitter(t *testing.T) (*MongoCommitter, fmongo.IMongo, string) {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Fatal("ROOST_DATAENGINE_IT_MONGO_URI is unset")
	}
	cfg := viper.New()
	cfg.Set("mongo.uri", uri)
	cfg.Set("mongo.require_replica_set", true)
	cfg.Set("mongo.connect_timeout", 5*time.Second)
	cfg.Set("mongo.transaction_timeout", 15*time.Second)
	mod := kitmongo.NewMongoMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(app.NewRegistry(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	client := mod.Client()
	database := fmt.Sprintf("roost_it_remote_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Database(database).Drop(ctx)
		mod.Stop()
	})
	committer := NewMongoCommitter(client, database, 7, 0)
	if err := committer.EnsureRemoteStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	return committer, client, database
}

func fencedCommit(tx byte, id int64, kind entity.EntityKind, base uint64, fence uint64, data string) entity.RemoteCommit {
	var txID entity.RemoteTransactionID
	txID[15] = tx
	return entity.RemoteCommit{
		TransactionID: txID, EntityID: id, Kind: kind, BaseVersion: base, NextVersion: base + 1,
		MarkerEpoch: 1, RouteEpoch: 1, LockFence: fence, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "players", ID: id, Version: base + 1, Data: []byte(data)}},
	}
}

// FEATURE_LOGIC §4.2, fifth clause: two commits on one entity at the same base
// version, one carrying a higher lock fence than the other, race on a real
// replica set. At most one lands; the other is told ErrRemoteVersionConflict and
// takes the existing isolation path. Afterwards a commit that presents the
// right version but a fence BELOW the stored one is also a version conflict —
// a fence lost to a newer owner is never a silent no-op — while the current
// fence proceeds. The predicate that guarantees this is applyCommit's filter
// (`_ver` == base AND `_lock_fence` <= commit fence); until this test the only
// evidence for it was the unit fake.
func TestRealCompetingFencedCommitsAdmitAtMostOne(t *testing.T) {
	committer, client, database := realCommitter(t)
	ctx := context.Background()
	const kind entity.EntityKind = 197
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(992, kind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := committer.CommitRemote(ctx, fencedCommit(1, id, kind, 0, 5, "seed")); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	high := fencedCommit(2, id, kind, 1, 9, "high")
	low := fencedCommit(3, id, kind, 1, 3, "low")
	results := make([]error, 2)
	var wait sync.WaitGroup
	for i, commit := range []entity.RemoteCommit{high, low} {
		wait.Add(1)
		go func(index int, commit entity.RemoteCommit) {
			defer wait.Done()
			_, results[index] = committer.CommitRemote(ctx, commit)
		}(i, commit)
	}
	wait.Wait()
	winners := 0
	for index, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, entity.ErrRemoteVersionConflict):
			// applyCommit's filter missed (fmongo.ErrVersionConflict); the
			// committer reports it in the protocol's vocabulary.
		default:
			t.Fatalf("commit %d failed with %v, want nil or ErrRemoteVersionConflict", index, err)
		}
	}
	if winners != 1 {
		t.Fatalf("%d of two competing commits at one base version landed; want exactly one (results=%v)", winners, results)
	}
	var meta struct {
		Version uint64 `bson:"_ver"`
		Fence   uint64 `bson:"_lock_fence"`
	}
	if err := client.Database(database).Collection(remoteMetaCollection).FindOne(ctx, bson.M{"_id": id}, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != 2 {
		t.Fatalf("meta version %d after one winning commit, want 2", meta.Version)
	}
	winnerFence := high.LockFence
	if results[0] != nil {
		winnerFence = low.LockFence
	}
	if meta.Fence != winnerFence {
		t.Fatalf("stored fence %d, the winner carried %d", meta.Fence, winnerFence)
	}

	// The current version is not enough on its own: a fence below the stored
	// one is refused as a version conflict, and the current fence goes through.
	if _, err := committer.CommitRemote(ctx, fencedCommit(4, id, kind, 2, winnerFence-1, "stale-fence")); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("a lower fence at the right version returned %v, want ErrRemoteVersionConflict", err)
	}
	if _, err := committer.CommitRemote(ctx, fencedCommit(5, id, kind, 2, winnerFence, "current-fence")); err != nil {
		t.Fatalf("the current fence at the right version was refused: %v", err)
	}
}
