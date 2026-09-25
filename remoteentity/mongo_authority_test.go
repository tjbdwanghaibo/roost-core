package remoteentity

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func authorityCommit(t *testing.T, id int64, tx byte, grant WriteGrant) entity.RemoteCommit {
	t.Helper()
	const kind entity.EntityKind = 198
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	return entity.RemoteCommit{TransactionID: entity.RemoteTransactionID{15: tx}, EntityID: id, Kind: kind,
		BaseVersion: uint64(grant.Version), NextVersion: uint64(grant.Version) + 1, MarkerEpoch: grant.Ownership.MarkerEpoch, RouteEpoch: grant.Ownership.RouteEpoch, LockFence: grant.Fence, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: "game", Collection: "players", ID: id, Version: uint64(grant.Version) + 1, Data: []byte("new")}}}
}
func TestMongoAuthorityFencesUncommittedWriterAndReplaysCommittedTransaction(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	s := NewMongoCommitter(db, "control", 7, 0)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, _ := entity.BuildEntityID(996, 198)
	owner, err := s.ClaimOwnership(ctx, id, 7)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.GrantWrite(ctx, id, "old", 7)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.GrantWrite(ctx, id, "new", 7)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Fence <= old.Fence || newer.Version != 0 {
		t.Fatalf("grant=%+v old=%+v", newer, old)
	}
	if _, err = s.CommitRemote(ctx, authorityCommit(t, id, 1, old)); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("stale grant committed: %v", err)
	}
	current := authorityCommit(t, id, 2, newer)
	receipt, err := s.CommitRemote(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.GrantWrite(ctx, id, "third", 7)
	if err != nil || grant.Version != 1 {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	replay, err := s.CommitRemote(ctx, current)
	if err != nil || receipt != replay {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = s.TransferExpected(ctx, id, owner, 8); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitRemote(ctx, authorityCommit(t, id, 3, grant)); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("transferred writer committed: %v", err)
	}
	if _, err = s.GrantWrite(ctx, id, "wrong-owner", 7); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("old owner admitted: %v", err)
	}
	// 新创建的 committer 同样必须严格比较，无需调用开关。
	another := NewMongoCommitter(db, "control", 7, 0)
	forged := authorityCommit(t, id, 4, grant)
	forged.MarkerEpoch = 100
	if _, err = another.CommitRemote(ctx, forged); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("authority bypass: %v", err)
	}
}
func TestMongoAuthoritySharedTransitionsAndOverflow(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	s := NewMongoCommitter(db, "control", 7, 0)
	owner, err := s.ClaimOwnership(ctx, 101, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantWrite(ctx, 101, "shared-before-enter", 0); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatal(err)
	}
	shared, err := s.EnterSharedExpected(ctx, 101, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantWrite(ctx, 101, "owner-in-shared", 7); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatal(err)
	}
	grant, err := s.GrantWrite(ctx, 101, "shared", 0)
	if err != nil || grant.Ownership != shared {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if _, err = s.LeaveSharedExpected(ctx, 101, owner); err == nil {
		t.Fatal("stale transition succeeded")
	}
	if _, err = s.LeaveSharedExpected(ctx, 101, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Collection("control", remoteMetaCollection).UpdateOne(ctx, bson.M{"_id": 101}, bson.M{"$set": bson.M{"_grant_fence": int64(math.MaxInt64)}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GrantWrite(ctx, 101, "overflow", 7); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("overflow=%v", err)
	}
}
func TestMongoAuthorityRejectsUnsupportedMetadata(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	store := NewMongoCommitter(db, "control", 7, 0)
	if _, err := db.Collection("control", remoteMetaCollection).InsertOne(ctx, bson.M{"_id": 101, "_ver": int64(3)}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureRemoteStorage(ctx); !errors.Is(err, ErrRemoteAuthorityInvalid) {
		t.Fatalf("startup=%v", err)
	}
	if _, err := store.ClaimOwnership(ctx, 101, 7); !errors.Is(err, ErrRemoteAuthorityInvalid) {
		t.Fatalf("claim=%v", err)
	}
}

// 模拟 majority 写成功但回复丢失，不把失败伪装成未执行。
type authorityLostReplyMongo struct{ fmongo.IMongo }
type authorityLostReplyDB struct{ fmongo.IDatabase }
type authorityLostReplyCollection struct{ fmongo.ICollection }

func (m authorityLostReplyMongo) Database(name string) fmongo.IDatabase {
	return authorityLostReplyDB{m.IMongo.Database(name)}
}
func (d authorityLostReplyDB) Collection(name string) fmongo.ICollection {
	c := d.IDatabase.Collection(name)
	if name == remoteMetaCollection {
		return authorityLostReplyCollection{c}
	}
	return c
}
func (c authorityLostReplyCollection) FindOneAndUpdate(ctx context.Context, filter, update, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	if err := c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...); err != nil {
		return err
	}
	return errors.New("majority acknowledged but response lost")
}
func TestMongoAuthorityLostGrantReplyDoesNotAllocateTwice(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	s := NewMongoCommitter(authorityLostReplyMongo{db}, "control", 7, 0)
	if _, err := s.ClaimOwnership(ctx, 101, 7); err != nil {
		t.Fatal(err)
	}
	grant, err := s.GrantWrite(ctx, 101, "one-attempt", 7)
	if err != nil || grant.Fence != 1 {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
	if got := db.Collection("control", remoteMetaCollection).Calls["FindOneAndUpdate"]; got != 1 {
		t.Fatalf("write attempts=%d", got)
	}
}

func TestOwnerRoutedAdmissionRechecksDurableOwnerDespiteHotCache(t *testing.T) {
	ctx := context.Background()
	const kind entity.EntityKind = 198
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 1000, 0)
	cfg := DefaultConfig()
	cfg.MarkerCacheTTL = time.Minute
	manager := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.StopFinalizer(ctx); err != nil {
			t.Error(err)
		}
	})
	loader := newRemoteTestLoader()
	live := newTestRemoteEntity(1003, 1, kind)
	loader.add(live)
	backend, err := NewBackend(loader, store)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetBackend(backend)
	manager.SetOwnershipStore(store)
	for want := uint64(1); want <= 2; want++ {
		batch, err := manager.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		if got := live.RemoteVersionVector().LockFence; got != want {
			t.Fatalf("owner write fence=%d want %d", got, want)
		}
		if err := batch.Abort(ctx, errors.New("abort probe")); err != nil {
			t.Fatal(err)
		}
		if err := batch.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	owner, _, err := store.GetOwnership(ctx, live.GUId())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransferExpected(ctx, live.GUId(), owner, 2000); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()}); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("cached old owner admitted: %v", err)
	}
}

