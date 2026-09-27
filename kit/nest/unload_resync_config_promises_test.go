package nest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260927-13（OPEN-ITEMS C13）：卸载后重载（RR-20260926-59，entity.UnloadResyncConfig）与共享冷加载的框架上限
// （RR-20260926-54，ManagerAccess.ConfigureLoadTimeout）只有代码级入口：kit 的 Nest Mod 固定传零值 UnloadResyncConfig{}
// （kit/nest/nest_mod.go:175），ConfigureLoadTimeout 没有任何生产调用方，部署无法按规模调整。承诺：kit 暴露配置键
// nest.unload_resync.{workers,attempts,queue_capacity} 与 nest.entity_load_timeout，键缺省时行为与之前完全相同（零值 → 框架默认），
// 负值在 Init 拒绝。

// recordingAccess 记下 kit 交给 ManagerAccess 的两项配置，其余行为照旧。
type recordingAccess struct {
	*entity.ManagerAccess
	resync         *entity.UnloadResyncConfig
	loadTimeout    time.Duration
	loadTimeoutSet bool
}

func (a *recordingAccess) ConfigureUnloadResync(target entity.UnloadedSubjectSync, config entity.UnloadResyncConfig) (func(context.Context) error, error) {
	a.resync = &config
	return a.ManagerAccess.ConfigureUnloadResync(target, config)
}

func (a *recordingAccess) ConfigureLoadTimeout(timeout time.Duration) {
	a.loadTimeout, a.loadTimeoutSet = timeout, true
	a.ManagerAccess.ConfigureLoadTimeout(timeout)
}

func startEntitySyncModWith(t *testing.T, cfg *viper.Viper) (*recordingAccess, error) {
	t.Helper()
	access := &recordingAccess{ManagerAccess: entity.NewManagerAccess(entity.NewEntityManager())}
	mod := NewModWithEntitySync(access, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error {
		return nil
	})}})
	if err := mod.Init(cfg); err != nil {
		return access, err
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
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	return access, nil
}

func TestNestModWiresUnloadResyncAndLoadTimeoutConfig(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("nest.unload_resync.workers", 2)
		cfg.Set("nest.unload_resync.attempts", 3)
		cfg.Set("nest.unload_resync.queue_capacity", 128)
		cfg.Set("nest.entity_load_timeout", "7s")
		access, err := startEntitySyncModWith(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := entity.UnloadResyncConfig{Workers: 2, Attempts: 3, QueueCapacity: 128}
		if access.resync == nil || *access.resync != want {
			t.Fatalf("unload resync config handed to ManagerAccess = %+v, want %+v from nest.unload_resync.*", access.resync, want)
		}
		if !access.loadTimeoutSet || access.loadTimeout != 7*time.Second {
			t.Fatalf("ConfigureLoadTimeout called=%v with %v, want 7s from nest.entity_load_timeout", access.loadTimeoutSet, access.loadTimeout)
		}
	})

	t.Run("defaults unchanged", func(t *testing.T) {
		access, err := startEntitySyncModWith(t, viper.New())
		if err != nil {
			t.Fatal(err)
		}
		if access.resync == nil || *access.resync != (entity.UnloadResyncConfig{}) {
			t.Fatalf("without nest.unload_resync.* the config must stay the zero value (framework defaults), got %+v", access.resync)
		}
		if access.loadTimeoutSet {
			t.Fatalf("without nest.entity_load_timeout the framework load timeout must not be touched, got %v", access.loadTimeout)
		}
	})

	for key, value := range map[string]any{
		"nest.unload_resync.workers":        -1,
		"nest.unload_resync.attempts":       -2,
		"nest.unload_resync.queue_capacity": -3,
		"nest.entity_load_timeout":          "-1s",
	} {
		t.Run("rejects negative "+key, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set(key, value)
			_, err := startEntitySyncModWith(t, cfg)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Init accepted %s=%v (err=%v); want an error naming the key", key, value, err)
			}
		})
	}
}
