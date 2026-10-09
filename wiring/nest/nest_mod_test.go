package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type emptyGetter struct{}

type dataEngineNestProvider struct {
	committer corenest.TransactionCommitter
}

func (provider dataEngineNestProvider) NestOptions() []corenest.NestOption {
	return []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(provider.committer)}
}

type noOpCommitter struct{}

func (noOpCommitter) Commit(context.Context, corenest.CommitRecord) error { return nil }

func (emptyGetter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return nil, corenest.ErrEntityNotFound
}
func (emptyGetter) GetMany(context.Context, []int64, []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return nil, corenest.ErrEntityNotFound
}

func TestModProvidesInstanceClientAndHealth(t *testing.T) {
	cfg := viper.New()
	cfg.Set("nest.fast.queue_capacity", 8)
	cfg.Set("nest.fast.workers", 2)
	cfg.Set("nest.slow.workers", 4)
	cfg.Set("nest.slow.queue_capacity", 7)
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	client, ok := app.Lookup[corenest.Client](registry, mods.ModNest)
	if !ok || client == nil {
		t.Fatal("nest.Client not registered")
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if got := mod.Engine().Stats().Slow.WorkerNum; got != 4 {
		t.Fatalf("remote workers = %d", got)
	}
	if stats := mod.Engine().Stats(); stats.Fast.WorkerNum != 2 || stats.Slow.QueueCap != 7 || stats.Fast.QueueCap != 8 {
		t.Fatalf("pool config=%+v", stats)
	}
	if !mod.Engine().Running() {
		t.Fatal("engine not running")
	}
	healthRegistry := app.MustLookup[*health.Registry](registry, mods.ModHealth)
	snapshot := healthRegistry.Snapshot(context.Background())
	if !snapshot.OK {
		t.Fatalf("health=%+v", snapshot)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mod.Engine().Running() {
		t.Fatal("engine still running")
	}
}

func TestModSelectsDataEngineCommitterWithoutLegacyWALRuntime(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if mod.Engine() == nil {
		t.Fatal("dataengine-backed Nest engine was not constructed")
	}
}

// App 单实例锁方案 §5：任何 fail-stop（失锁、DataEngine fatal、Remote fatal）都经
// RuntimeFailure.OnFail 立即围栏 Nest，拒绝新的派发；模块自己不再各写一遍“找 Nest、围栏”。
func TestRuntimeFailureFencesNestDispatch(t *testing.T) {
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })

	cause := errors.New("remote_entity fatal release failure")
	app.MustLookup[*app.RuntimeFailure](registry, mods.ModRuntimeFailure).Fail(cause)

	if err := mod.Engine().FenceError(); !errors.Is(err, corenest.ErrNestFenced) || !errors.Is(err, cause) {
		t.Fatalf("FenceError after RuntimeFailure = %v, want ErrNestFenced wrapping the cause", err)
	}
	id := int64(((uint64(999) & entity.UniqueIDMask) << entity.UniqueIDShift) | (uint64(1) & entity.EntityCategoryMask))
	if err := mod.Engine().Dispatch(context.Background(), corenest.NewHandlerName("fenced"), id, nil); !errors.Is(err, corenest.ErrNestFenced) {
		t.Fatalf("dispatch after RuntimeFailure = %v, want ErrNestFenced", err)
	}
}