func TestMongoAuthorityBatchCASRollsBackEveryEntityOnOneStaleGrant(t *testing.T) {
	ctx := context.Background()
	db := newRemoteMongoFake()
	store := NewMongoCommitter(db, "control", 7, 0)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	var commits []entity.RemoteCommit
	for i := range 2 {
		id, _ := entity.BuildEntityID(int64(1004+i), 198)
		if _, err := store.ClaimOwnership(ctx, id, 7); err != nil {
			t.Fatal(err)
		}
		grant, err := store.GrantWrite(ctx, id, generateToken(), 7)
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, authorityCommit(t, id, 5, grant))
	}
	second := commits[1].EntityID
	newer, err := store.GrantWrite(ctx, second, generateToken(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRemoteBatch(ctx, commits); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("partial CAS accepted: %v", err)
	}
	first, err := store.readAuthority(ctx, commits[0].EntityID)
	if err != nil || first.Version != 0 {
		t.Fatalf("first CAS was not rolled back: %+v %v", first, err)
	}
	commits[1] = authorityCommit(t, second, 5, newer)
	before := db.Collection("control", remoteMetaCollection).Calls["BulkWrite"]
	if _, err := store.CommitRemoteBatch(ctx, commits); err != nil {
		t.Fatal(err)
	}
	if calls := db.Collection("control", remoteMetaCollection).Calls["BulkWrite"] - before; calls != 1 {
		t.Fatalf("metadata commands=%d", calls)
	}
	for _, commit := range commits {
		current, err := store.readAuthority(ctx, commit.EntityID)
		if err != nil || current.Version != 1 {
			t.Fatalf("entity not committed: %+v %v", current, err)
		}
	}
}

func TestMongoCommitterRejectsWriteWithoutGrantByDefault(t *testing.T) {
	ctx := context.Background()
	store := NewMongoCommitter(newRemoteMongoFake(), "control", 7, 0)
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, _ := entity.BuildEntityID(1099, 198)
	commit := authorityCommit(t, id, 99, WriteGrant{Ownership: entity.RemoteEntityMarkerLease{OwnerSid: 7, MarkerEpoch: 1, RouteEpoch: 1}})
	if _, err := store.CommitRemote(ctx, commit); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("write without ownership or grant accepted: %v", err)
	}
}

// 所有存储用例也走正式准入，不能靠 committer 的弱校验模式伪造许可。
func testWriteGrant(t *testing.T, store *MongoCommitter, id int64) WriteGrant {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ClaimOwnership(ctx, id, 7); err != nil {
		t.Fatal(err)
	}
	grant, err := store.GrantWrite(ctx, id, generateToken(), 7)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestSharedAdmissionUsesOwnershipFromDurableGrant(t *testing.T) {
	ctx := context.Background()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	db := newRemoteMongoFake()
	store := NewMongoCommitter(db, "control", 7, 0)
	live := newTestRemoteEntity(1100, 1, 198)
	owner, err := store.ClaimOwnership(ctx, live.GUId(), 7)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := store.EnterSharedExpected(ctx, live.GUId(), owner)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.MarkerCacheTTL = time.Minute
	r := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub()}
	mgr := NewManager(NewVersionedLockFactory(r, store), cfg, 7)
	loader := newRemoteTestLoader()
	loader.add(live)
	backend, err := NewBackend(loader, store)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetBackend(backend)
	mgr.SetOwnershipStore(store)
	t.Cleanup(func() {
		if err := mgr.StopFinalizer(ctx); err != nil {
			t.Error(err)
		}
	})
	meta := db.Collection("control", remoteMetaCollection)
	for i := range 2 {
		before := meta.Calls["FindOne"]
		batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
		if err != nil {
			t.Fatal(err)
		}
		entry := batch.(*remoteWriteBatch).entries[0]
		if entry.lease.MarkerEpoch != shared.MarkerEpoch || entry.lease.RouteEpoch != shared.RouteEpoch || entry.lease.LockFence != uint64(i+1) {
			t.Fatalf("lease=%+v", entry.lease)
		}
		wantReads := 0
		if i == 0 {
			wantReads = 1
		}
		if reads := meta.Calls["FindOne"] - before; reads != wantReads {
			t.Fatalf("ownership reads=%d want=%d", reads, wantReads)
		}
		lock := entry.wrapper.rMu.(*versionedLock)
		if grant, ok := lock.writeGrant(); !ok || grant.Fence != entry.lease.LockFence {
			t.Fatalf("grant=%+v ok=%v", grant, ok)
		}
		if err := batch.Abort(ctx, errors.New("probe")); err != nil {
			t.Fatal(err)
		}
		if err := batch.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := lock.writeGrant(); ok {
			t.Fatal("closed lock returned a usable grant")
		}
	}
}
