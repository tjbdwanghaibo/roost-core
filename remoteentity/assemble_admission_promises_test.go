package remoteentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// U-0128 · C2（空洞测试）· nightly gap map core `remoteentity` 12/20。
//
// P3b 新增的 Assemble 是 kit Mod 与本包之间唯一的装配入口，它对缺失依赖的每
// 一条拒绝（无 Redis、sid 为零、按 Loader 建 Mongo 后端却没有 Mongo、既无 Loader
// 也无 Backend、Backend 不支持调用方持有的原子事务）此前零覆盖；Start 对未装配
// / 无总线的拒绝同样。写批次准入：nil / 无配置的管理器、未设后端、finalize 槽位
// 耗尽（AsyncFinalizeCapacity）都必须以对应哨兵拒绝，槽位在批次 Close 后归还；
// nil 包装器 / nil 批次的 beginWrite / Commit 不能 panic。

// assembleRedis is an IRedis that must never be reached: Assemble only wires
// constructors, none of which talk to Redis.
type assembleRedis struct{ fredis.IRedis }

// atomicTestBackend adds the caller-owned atomic transaction capability the
// Assembly requires of a backend.
type atomicTestBackend struct{ *remoteTestLoader }

func (b *atomicTestBackend) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func TestAssembleRefusesEachMissingDependency(t *testing.T) {
	redis := assembleRedis{}
	backend := &atomicTestBackend{newRemoteTestLoader()}
	cases := []struct {
		name string
		deps AssemblyDeps
		sid  int32
		text string
	}{
		{"no redis", AssemblyDeps{Backend: backend}, 7, "redis client is required"},
		{"zero sid", AssemblyDeps{Redis: redis, Backend: backend}, 0, "non-zero sid is required"},
		{"loader without mongo", AssemblyDeps{Redis: redis, Loader: newMockLoader()}, 7, "mongo client is required"},
		{"neither loader nor backend", AssemblyDeps{Redis: redis}, 7, "atomic storage backend is required"},
		{"backend without atomic transactions", AssemblyDeps{Redis: redis, Backend: newRemoteTestLoader()}, 7, "must support caller-owned atomic transactions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assembly, err := Assemble(tc.deps, nil, tc.sid, MongoBackendConfig{})
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Assemble = (%v, %v), want error containing %q", assembly, err, tc.text)
			}
			if assembly != nil {
				t.Fatal("a refused Assemble still returned an assembly")
			}
		})
	}

	assembly, err := Assemble(AssemblyDeps{Redis: redis, Backend: backend}, nil, 7, MongoBackendConfig{})
	if err != nil {
		t.Fatalf("Assemble with every dependency = %v", err)
	}
	if assembly.Manager == nil || assembly.LockFactory == nil || assembly.AtomicStore == nil {
		t.Fatalf("assembly is missing a part: %+v", assembly)
	}
	var none *Assembly
	if err := none.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("Start on a nil assembly = %v", err)
	}
	if err := (&Assembly{}).Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("Start on an empty assembly = %v", err)
	}
	if err := assembly.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "sync bus is required") {
		t.Fatalf("Start without a bus = %v", err)
	}
}

func TestPrepareRemoteWriteBatchRefusesWithoutManagerBackendOrFinalizeSlot(t *testing.T) {
	const kind entity.EntityKind = 135
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	// 每次准入都带期限：守卫若失效，调用会卡在写门或 nil 通道上，必须在期限内红而不是挂住。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, other := newTestRemoteEntity(1735, 1, kind), newTestRemoteEntity(1736, 1, kind)
	ids := []int64{live.GUId()}

	var none *Manager
	if _, err := none.PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare on a nil manager = %v", err)
	}
	if _, err := (&Manager{}).PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare on a manager without config = %v", err)
	}

	// 未设后端：批次 id 合法也不能准入。
	bare := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	if _, err := bare.PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare without a backend = %v", err)
	}
	bare.mu.RLock()
	wrappers := len(bare.wrappers)
	bare.mu.RUnlock()
	if wrappers != 0 {
		t.Fatalf("a refused Prepare left %d wrappers behind", wrappers)
	}

	// finalize 槽位只有一个：第二个批次被拒，第一个 Close 后归还。
	cfg := DefaultConfig()
	cfg.AsyncFinalizeCapacity = 1
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	loader.add(live)
	loader.add(other)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	first, err := mgr.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		t.Fatalf("first Prepare = %v", err)
	}
	if _, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{other.GUId()}); !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("Prepare for another entity with every finalize slot taken = %v, want ErrRemoteOverloaded", err)
	}
	if err := first.Abort(ctx, errors.New("test")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := mgr.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		t.Fatalf("Prepare after the slot was returned = %v", err)
	}
	_ = second.Abort(ctx, errors.New("test"))
	_ = second.Close(ctx)

	var wrapper *remoteEntityWrapper
	if _, err := wrapper.beginWrite(ctx); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("beginWrite on a nil wrapper = %v", err)
	}
	if _, err := (&remoteEntityWrapper{}).beginWrite(ctx); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("beginWrite on a wrapper without a manager = %v", err)
	}
	var batch *remoteWriteBatch
	if _, err := batch.Commit(ctx); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Commit on a nil batch = %v", err)
	}
	if _, err := (&remoteWriteBatch{}).Commit(ctx); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Commit on a batch without a manager = %v", err)
	}
}
