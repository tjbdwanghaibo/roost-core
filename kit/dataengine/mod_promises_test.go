package dataengine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	engine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// U-0113 · C2（空洞测试）· nightly gap map kit `dataengine` 10/10。
//
// P3b 之后 Mod 只剩查找能力、装配、注册与转交，这十条守卫就是它的全部职责：
// nil 注册表、缺实体访问、缺 Mongo / JetStream / Remote Entity / 原子存储 / 健康
// 注册表各自点名拒绝（Provide 失败让进程在启动时停下，而不是 Start 时 nil 解
// 引用）；未 Provide 就 Start 拒绝；Start 之前 Commit / Enqueue 以
// ErrCommitterRequired 拒绝，Nest 不能把一条提交交给一个还没打开 WAL 的引擎。

// remoteManagerStub is an entity.IRemoteEntityManager that can apply commits;
// nothing else is expected to be called.
type remoteManagerStub struct{ entity.IRemoteEntityManager }

func (remoteManagerStub) ApplyRemoteCommits(context.Context, entity.RemoteTransactionID, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

type remoteStoreStub struct{}

func (remoteStoreStub) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func newModConfig(t *testing.T) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.dir", t.TempDir())
	return cfg
}

func TestModProvideRefusesEachMissingCapability(t *testing.T) {
	cfg := newModConfig(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	type registration func(*app.Registry)
	withMongo := func(r *app.Registry) { _ = r.Register(mods.ModMongo, fmongo.IMongo(mongotest.NewClient())) }
	withJetStream := func(r *app.Registry) { _ = r.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})) }
	withRemote := func(r *app.Registry) { _ = r.Register(mods.ModRemoteEntity, entity.IRemoteEntityManager(remoteManagerStub{})) }
	withAtomic := func(r *app.Registry) { _ = r.Register(mods.ModRemoteEntityAtomicStore, engine.RemoteProjectionStore(remoteStoreStub{})) }

	if err := NewMod(WithEntityAccess(access)).Provide(nil); err == nil || !strings.Contains(err.Error(), "nil registry") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	cases := []struct {
		name    string
		mod     *Mod
		regs    []registration
		text    string
		wantErr error
	}{
		{"no entity access", NewMod(), []registration{withMongo, withJetStream}, "entity access is required", nil},
		{"access without manager", NewMod(WithEntityAccess(&entity.ManagerAccess{})), []registration{withMongo, withJetStream}, "entity access is required", nil},
		{"no mongo", NewMod(WithEntityAccess(access)), []registration{withJetStream}, `capability "mongo" not found`, nil},
		{"no jetstream", NewMod(WithEntityAccess(access)), []registration{withMongo}, `capability "nats.jetstream" not found`, nil},
		{"remote projection without manager", NewMod(WithEntityAccess(access), WithRemoteProjection(true)), []registration{withMongo, withJetStream, withAtomic}, `capability "remote_entity" not found`, nil},
		{"remote projection without atomic store", NewMod(WithEntityAccess(access), WithRemoteProjection(true)), []registration{withMongo, withJetStream, withRemote}, `capability "remote_entity.atomic_store" not found`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := app.NewRegistry(cfg)
			for _, reg := range tc.regs {
				reg(registry)
			}
			if err := tc.mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			err := tc.mod.Provide(registry)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Provide = %v, want error containing %q", err, tc.text)
			}
			if _, registered := registry.Get(mods.ModDataEngine); registered {
				t.Fatal("a refused Provide still published the dataengine capability")
			}
		})
	}
}

func TestModRefusesLifecycleCallsBeforeProvideAndStart(t *testing.T) {
	ctx := context.Background()
	cfg := newModConfig(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	mod := NewMod(WithEntityAccess(access))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	// 未 Provide：Start 拒绝，提交入口拒绝而不是 nil 解引用。
	if err := mod.Start(); err == nil || !strings.Contains(err.Error(), "not provided") {
		t.Fatalf("Start before Provide = %v", err)
	}
	var none *Mod
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "not provided") {
		t.Fatalf("nil Mod Start = %v", err)
	}
	record := corenest.CommitRecord{ID: corenest.TransactionID{1}}
	if err := mod.Commit(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Commit before Start = %v, want ErrCommitterRequired", err)
	}
	if ticket, err := mod.Enqueue(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) || ticket != nil {
		t.Fatalf("Enqueue before Start = (%v, %v), want ErrCommitterRequired", ticket, err)
	}

	// Provide 后、Start 前：runtime 仍为空，提交入口同样拒绝。
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongotest.NewClient())); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Commit(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Commit after Provide, before Start = %v", err)
	}
	if _, err := mod.Enqueue(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Enqueue after Provide, before Start = %v", err)
	}
}
