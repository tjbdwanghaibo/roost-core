package nest

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// reloadLoader 是 AggregateLoader：按“权威”构造同 ID 的新实例，发布经 entity.RunLocal（与 EntityRepository 相同）。
type reloadLoader struct {
	manager *entity.EntityManager
	loads   chan bool // 值：发布是否在快池执行
}

func (loader *reloadLoader) LoadEntity(ctx context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	base := entity.NewEntityBase(id, entity.EntityCategory(1), false)
	base.SetSyncState(reloadTestState(id))
	fresh := &reloadedEntity{base}
	var addErr error
	onFast := false
	if err := entity.RunLocal(ctx, func() {
		onFast = fctx.InFastWorker()
		addErr = loader.manager.TryAdd(fresh)
	}); err != nil {
		return nil, err
	}
	loader.loads <- onFast
	return fresh, addErr
}

// RR-20260926-59：kit 装配在 Nest Mod 启动时把 entitysync 接到 ManagerAccess 的卸载后重载（ConfigureUnloadResync，
// 发布经 NestMgr.RunLocal 回快池），停止时先停重载再停 Nest。卸载一个仍有订阅者的实体后，框架主动重载并重新绑定。
func TestEntitySyncModResyncsSubscribersAfterUnload(t *testing.T) {
	cfg := viper.New()
	frames := make(chan []byte, 8)
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &reloadLoader{manager: manager, loads: make(chan bool, 4)}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	mod := NewModWithEntitySync(access, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		frames <- append([]byte(nil), p...)
		return nil
	})}})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = mod.StopWithContext(context.Background())
		}
	})
	const id = 4101
	base := entity.NewEntityBase(id, entity.EntityCategory(1), false)
	base.SetSyncState(reloadTestState(id))
	stale := &reloadedEntity{base}
	manager.Add(stale)
	syncMgr := mod.EntitySync()
	if err := syncMgr.Register(stale.Sync()); err != nil {
		t.Fatal(err)
	}
	if err := syncMgr.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err := syncMgr.Subscribe(1, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := syncMgr.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-frames
	if err := mod.Engine().RunLocal(context.Background(), func() {
		if err := access.Unload(context.Background(), stale); err != nil {
			t.Error(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case onFast := <-loader.loads:
		if !onFast {
			t.Fatal("reloaded entity was published outside the Nest fast pool")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kit assembly did not reload an unloaded entity that still has subscribers")
	}
	fresh := manager.Get(id)
	if fresh == nil || fresh.Base() == stale.Base() {
		t.Fatal("reloaded instance is not resident")
	}
	deadline := time.Now().Add(3 * time.Second)
	for syncMgr.SubjectAwaitsReload(id) {
		if time.Now().After(deadline) {
			t.Fatal("reloaded state was not bound to the subject")
		}
		time.Sleep(time.Millisecond)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped = true
	stop, err := access.ConfigureUnloadResync(syncMgr, entity.UnloadResyncConfig{})
	if err != nil {
		t.Fatalf("stop did not release the unload resync: %v", err)
	}
	_ = stop(context.Background())
}
