package nest

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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

func TestEntitySyncBatchWindowConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, value             string
		initial, override, want time.Duration
		invalid                 bool
	}{
		{name: "configured", value: "5ms", want: 5 * time.Millisecond},
		{name: "explicit-zero", value: "0s", initial: time.Second},
		{name: "code-default", initial: time.Second, want: time.Second},
		{name: "configure-wins", value: "5ms", override: 2 * time.Millisecond, want: 2 * time.Millisecond},
		{name: "negative", value: "-1ms", invalid: true},
		{name: "malformed", value: "wrong", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("sync.entity.mode", "on_change")
			if tc.value != "" {
				cfg.Set("sync.entity.on_change_batch_window", tc.value)
			}
			setup := EntitySyncSetup{Config: entitysync.ManagerConfig{Mode: entitysync.ModeOnChange, OnChangeBatchWindow: tc.initial, Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })}}
			if tc.override > 0 {
				setup.Configure = func(c *entitysync.ManagerConfig) { c.OnChangeBatchWindow = tc.override }
			}
			mod := NewModWithEntitySync(emptyGetter{}, setup)
			err := mod.Init(cfg)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid setting accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer mod.EntitySync().Close(context.Background())
			if got := mod.EntitySync().OnChangeBatchWindow(); got != tc.want {
				t.Fatalf("window %v want %v", got, tc.want)
			}
		})
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

// DataEngine 驱逐实体后同步状态关闭；装配在实体重新加载时把仍登记的 subject 接到新对象上（entitysync.Manager.Rebind），停止时注销回调。
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

// pipelinedStubCommitter 只提供水位；本测试不跑事务，内容的 CommitLSN 手工设定。
type pipelinedStubCommitter struct {
	noOpCommitter
	durable atomic.Uint64
}

func (c *pipelinedStubCommitter) Enqueue(context.Context, corenest.CommitRecord) (corenest.CommitTicket, error) {
	return nil, corenest.ErrCommitterRequired
}
func (c *pipelinedStubCommitter) DurableLSN() uint64 { return c.durable.Load() }

// 装配的 EntitySync 在 committer 为 pipelined 时自动以其 DurableLSN 作外发水位。
func TestEntitySyncModWiresPipelinedDurableWatermark(t *testing.T) {
	pipelined := &pipelinedStubCommitter{}
	for _, tc := range []struct {
		name      string
		committer corenest.TransactionCommitter
		explicit  func() uint64
		gated     bool
	}{
		{name: "pipelined_committer", committer: pipelined, gated: true},
		{name: "explicit_watermark_wins", committer: pipelined, explicit: func() uint64 { return math.MaxUint64 }},
		{name: "strict_committer_has_no_gate", committer: noOpCommitter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipelined.durable.Store(0)
			sent := make(chan []byte, 8)
			setup := EntitySyncSetup{Config: entitysync.ManagerConfig{DurableWatermark: tc.explicit, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
				sent <- p
				return nil
			})}}
			mod := NewModWithEntitySync(emptyGetter{}, setup)
			cfg := viper.New()
			if err := mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			registry := app.NewRegistry(cfg)
			if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: tc.committer}); err != nil {
				t.Fatal(err)
			}
			if err := mod.Provide(registry); err != nil {
				t.Fatal(err)
			}
			manager := mod.EntitySync()
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			payload := entity.CopyFrozenSyncPayload(1, []byte{9})
			pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) { return payload, nil }
			state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: 4242, Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(p) }}})
			state.SetLastCommitLSN(5) // 一次尚未持久的 pipelined 提交产生的内容
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			if err := manager.OpenSession(1); err != nil {
				t.Fatal(err)
			}
			if err := manager.Subscribe(1, 4242, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(sent); tc.gated && got != 0 {
				t.Fatalf("content above the pipelined durable watermark was sent (%d frames); kit did not wire DurableLSN", got)
			} else if !tc.gated && got != 1 {
				t.Fatalf("frames=%d, want 1 (no pipelined watermark applies)", got)
			}
			if !tc.gated {
				return
			}
			pipelined.durable.Store(5)
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(sent); got != 1 {
				t.Fatalf("frames after the watermark reached the commit = %d, want 1", got)
			}
		})
	}
}

// reloadLoader 是 AggregateLoader：按“权威”构造同 ID 的新实例，发布经 entity.RunLocal（与 EntityRepository 相同）。
type reloadLoader struct {
	manager *entity.EntityManager
	loads   chan bool // 值：发布是否在业务池执行
}

func (loader *reloadLoader) LoadEntity(ctx context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeEntity, error) {
	base := entity.NewEntityBase(id, entity.EntityCategory(1), false)
	base.SetSyncState(reloadTestState(id))
	fresh := &reloadedEntity{base}
	var addErr error
	onFast := false
	if err := entity.RunLocal(ctx, func() {
		onFast = fctx.InShortWorker()
		addErr = loader.manager.TryAdd(fresh)
	}); err != nil {
		return nil, err
	}
	loader.loads <- onFast
	return fresh, addErr
}

// 装配在 Nest Mod 启动时把 entitysync 接到 ManagerAccess 的卸载后重载（ConfigureUnloadResync，发布经 NestMgr.RunLocal 回业务池），
// 停止时先停重载再停 Nest。卸载一个仍有订阅者的实体后，框架主动重载并重新绑定。
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
			t.Fatal("reloaded entity was published outside the Nest business pool")
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
