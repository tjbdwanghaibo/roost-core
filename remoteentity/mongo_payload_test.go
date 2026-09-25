package remoteentity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoPayloadBatchingPreservesDeletesAndTransactionRollback(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	store := NewMongoCommitter(db, "control", 7, 0)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	var commits []entity.RemoteCommit
	for i := range 2 {
		id, _ := entity.BuildEntityID(int64(1001+i), 198)
		c := authorityCommit(t, id, 1, testWriteGrant(t, store, id))
		c.Mutations = append(c.Mutations, entity.RemoteDataMutation{Database: "game", Collection: "inventory", ID: id, Version: 1, Data: []byte("item")})
		commits = append(commits, c)
	}
	// 后一个集合失败，前一个集合和元数据 CAS 必须一起回滚。
	failure := errors.New("inventory unavailable")
	db.Collection("game", "inventory").Errors["BulkWrite"] = failure
	if _, err := store.CommitRemoteBatch(ctx, commits); !errors.Is(err, failure) {
		t.Fatalf("failure=%v", err)
	}
	for _, c := range commits {
		var doc bson.M
		if err := db.Collection("game", "players").FindOne(ctx, bson.M{"_id": c.EntityID}, &doc); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("partial payload: %v", err)
		}
		if current, err := store.readAuthority(ctx, c.EntityID); err != nil || current.Version != 0 {
			t.Fatalf("partial CAS: %+v %v", current, err)
		}
	}
	delete(db.Collection("game", "inventory").Errors, "BulkWrite")
	before := db.Collection("game", "players").Calls["BulkWrite"]
	if _, err := store.CommitRemoteBatch(ctx, commits); err != nil {
		t.Fatal(err)
	}
	if got := db.Collection("game", "players").Calls["BulkWrite"] - before; got != 1 {
		t.Fatalf("two entities require one collection write, got %d", got)
	}
	// 删除实体仍走同一事务，不能漏掉批量计划中的删除操作。
	c := commits[0].Clone()
	c.TransactionID[15] = 2
	c.BaseVersion = 1
	c.NextVersion = 2
	c.LockFence = testWriteGrant(t, store, c.EntityID).Fence
	c.Delete = true
	c.Mutations = nil
	c.Deletes = []entity.RemoteDataDelete{{Database: "game", Collection: "players", ID: c.EntityID}}
	if _, err := store.CommitRemote(ctx, c); err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err := db.Collection("game", "players").FindOne(ctx, bson.M{"_id": c.EntityID}, &doc); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("replace/delete order: %v", err)
	}
}
