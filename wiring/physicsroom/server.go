package physicsroom

import (
	"context"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 房间路由必须固定 owner，不能使用通用生成装配中的随机实例 ClientMod。
// 传输 DTO/handler 仍从 Rooms 接口生成；这里只负责固定路由和生命周期。
func OwnerCapabilities(s domain.Rooms) []mods.Capability {
	return []mods.Capability{{Name: domain.CapabilityName, Value: domain.Capability(s)}, {Name: domain.LocalCapabilityName, Value: s}}
}

type Server struct{ runtime *domain.Server }

func NewServer() *Server              { return &Server{} }
func (*Server) Name() app.ServiceName { return app.ServiceName(domain.ServiceType) }
func (s *Server) Init(r *app.Registry) error {
	local, ok := app.Lookup[domain.Rooms](r, domain.LocalCapabilityName)
	if !ok {
		return domain.ErrRequestInvalid
	}
	b, ok := app.Lookup[bus.IBus](r, mods.ModBus)
	if !ok {
		return domain.ErrRequestInvalid
	}
	if err := domain.RegisterHandlers(b, local); err != nil {
		return err
	}
	s.runtime = domain.NewServer(local)
	return nil
}
func (s *Server) Serve(ctx context.Context) error { return s.runtime.Serve(ctx) }
func (s *Server) Shutdown(ctx context.Context) error {
	if s.runtime == nil {
		return nil
	}
	return s.runtime.Shutdown(ctx)
}

type ClientMod struct {
	owner   int32
	timeout time.Duration
}

func NewClientMod(ownerSID int32, timeout time.Duration) *ClientMod {
	return &ClientMod{owner: ownerSID, timeout: timeout}
}
func (*ClientMod) Name() app.ModName { return domain.CapabilityName }

// DependsOn 写 NATS Mod 的名字：bus 只是它提供的能力名，App 按 Mod 名排序，写成能力名会在启动时报 unknown mod dependency。
func (*ClientMod) DependsOn() []app.ModName { return []app.ModName{mods.ModNats} }
func (m *ClientMod) Init(*viper.Viper) error {
	if m.owner <= 0 {
		return domain.ErrRequestInvalid
	}
	return nil
}
func (m *ClientMod) Provide(r *app.Registry) error {
	b, ok := app.Lookup[bus.IBus](r, mods.ModBus)
	if !ok {
		return domain.ErrRequestInvalid
	}
	client, err := domain.NewOwnerClient(b, m.owner, m.timeout)
	if err != nil {
		return err
	}
	return mods.RegisterAll(r, mods.Capability{Name: domain.CapabilityName, Value: domain.Capability(client)})
}
func (*ClientMod) Start() error { return nil }
func (*ClientMod) Stop()        {}

var _ app.Service = (*Server)(nil)
var _ app.Mod = (*ClientMod)(nil)
