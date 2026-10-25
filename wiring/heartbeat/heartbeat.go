// Package heartbeat 把按实体的心跳驱动（gameplay/heartbeat.Driver）接进 App：依赖 Nest Mod，
// Provide 时取 Nest 客户端、只读实体查找与 App 业务时钟交给驱动，并把驱动登记为 mods.ModHeartbeat；Start 启动驱动
// goroutine，Stop 先于 Nest 停止（App 按依赖逆序停：先停 Service、再停本 Mod、最后停 Nest）。
//
// 驱动本身（调度、投递、跳拍规则）在 gameplay/heartbeat；这里只做接线，不持有运行状态。唯一的配置项是投递方式
// heartbeat.delivery（batch 缺省，走 Nest 批量心跳作业 / dispatch），Init 时校验并
// 交给驱动一次，运行期不切换；档位、分辨率、驱动数都是固定的。业务把驱动写成包级变量，组件经它 NewHost，进程装配用
// NewMod 装上同一个驱动。
package heartbeat

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	coreheartbeat "github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod 装配一个 Nest 引擎上的心跳驱动。
type Mod struct {
	access  *entity.ManagerAccess
	driver  *coreheartbeat.Driver
	started bool
}

// NewMod 接收实例级 ManagerAccess（驱动只读其中已发布的实体，不加载）与业务的心跳驱动。
func NewMod(access *entity.ManagerAccess, driver *coreheartbeat.Driver) *Mod {
	return &Mod{access: access, driver: driver}
}

func (m *Mod) Name() app.ModName { return mods.ModHeartbeat }

// config 是 heartbeat.* 的声明。
type config struct {
	Heartbeat struct {
		Delivery string `config:"delivery" default:"batch" enum:"dispatch|batch" example:"batch" help:"心跳交给 Nest 的方式：batch 按 64 拍一批走 Nest 批量心跳作业（缺省：进程 CPU 低，普通业务消息 p99 略高）；dispatch 每拍一条单目标 Dispatch。启动时确定，运行期不切换"`
	} `config:"heartbeat"`
}

// ConfigSchema 声明 heartbeat.delivery。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// DependsOn：投递走 Nest 客户端，Nest 先启动、后停止。
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModNest} }

// Init 校验依赖并把 heartbeat.delivery 交给驱动（驱动第一次 Start 之前；之后不能再改）。
func (m *Mod) Init(cfg *viper.Viper) error {
	if m.access == nil || m.access.Manager() == nil {
		return errors.New("heartbeat mod: entity access is required")
	}
	if m.driver == nil {
		return errors.New("heartbeat mod: driver is required")
	}
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("heartbeat mod: %w", err)
	}
	delivery, err := coreheartbeat.ParseDelivery(settings.Heartbeat.Delivery)
	if err != nil {
		return fmt.Errorf("heartbeat mod: %w", err)
	}
	if err := m.driver.SetDelivery(delivery); err != nil {
		return fmt.Errorf("heartbeat mod: %w", err)
	}
	return nil
}

// Provide 把 Nest 客户端与只读实体查找交给驱动，并登记驱动。业务时间不在这里接：驱动缺省读 fctx.Now，
// 一拍的 Nest 消息里是这条消息冻结的业务时间，它来自进程业务时钟——App 启动时按 time.logic_offset 设定，与
// app.BusinessClock(r) 同一份配置。
func (m *Mod) Provide(r *app.Registry) error {
	client, ok := app.Lookup[nest.Client](r, mods.ModNest)
	if !ok || client == nil {
		return errors.New("heartbeat mod: nest client is unavailable; install the nest mod in the same service")
	}
	manager := m.access.Manager()
	if err := m.driver.Bind(client, func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }); err != nil {
		return err
	}
	return mods.RegisterAll(r, mods.Capability{Name: mods.ModHeartbeat, Value: m.driver})
}

func (m *Mod) Start() error {
	if err := m.driver.Start(); err != nil {
		return err
	}
	m.started = true
	return nil
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }

// StopWithContext 停止驱动 goroutine。心跳不持久，停止不丢任何业务状态：Stop 返回后不再投递，已在 Nest 队列里的拍
// 由 Nest 照常执行或随 Nest 停机丢弃。
func (m *Mod) StopWithContext(ctx context.Context) error {
	if !m.started {
		return nil
	}
	m.started = false
	return m.driver.Stop(ctx)
}

var (
	_ app.Mod                   = (*Mod)(nil)
	_ app.ModConfigSchema       = (*Mod)(nil)
	_ app.ModDependencyProvider = (*Mod)(nil)
	_ app.ModStopperWithContext = (*Mod)(nil)
)
