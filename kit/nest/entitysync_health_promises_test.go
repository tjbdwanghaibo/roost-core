package nest

import (
	"context"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"testing"
)

type healthSyncTransport struct{}

func (healthSyncTransport) Push(context.Context, entitysync.SessionID, []byte) error { return nil }
func TestProvidedEntitySyncHealthIsRegistered(t *testing.T) {
	cfg := viper.New()
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: healthSyncTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMod(emptyGetter{})
	m.entitySync = manager
	if err = m.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err = m.Provide(r); err != nil {
		t.Fatal(err)
	}
	if err = manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := app.MustLookup[*health.Registry](r, mods.ModHealth)
	for _, result := range h.Snapshot(context.Background()).Results {
		if result.Name == "entitysync" {
			if result.Status != health.StatusFail {
				t.Fatalf("closed sync: %+v", result)
			}
			return
		}
	}
	t.Fatal("entitysync health is absent from production registry")
}
