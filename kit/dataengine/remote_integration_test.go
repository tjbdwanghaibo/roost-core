//go:build integration

package dataengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 发布阶段无需装载 live Entity；意外进入业务装载直接报错，避免替身掩盖调用链变化。
type remoteProjectionLoader struct{}

func (remoteProjectionLoader) LoadRemoteEntity(context.Context, int64, entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	return nil, errors.New("unexpected entity load in projection test")
}

type remotePublicationProbe struct {
	fail      atomic.Bool
	published atomic.Int64
}

func (s *remotePublicationProbe) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	if s.fail.Load() {
		return errors.New("injected snapshot transport unavailable")
	}
	s.published.Add(1)
	return nil
}
func (*remotePublicationProbe) DeleteRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, uint64) error {
	return nil
}
func (*remotePublicationProbe) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

// 所有 L2 操作透传真实 Redis，仅前缀隔离并记录清理范围，不改写脚本或 CAS 结果。
type isolatedSnapshotRedis struct {
	fredis.IRedis
	t      *testing.T
	prefix string
	keys   sync.Map
}

func (r *isolatedSnapshotRedis) key(key string) string {
	key = r.prefix + key
	r.keys.Store(key, struct{}{})
	return key
}
func (r *isolatedSnapshotRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	scoped := make([]string, len(keys))
	for i, key := range keys {
		scoped[i] = r.key(key)
	}
	result, err := r.IRedis.Eval(ctx, script, scoped, args...)
	if err != nil {
		r.t.Errorf("real snapshot Redis Eval: %v", err)
	}
	return result, err
}
func (r *isolatedSnapshotRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	return r.IRedis.HGet(ctx, r.key(key), field)
}
func (r *isolatedSnapshotRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	scoped := make([]string, len(keys))
	for i, key := range keys {
		scoped[i] = r.key(key)
	}
	return r.IRedis.Del(ctx, scoped...)
}

const remoteProjectionKind entity.EntityKind = 239

var registerRemoteProjectionKind = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: remoteProjectionKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
})

