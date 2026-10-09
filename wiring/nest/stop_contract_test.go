package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// A3 / RR-20261005-NC-171：Nest Mod 的停止套共用停机契约骨架。卡住的工作是卸载后重载的 worker
// （gatedResyncAccess 的停止句柄等 drained），“资源”是它写入的 entitysync——只能在 worker 退出后关闭。
// 只覆盖停机路径：快池 handler 不参与，契约里没有任何等待进入快 worker。
func TestNestModStopContract(t *testing.T) {
	access := &gatedResyncAccess{ManagerAccess: entity.NewManagerAccess(entity.NewEntityManager()), entered: make(chan struct{}, 16), drained: make(chan struct{})}
	var mod *Mod
	probe := int64(4300)
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			cfg := viper.New()
			mod = NewModWithEntitySync(access, EntitySyncSetup{Config: entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error {
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
		},
		Stop:    func(ctx context.Context) error { return mod.StopWithContext(ctx) },
		Release: func() { close(access.drained) },
		Released: func() bool {
			probe++
			err := mod.EntitySync().Register(reloadTestState(probe))
			if err != nil && !errors.Is(err, entitysync.ErrManagerClosed) {
				t.Fatalf("Register probe: %v", err)
			}
			return errors.Is(err, entitysync.ErrManagerClosed)
		},
	})
}
