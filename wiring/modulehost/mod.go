// Package modulehost 让业务模块（roost module）的服务级启动入口跑在一个**托管的框架服务**进程里。
//
// 业务服务有自己的 Service：生成的适配层（internal/service/<服务>/modules_gen.go）在它的 Init 之前调模块的
// Start。托管服务（account、mail……）的 Service 是框架给的 Server，没有可以包一层的业务 Init，所以换一种挂法：
// 生成的 bootstrap 在这个服务的 Mod 列表末尾加一个本包的 Mod，由它在 Start 阶段调模块的入口。
//
// 时序与业务服务上的模块相同：
//
//   - 它依赖托管服务的 owner Mod，并排在本服务其余 Mod 之后启动，所以模块的 Start 运行时，本进程的
//     框架 Mod（含 owner Mod 发布的本地服务能力、web Mod 的路由登记器）都已经 Provide 并 Start；
//   - 它在 Service.Init 与 service.started 之前：模块往 web Mod 登记的路由赶在路由表冻结之前；
//   - 停机时它先于它依赖的 Mod 停（App 逆序停 Mod），模块按启动的反序停。
//
// 托管服务的固定装配（owner Mod、Server、它依赖的 Kit Mod）不因此改变：这个 Mod 只是多出来的最后一个。
package modulehost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// StartFunc 是模块的服务级启动入口（清单的 runtime）：返回停止函数（可为 nil）与错误。
// 返回错误时仍可同时返回停止函数——已经起了一半的资源由它收回。
type StartFunc func(*app.Registry) (func(context.Context) error, error)

// Module 是装在这个托管服务上的一个模块。
type Module struct {
	// Name 是模块名，只用于日志与错误。
	Name string
	// Start 是它的启动入口。
	Start StartFunc
}

// stopFallback 是 Stop（不带 context 的停止入口）给模块停止函数的时限。App 走 StopWithContext，
// 用的是它分给本 Mod 的停机预算；这个值只给直接调 Stop 的调用方。
const stopFallback = 3 * time.Second

// Mod 在托管服务的进程里按给定顺序启动模块、反序停止。用 NewMod 构造。
//
// 生命周期只走一轮（Provide → Start → Stop）。Start 失败时已经返回停止函数的模块留在待停列表里，
// App 的启动失败收尾会调 Stop / StopWithContext 把它们收回。停止函数返回错误时停在那里并保留它与它之前
// 启动的模块（后启动的可能还在用先启动的），再次停止从这里继续。
type Mod struct {
	service string
	owner   app.ModName
	modules []Module

	mu       sync.Mutex
	registry *app.Registry
	started  bool
	stops    []namedStop
}

type namedStop struct {
	name string
	stop func(context.Context) error
}

// NewMod 返回托管服务 service 的模块 Mod。owner 是这个服务的 owner Mod 的名字（各托管服务的
// CapabilityName）：模块要用它发布的本地服务，所以是硬依赖。modules 按启动顺序给出（生成器已经把
// 模块的依赖排在前面）。
func NewMod(service string, owner app.ModName, modules ...Module) *Mod {
	return &Mod{service: service, owner: owner, modules: append([]Module(nil), modules...)}
}

// Name 实现 app.Mod：modules.<服务名>。一个进程只有一个托管服务，所以不会重名。
func (m *Mod) Name() app.ModName { return app.ModName("modules." + m.service) }

// DependsOn 实现 app.ModDependencyProvider：托管服务的 owner Mod。
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{m.owner} }

// OptionalDependsOn 实现 app.ModOptionalDependencyProvider：模块常用的 Kit Mod 在场时排在本 Mod 之前。
// 是否必需由模块自己在 Start 里判断并报错（例如登录入口要 web Mod），这里只管顺序。
func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModWeb, mods.ModRedis, mods.ModNats}
}

// Init 实现 app.Mod。构造参数不对在这里报：进程不该带着一个没有入口的模块启动。
func (m *Mod) Init(*viper.Viper) error {
	if m.service == "" || m.owner == "" {
		return errors.New("modulehost: the hosted service and its owner mod are required")
	}
	seen := map[string]bool{}
	for _, module := range m.modules {
		if module.Name == "" || module.Start == nil {
			return fmt.Errorf("modulehost: service %s lists a module without a name or a start function", m.service)
		}
		if seen[module.Name] {
			return fmt.Errorf("modulehost: service %s lists module %s twice", m.service, module.Name)
		}
		seen[module.Name] = true
	}
	return nil
}

// Provide 实现 app.Mod：只记下 Registry。模块在 Start 阶段才启动——那时本服务的其余 Mod 都已经 Start。
func (m *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return errors.New("modulehost: app registry is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registry = registry
	return nil
}

// Start 实现 app.Mod：按顺序调每个模块的入口。任何一个失败就返回错误、不再启动后面的；
// 已经返回停止函数的（含失败的那一个）留给 Stop。
func (m *Mod) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.registry == nil {
		return errors.New("modulehost: Start before Provide")
	}
	if m.started {
		return fmt.Errorf("modulehost: modules of service %s were already started", m.service)
	}
	m.started = true
	for _, module := range m.modules {
		stop, err := module.Start(m.registry)
		if stop != nil {
			m.stops = append(m.stops, namedStop{name: module.Name, stop: stop})
		}
		if err != nil {
			return fmt.Errorf("start module %s on hosted service %s: %w", module.Name, m.service, err)
		}
	}
	return nil
}

// StopWithContext 实现 app.ModStopperWithContext：按启动的反序停模块，遵守 ctx。某个停止函数返回错误时
// 停在那里、返回它，保留这个模块与它之前启动的模块，再次调用从这里继续；已经停掉的不再停第二次。
func (m *Mod) StopWithContext(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.stops) > 0 {
		last := len(m.stops) - 1
		item := m.stops[last]
		if err := item.stop(ctx); err != nil {
			return fmt.Errorf("stop module %s on hosted service %s: %w", item.name, m.service, err)
		}
		m.stops[last] = namedStop{}
		m.stops = m.stops[:last]
	}
	return nil
}

// Stop 实现 app.Mod。App 认得 StopWithContext 时不会调它。
func (m *Mod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), stopFallback)
	defer cancel()
	_ = m.StopWithContext(ctx)
}

var (
	_ app.Mod                           = (*Mod)(nil)
	_ app.ModDependencyProvider         = (*Mod)(nil)
	_ app.ModOptionalDependencyProvider = (*Mod)(nil)
	_ app.ModStopperWithContext         = (*Mod)(nil)
)
