package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// RR-20261005-NC-171：卸载后重载的停止超时，Nest Mod 不能丢掉它的停止句柄，也不能在重载 worker
// 仍可能运行时关闭它的目标 entitysync；重试用新 ctx 再等，排空后才释放。

// gatedResyncAccess 把 ManagerAccess 返回的停止句柄包一层：每次调用先登记，等 drained 关闭（模拟
// worker 退出）后才交给真实句柄；ctx 先结束则如实返回 ctx 错误。
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