func TestRealDataEngineRemotePublicationAndWALRecovery(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	addr := os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
	if addr == "" {
		t.Fatal("isolated Redis address required")
	}
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redis.Close() })
	scoped := &isolatedSnapshotRedis{IRedis: redis, t: t, prefix: fx.database + ":"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		scoped.keys.Range(func(key, value any) bool {
			if _, err := redis.Del(ctx, key.(string)); err != nil {
				t.Errorf("cleanup Redis: %v", err)
			}
			return true
		})
	})
	remoteStore := remoteentity.NewMongoCommitter(fx.mongo, fx.database, 901, 0)
	if err := remoteStore.EnsureRemoteStorage(fx.context()); err != nil {
		t.Fatal(err)
	}
	backend, err := remoteentity.NewBackend(remoteProjectionLoader{}, remoteStore)
	if err != nil {
		t.Fatal(err)
	}
	publisher := &remotePublicationProbe{}
	newManager := func() *remoteentity.Manager {
		cfg := remoteentity.DefaultConfig()
		cfg.TransactionTrackLimit = 2
		m := remoteentity.NewManager(remoteentity.NewVersionedLockFactory(redis), cfg, 901, remoteentity.NewSnapshotL2Store(scoped, time.Minute))
		m.SetBackend(backend)
		m.SetSyncer(publisher)
		t.Cleanup(func() { _ = m.StopFinalizer(context.Background()) })
		return m
	}
	manager := newManager()
	store, err := engine.NewMongoStore(fx.mongo, engine.MongoStoreConfig{DefaultDatabase: fx.database})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(remoteStore, manager); err != nil {
		t.Fatal(err)
	}
	registerRemoteProjectionKind()
	const kind = remoteProjectionKind
	ids := make([]int64, 2)
	for i := range ids {
		ids[i], err = entity.BuildEntityID(int64(7000+i), kind)
		if err != nil {
			t.Fatal(err)
		}
	}
	makeRecord := func(seq byte, base uint64) coredata.CommitRecord {
		r := realRecord(seq, []coredata.Mutation{
			realPut(t, fx.database, "local_wallet", 9001, base, base+1, bson.M{"value": base + 1}),
			realPut(t, fx.database, "local_inventory", 9001, base, base+1, bson.M{"value": base + 1}),
		})
		for _, id := range ids {
			if _, err := remoteStore.ClaimOwnership(fx.context(), id, 901); err != nil {
				t.Fatal(err)
			}
			grant, err := remoteStore.GrantWrite(fx.context(), id, fmt.Sprintf("%s/%d", r.ID, id), 901)
			if err != nil {
				t.Fatal(err)
			}
			commit := entity.RemoteCommit{TransactionID: entity.RemoteTransactionID(r.ID), EntityID: id, Kind: kind, BaseVersion: base, NextVersion: base + 1, MarkerEpoch: 1, RouteEpoch: 1, LockFence: grant.Fence, Schema: 1, Codec: 1}
			for i, coll := range []string{"remote_wallet", "remote_inventory"} {
				data := []byte(fmt.Sprintf("entity=%d collection=%s version=%d", id, coll, base+1))
				commit.Mutations = append(commit.Mutations, entity.RemoteDataMutation{Database: fx.database, Collection: coll, ID: id, Version: base + 1, Data: data})
				commit.Snapshots = append(commit.Snapshots, entity.RemoteSnapshotRecord{Key: entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: uint32(i + 1)}, BaseVersion: base, StateVersion: base + 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data)})
			}
			r.Mutations = append(r.Mutations, coredata.Mutation{Key: coredata.DocumentKey{Resource: "remote_entity", ID: id}, Kind: coredata.MutationPut, ExpectedVersion: base, NextVersion: base + 1, Schema: 1, Codec: "remote", Remote: &commit})
		}
		return r
	}
	verify := func(version uint64) {
		for _, coll := range []string{"local_wallet", "local_inventory"} {
			assertDocumentVersion(t, fx, coll, 9001, version)
		}
		for _, id := range ids {
			for _, coll := range []string{"remote_wallet", "remote_inventory"} {
				var doc struct {
					Version uint64 `bson:"_ver"`
					Data    []byte `bson:"data"`
				}
				if err := fx.mongo.Database(fx.database).Collection(coll).FindOne(fx.context(), bson.M{"_id": id}, &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Version != version || string(doc.Data) != fmt.Sprintf("entity=%d collection=%s version=%d", id, coll, version) {
					t.Fatalf("%s/%d doc=%+v", coll, id, doc)
				}
			}
		}
	}
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wal.Close(context.Background()) }()
	p := manualRealProjector(t, wal, store)
	first := makeRecord(151, 0)
	if _, err = wal.Append(fx.context(), first); err != nil {
		t.Fatal(err)
	}
	publisher.fail.Store(true)
	if n, err := p.ReplayPass(fx.context()); n != 0 || !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("publication failure n=%d err=%v", n, err)
	}
	verify(1)
	assertWALReplayCount(t, wal, 1)
	pending, err := remoteStore.PendingRemoteCommits(fx.context(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if p.Stats().FatalProjectionConflicts != 0 {
		t.Fatal("transient publication failure fenced projector")
	}
	if err = wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	if err = manager.StopFinalizer(fx.context()); err != nil {
		t.Fatal(err)
	}
	manager = newManager()
	publisher.fail.Store(false)
	if err = manager.RecoverOutbox(fx.context()); err != nil {
		t.Fatal(err)
	}
	if publisher.published.Load() != 4 {
		t.Fatalf("published=%d", publisher.published.Load())
	}
	if err = store.SetRemoteProjection(remoteStore, manager); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	p = manualRealProjector(t, wal, store)
	if err = p.Flush(fx.context()); err != nil {
		t.Fatal(err)
	}
	verify(1)
	assertWALReplayCount(t, wal, 0)
	// 第二笔 Mongo 和快照都成功，模拟只丢 checkpoint，再用新 Manager 重放。
	second := makeRecord(152, 1)
	if _, err = wal.Append(fx.context(), second); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("injected checkpoint loss")
	p.OverrideAck(func(context.Context, corenest.CommitFence) error { return lost })
	if n, err := p.ReplayPass(fx.context()); n != 1 || !errors.Is(err, lost) {
		t.Fatalf("checkpoint n=%d err=%v", n, err)
	}
	verify(2)
	assertWALReplayCount(t, wal, 1)
	if err = wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	if err = manager.StopFinalizer(fx.context()); err != nil {
		t.Fatal(err)
	}
	manager = newManager()
	if err = store.SetRemoteProjection(remoteStore, manager); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	p = manualRealProjector(t, wal, store)
	if err = p.Flush(fx.context()); err != nil {
		t.Fatal(err)
	}
	verify(2)
	assertWALReplayCount(t, wal, 0)
	pending, err = remoteStore.PendingRemoteCommits(fx.context(), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	// 另一个 Manager 没有参与重放；它必须从共享 L2 读到结果。
	reader := newManager()
	for _, id := range ids {
		for scope := uint32(1); scope <= 2; scope++ {
			key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: scope}
			value, found, err := manager.ReadRemoteSnapshot(fx.context(), key, entity.RemoteReadCached, 2)
			if err != nil || !found || value.StateVersion != 2 {
				t.Fatalf("snapshot=%+v found=%v err=%v", value, found, err)
			}
			l2, found, err := remoteentity.NewSnapshotL2Store(scoped, time.Minute).Get(fx.context(), key)
			if err != nil || !found || l2.StateVersion != 2 {
				t.Fatalf("Redis snapshot=%+v found=%v err=%v", l2, found, err)
			}
			cold, found, err := reader.ReadRemoteSnapshot(fx.context(), key, entity.RemoteReadCached, 2)
			if err != nil || !found || cold.StateVersion != 2 || cold.Checksum != l2.Checksum {
				t.Fatalf("cold reader snapshot=%+v found=%v err=%v", cold, found, err)
			}
		}
	}
	// 后一 remote Entity 冲突必须回滚前面的本地 DAO 和第一 remote Entity。
	conflict := makeRecord(153, 2)
	remote := conflict.Mutations[3].Remote
	remote.BaseVersion = 8
	remote.NextVersion = 9
	for i := range remote.Mutations {
		remote.Mutations[i].Version = 9
	}
	for i := range remote.Snapshots {
		remote.Snapshots[i].BaseVersion = 8
		remote.Snapshots[i].StateVersion = 9
	}
	conflict.Mutations[3].ExpectedVersion = 8
	conflict.Mutations[3].NextVersion = 9
	if _, err = wal.Append(fx.context(), conflict); err != nil {
		t.Fatal(err)
	}
	published := publisher.published.Load()
	if n, err := p.ReplayPass(fx.context()); n != 0 || !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("conflict n=%d err=%v", n, err)
	}
	verify(2)
	assertWALReplayCount(t, wal, 1)
	if publisher.published.Load() != published {
		t.Fatal("failed atomic transaction published snapshot")
	}
	assertCollectionCount(t, fx, engine.TransactionCollection, 2)
	assertCollectionCount(t, fx, "_remote_entity_transactions", 2)
}
