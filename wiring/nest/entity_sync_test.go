package nest

import (
	"context"
	"errors"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"testing"
	"time"
)

func TestEntitySyncModConfigurationAndLifecycle(t *testing.T) {
	for _, mode := range []string{"periodic", "on_change"} {
		t.Run(mode, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("sync.entity.mode", mode)
			cfg.Set("sync.entity.interval", "1s")
			mod := NewModWithEntitySync(emptyGetter{}, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })}, Configure: func(c *entitysync.ManagerConfig) { c.Interval = 50 * time.Millisecond }})
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if mod.EntitySync().Mode().String() != mode || mod.EntitySync().Interval() != 50*time.Millisecond {
				t.Fatal("config precedence")
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
			if err := mod.StopWithContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mod.EntitySync().Start(context.Background()); err == nil {
				t.Fatal("owned manager not closed")
			}
		})
	}
}
func TestEntitySyncModRejectsInvalidInterval(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sync.entity.interval", "nonsense")
	mod := NewModWithEntitySync(emptyGetter{}, EntitySyncSetup{})
	if err := mod.Init(cfg); err == nil {
		t.Fatal("malformed interval became default")
	}
}

// dataEngineReloadProvider 额外提供 OnEntityLoaded（kit/dataengine Mod 的正式能力），记录 Nest Mod 装的回调。
type dataEngineReloadProvider struct {
	dataEngineNestProvider
	hook     *func(entity.IThreadSafeEntity)
	unhooked *bool
}

func (provider dataEngineReloadProvider) OnEntityLoaded(hook func(entity.IThreadSafeEntity)) (func(), error) {
	*provider.hook = hook
	return func() { *provider.unhooked = true }, nil
}

// reloadedEntity 是指针实体（RR-20260930-15 契约：实体实现必须是指针，值类型在 EntityManager.Add / BuildEntity 处被拒绝）。
type reloadedEntity struct{ *entity.EntityBase }

func (value *reloadedEntity) Base() *entity.EntityBase { return value.EntityBase }

func reloadTestState(id int64) *entity.SubjectSyncState {
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.TakeFrozenSyncPayload(1, []byte("x")), nil
	}
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }},
	})
}

// RR-20260926-30：DataEngine 驱逐实体后同步状态关闭；kit 装配在实体重新加载时把仍登记的 subject
// 接到新对象上（entitysync.Manager.Rebind），停止时注销回调。
func TestEntitySyncModRebindsReloadedEntities(t *testing.T) {
	cfg := viper.New()
	mod := NewModWithEntitySync(emptyGetter{}, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })}})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	var hook func(entity.IThreadSafeEntity)
	unhooked := false
	registry := app.NewRegistry(cfg)
	provider := dataEngineReloadProvider{dataEngineNestProvider: dataEngineNestProvider{committer: noOpCommitter{}}, hook: &hook, unhooked: &unhooked}
	if err := registry.Register(mods.ModDataEngine, provider); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if hook == nil {
		t.Fatal("nest mod did not install the entity reload hook")
	}
	const id = 4001
	evicted := reloadTestState(id)
	if err := mod.EntitySync().Register(evicted); err != nil {
		t.Fatal(err)
	}
	evicted.Close()
	base := entity.NewEntityBase(id, entity.EntityCategory(1), false)
	base.SetSyncState(reloadTestState(id))
	hook(&reloadedEntity{base})
	if err := mod.EntitySync().Rebind(reloadTestState(id)); !errors.Is(err, entitysync.ErrSubjectRegistered) {
		t.Fatalf("the reloaded state was not bound to the subject: rebind of another state=%v", err)
	}
	hook(&reloadedEntity{entity.NewEntityBase(id+1, entity.EntityCategory(1), false)}) // 未登记、无同步状态：忽略
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !unhooked {
		t.Fatal("stop did not remove the reload hook")
	}
}
