package physicsroom

import (
	"context"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod 显式接收房间容量与 Nest 提交适配器。部署可把 NewServer 单独装入 app 进程。
// Commit 使用的业务 Mod 名必须放入 dependencies，保证停房先于其依赖停止。
type Mod struct {
	config       domain.Config
	dependencies []app.ModName
	local        *domain.Service
}

func NewMod(config domain.Config, dependencies ...app.ModName) *Mod {
	return &Mod{config: config, dependencies: append([]app.ModName(nil), dependencies...)}
}
func (m *Mod) Name() app.ModName        { return domain.CapabilityName }
func (m *Mod) DependsOn() []app.ModName { return append([]app.ModName(nil), m.dependencies...) }
func (m *Mod) Init(*viper.Viper) error  { return nil }
func (m *Mod) Provide(r *app.Registry) error {
	s, err := domain.New(m.config)
	if err != nil {
		return err
	}
	m.local = s
	return mods.RegisterAll(r, OwnerCapabilities(s)...)
}
func (m *Mod) Start() error { return nil }
func (m *Mod) Stop()        { _ = m.StopWithContext(context.Background()) }
func (m *Mod) StopWithContext(ctx context.Context) error {
	if m.local == nil {
		return nil
	}
	return m.local.Shutdown(ctx)
}

var _ app.Mod = (*Mod)(nil)
