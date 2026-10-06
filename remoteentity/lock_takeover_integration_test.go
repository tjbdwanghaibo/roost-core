//go:build integration

package remoteentity

// O-M6-6 在真实 Redis（取锁 Lua）与真实 Mongo（许可 / 提交权威）上：同 sid 重启的新进程第一次取锁（不重试）
// 就接管上一代进程留下的共享锁；别的 sid、未启用单实例锁（没有 Incarnation）的不接管。私有环境
// scripts/mirror-local.sh test-core 上运行（ROOST_MIRROR_LOCAL=1），不碰共享隔离环境。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

type realTakeoverEnv struct {
	redis    fredis.IRedis
	store    *MongoCommitter
	database string
	cfg      *Config
	entity   int64
}

func newRealTakeoverEnv(t *testing.T) *realTakeoverEnv {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || os.Getenv("ROOST_MIRROR_LOCAL") != "1" {
		t.Skip("run through scripts/mirror-local.sh test-core (private environment)")
	}
	redis := realRemoteRedis(t)
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	env := &realTakeoverEnv{redis: redis, database: fmt.Sprintf("roost_om66_%d", time.Now().UnixNano()), cfg: DefaultConfig()}
	env.cfg.LockTTL = 3 * time.Second
	env.cfg.LockKey = "{om66-" + generateToken() + "}"
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 198, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	env.entity, _ = entity.BuildEntityID(998, 198)
	lockKey := "lock:" + env.cfg.LockKey + ":" + fmt.Sprint(env.entity)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = redis.Del(ctx, lockKey, lockKey+":fence")
		if err := mongo.Database(env.database).Drop(ctx); err != nil {
			t.Error(err)
		}
		_ = mongo.Close(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env.store = NewMongoCommitter(mongo, env.database, takeoverSid, 0)
	if err := env.store.EnsureRemoteStorage(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := env.store.ClaimOwnership(ctx, env.entity, takeoverSid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.EnterSharedExpected(ctx, env.entity, lease); err != nil {
		t.Fatal(err)
	}
	return env
}

// process 是一个进程的 Assembly 与这个实体的一把锁：不开异步续期、不重试（第一次取锁必须就成）。
func (env *realTakeoverEnv) process(t *testing.T, sid int32, incarnation *ProcessIncarnation) *versionedLock {
	t.Helper()
	backend, err := NewBackend(entity.NewManagerAccess(entity.NewEntityManager()), env.store)
	if err != nil {
		t.Fatal(err)
	}
	asm, err := Assemble(AssemblyDeps{Redis: env.redis, Backend: backend, Incarnation: incarnation}, env.cfg, sid, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return asm.LockFactory.NewVersionedLock(env.entity, fredis.VersionedLockOptions{Key: env.cfg.LockKey, TTL: env.cfg.LockTTL}).(*versionedLock)
}

func (env *realTakeoverEnv) commit(tx byte, grant WriteGrant) entity.RemoteCommit {
	return entity.RemoteCommit{TransactionID: entity.RemoteTransactionID{14: 0x66, 15: tx}, EntityID: env.entity, Kind: 198,
		BaseVersion: uint64(grant.Version), NextVersion: uint64(grant.Version) + 1, MarkerEpoch: grant.Ownership.MarkerEpoch, RouteEpoch: grant.Ownership.RouteEpoch, LockFence: grant.Fence, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Database: env.database, Collection: "guilds", ID: env.entity, Version: uint64(grant.Version) + 1, Data: []byte(fmt.Sprintf("tx%d", tx))}}}
}

func TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock(t *testing.T) {
	env := newRealTakeoverEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old := env.process(t, takeoverSid, gameSingleton(takeoverSid, "0123456789abcdef"))
	if err := old.TryLock(ctx); err != nil {
		t.Fatal(err)
	}
	oldGrant, _ := old.writeGrant()
	// SIGKILL：旧进程不释放、不续期。另一个 sid、未启用单实例锁的同 sid 进程都不能接管。
	if err := env.process(t, 1102, gameSingleton(1102, "fedcba9876543210")).TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("another sid's TryLock = %v, want not acquired", err)
	}
	if err := env.process(t, takeoverSid, nil).TryLock(ctx); !errors.Is(err, ErrVersionedLockNotAcquired) {
		t.Fatalf("same sid without the singleton lock: TryLock = %v, want not acquired (no proof the old process is dead)", err)
	}
	restarted := env.process(t, takeoverSid, gameSingleton(takeoverSid, "fedcba9876543210"))
	started := time.Now()
	if err := restarted.TryLock(ctx); err != nil {
		t.Fatalf("the restarted same-sid process's first TryLock = %v, want it to take over the dead incarnation's lease (lock_ttl %v)", err, env.cfg.LockTTL)
	}
	took := time.Since(started)
	newGrant, _ := restarted.writeGrant()
	if newGrant.Fence <= oldGrant.Fence || restarted.Fence() != newGrant.Fence {
		t.Fatalf("grant after takeover %+v (redis fence %d), want newer than %+v", newGrant, restarted.Fence(), oldGrant)
	}
	_, oldErr := env.store.CommitRemote(ctx, env.commit(1, oldGrant))
	if oldErr == nil {
		t.Fatal("the previous incarnation's grant still committed after the takeover")
	}
	if err := old.Touch(ctx, time.Second); !errors.Is(err, ErrVersionedLockExpired) {
		t.Fatalf("the previous incarnation's touch = %v, want expired", err)
	}
	if err := old.Unlock(ctx, 9, time.Second); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("the previous incarnation's unlock = %v, want not owned", err)
	}
	if _, err := env.store.CommitRemote(ctx, env.commit(2, newGrant)); err != nil {
		t.Fatalf("commit with the new incarnation's grant: %v", err)
	}
	if err := restarted.Unlock(ctx, newGrant.Version+1, time.Second); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	t.Logf("MIRROR O-M6-6: restarted same-sid TryLock took %v (lock_ttl %v); fence %d -> %d; old grant commit: %v", took.Round(time.Millisecond), env.cfg.LockTTL, oldGrant.Fence, newGrant.Fence, oldErr)
}
