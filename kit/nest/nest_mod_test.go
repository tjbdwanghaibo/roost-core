package nest

import (
	"context"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/mods"
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
	cfg.Set("nest.worker_num", 1)
	cfg.Set("nest.queue_capacity", 8)
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
