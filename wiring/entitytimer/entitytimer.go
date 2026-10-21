// Package entitytimer 把按实体的持久定时器驱动（framework/nest/entitytimer.Driver）接进 App：
// 依赖 Nest Mod，Provide 时取 Nest 客户端与 App 业务时钟交给驱动，Start 启动投递循环，Stop 先于 Nest 停止。
//
// 驱动本身（索引、投递、离线语义）在 core；这里只做接线，不持有运行状态。生成工程在 roost.yaml 写了
// timers（roost add timer --entity <Entity>）时，生成的 bootstrap 用 NewMod 装上各实体的驱动。
package entitytimer

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	coretimer "github.com/tjbdwanghaibo/roost-core/framework/nest/entitytimer"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod 装配一组实体定时器驱动。
type Mod struct {
	access  *entity.ManagerAccess
	drivers []*coretimer.Driver
	started []*coretimer.Driver
}

// NewMod 接收实例级 ManagerAccess（生成 bootstrap 的 EntityAccess，驱动只读其中已发布的实体，不加载）与
// 各实体组件生成的驱动变量。
func NewMod(access *entity.ManagerAccess, drivers ...*coretimer.Driver) *Mod {
	return &Mod{access: access, drivers: append([]*coretimer.Driver(nil), drivers...)}
}

func (m *Mod) Name() app.ModName { return mods.ModEntityTimer }

// DependsOn：投递走 Nest 客户端，Nest 先启动、后停止。
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModNest} }

func (m *Mod) Init(*viper.Viper) error {
	if m.access == nil || m.access.Manager() == nil {
		return errors.New("entity timer mod: entity access is required")
	}
	seen := make(map[string]bool, len(m.drivers))
	for _, driver := range m.drivers {
		if driver == nil {
			return errors.New("entity timer mod: nil driver")
		}
		if seen[driver.Name()] {
			return fmt.Errorf("entity timer mod: driver %s is installed twice", driver.Name())
		}
		seen[driver.Name()] = true
	}
	return nil
}

// Provide 把 Nest 客户端与只读实体查找交给每个驱动。业务时间不在这里接：驱动缺省读 fctx.Now（D22-1），Nest 消息
// 里是这条消息冻结的业务时间，它来自进程业务时钟——App 启动时按 time.logic_offset 设定，与 app.BusinessClock(r)
// 同一份配置。
func (m *Mod) Provide(r *app.Registry) error {
	client, ok := app.Lookup[nest.Client](r, mods.ModNest)
	if !ok || client == nil {
		return errors.New("entity timer mod: nest client is unavailable; install the nest mod in the same service")
	}
	manager := m.access.Manager()
	lookup := func(id int64) entity.IThreadSafeEntity { return manager.Get(id) }
	for _, driver := range m.drivers {
		if err := driver.Bind(client, lookup); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mod) Start() error {
	for _, driver := range m.drivers {
		if err := driver.Start(); err != nil {
			stopErr := m.StopWithContext(context.Background())
			return errors.Join(fmt.Errorf("entity timer mod: start %s: %w", driver.Name(), err), stopErr)
		}
		m.started = append(m.started, driver)
	}
	return nil
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }

// StopWithContext 停止投递循环。期限都在 DAO 里，停止不丢任何定时器：下次加载照样补触发。
func (m *Mod) StopWithContext(ctx context.Context) error {
	var joined error
	for _, driver := range m.started {
		joined = errors.Join(joined, driver.Stop(ctx))
	}
	m.started = nil
	return joined
}

var (
	_ app.Mod                   = (*Mod)(nil)
	_ app.ModDependencyProvider = (*Mod)(nil)
	_ app.ModStopperWithContext = (*Mod)(nil)
)
