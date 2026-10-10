package nest

import (
	"context"
	"errors"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

type gatedResyncAccess struct {
	*entity.ManagerAccess
	entered chan struct{}
	drained chan struct{}
	calls   atomic.Int32
}

func (a *gatedResyncAccess) ConfigureUnloadResync(target entity.UnloadedSubjectSync, config entity.UnloadResyncConfig) (func(context.Context) error, error) {
	stop, err := a.ManagerAccess.ConfigureUnloadResync(target, config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		a.calls.Add(1)
		select {
		case a.entered <- struct{}{}:
		default:
		}
		select {
		case <-a.drained:
			return stop(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}

func TestNestModStopRetryKeepsUnloadResyncUntilItDrains(t *testing.T) {
	cfg := viper.New()
	access := &gatedResyncAccess{ManagerAccess: entity.NewManagerAccess(entity.NewEntityManager()), entered: make(chan struct{}, 4), drained: make(chan struct{})}
	mod := NewModWithEntitySync(access, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error {
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
	drainedClosed := false
	t.Cleanup(func() {
		if !drainedClosed {
			close(access.drained)
		}
		_ = mod.StopWithContext(context.Background())
	})
	syncMgr := mod.EntitySync()
	syncOpen := func(id int64) bool {
		err := syncMgr.Register(reloadTestState(id))
		if err != nil && !errors.Is(err, entitysync.ErrManagerClosed) {
			t.Fatalf("Register probe: %v", err)
		}
		return err == nil
	}

	// 第一次停止：预算在等重载 worker 时耗尽。
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mod.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Stop = %v, want context.Canceled from the unload resync wait", err)
	}
	select {
	case <-access.entered: // 第一次停止确实在等重载 worker
	default:
		t.Fatal("first Stop did not wait for the unload resync")
	}

	// 重试：必须再次在自己的 ctx 内等重载 worker，而不是报告成功并关闭 entitysync。
	retryCtx, cancelRetry := context.WithCancel(context.Background())
	defer cancelRetry()
	retry := make(chan error, 1)
	go func() { retry <- mod.StopWithContext(retryCtx) }()
	var retryErr error
	select {
	case <-access.entered:
		cancelRetry()
		retryErr = <-retry
	case retryErr = <-retry:
	case <-time.After(5 * time.Second):
		t.Fatal("retry Stop neither waited for the unload resync nor returned")
	}
	if retryErr == nil {
		t.Errorf("retry Stop reported success while the unload resync worker was still running (stop handle calls=%d)", access.calls.Load())
	}
	if !syncOpen(4201) {
		t.Errorf("entity sync was closed while the unload resync worker that targets it was still running")
	}

	// 排空后用新 ctx 重试：完成停止，entitysync 此时才关闭。
	close(access.drained)
	drainedClosed = true
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after the resync drained = %v", err)
	}
	if syncOpen(4202) {
		t.Fatal("entity sync still open after a successful Stop")
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after a completed stop = %v", err)
	}
}
