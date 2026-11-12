package roost

import (
	"fmt"
	"go/format"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// moduleHostWiring 是 wiring/modulehost：托管服务进程里启动模块入口的 Mod。
const moduleHostWiring = "github.com/tjbdwanghaibo/roost-core/wiring/modulehost"

// sortedModuleRecordNames 是 roost.yaml 里已装模块的名字，按字典序。
func sortedModuleRecordNames(m Manifest) []string {
	return slices.Sorted(maps.Keys(m.Modules))
}

// hasModuleRuntime 报告业务服务 service 上有没有模块的启动入口（生成 modules_gen.go、bootstrap 用 NewWithModules）。
// 入口落在托管框架服务上的模块（runtime.framework）不算：它们走 wiring/modulehost，见 hostedModuleRuntimes。
func hasModuleRuntime(m Manifest, service string) bool {
	if m.isFrameworkService(service) {
		return false
	}
	for _, module := range m.Modules {
		if module.Runtime != nil && moduleRuntimeHost(m, module) == service {
			return true
		}
	}
	return false
}

// moduleRuntimeHost 是一个已装模块的启动入口所在的服务：不写 runtime.framework 时是它装进的业务服务；
// 写了就是托管那个框架服务的服务（没有、或不止一个时返回空串，generateModuleServices 报错）。
func moduleRuntimeHost(m Manifest, module InstalledModule) string {
	if module.Runtime == nil || module.Runtime.Framework == "" {
		return module.Service
	}
	host, _ := frameworkHostService(m, module.Runtime.Framework)
	return host
}

// frameworkHostService 找出托管 framework 这个框架服务的服务名。工程没有托管它、或者有两个服务都托管它时返回错误
// （后者说不清入口该在哪个进程里启动）。
func frameworkHostService(m Manifest, framework string) (string, error) {
	var hosts []string
	for _, name := range sortedServiceNames(m) {
		if strings.TrimSpace(m.Services[name].Framework) == framework {
			hosts = append(hosts, name)
		}
	}
	switch len(hosts) {
	case 1:
		return hosts[0], nil
	case 0:
		return "", fmt.Errorf("no service hosts the %s framework service (declare services.<name>.framework: %s in roost.yaml and run roost project sync)", framework, framework)
	default:
		return "", fmt.Errorf("services %s all host the %s framework service; a module runtime needs exactly one host", strings.Join(hosts, ", "), framework)
	}
}

// hostedModuleRuntimes 是在托管服务 service 的进程里启动的模块，按启动顺序；service 是业务服务、或上面没有模块时为空。
// 排序出错（清单被手工改坏）时也返回空：generateModuleServices 会报出同一个错误。
func hostedModuleRuntimes(m Manifest, service string) []string {
	if !m.isFrameworkService(service) {
		return nil
	}
	order, err := moduleRuntimeOrder(m, service)
	if err != nil {
		return nil
	}
	return order
}

// moduleRuntimeOrder 给出在 service 的进程里启动的模块，依赖在前、其余按名字。
// 只读安装记录，不重新读取模块库：安装之后的业务入口与依赖归工程所有。
// 依赖（requires）只决定同一个进程里的先后：依赖的模块入口在别的进程（或没有入口）时不参与排序，
// 但它必须装在同一个业务服务上。
func moduleRuntimeOrder(m Manifest, service string) ([]string, error) {
	var names, order []string
	for name, module := range m.Modules {
		if module.Runtime != nil && moduleRuntimeHost(m, module) == service {
			if !validName(name) {
				return nil, fmt.Errorf("invalid module name %q", name)
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	state := map[string]int{}
	// recorded 是这条依赖链上的模块共同归属的业务服务（安装记录的 service）：模块只装进一个业务服务，
	// 依赖必须装在同一个业务服务上——即使它们的入口不在同一个进程里。
	var visit func(name, recorded string) error
	visit = func(name, recorded string) error {
		if state[name] == 1 {
			return fmt.Errorf("module runtime dependency cycle at %s", name)
		}
		if state[name] == 2 {
			return nil
		}
		module, ok := m.Modules[name]
		if !ok || module.Service != recorded {
			return fmt.Errorf("module %s is not installed for service %s", name, recorded)
		}
		if err := validateModuleRuntime(module.Runtime); err != nil {
			return fmt.Errorf("module %s: %w", name, err)
		}
		state[name] = 1
		for _, dep := range module.Requires {
			if err := visit(dep, recorded); err != nil {
				return err
			}
		}
		state[name] = 2
		if module.Runtime != nil && moduleRuntimeHost(m, module) == service {
			order = append(order, name)
		}
		return nil
	}
	for _, name := range names {
		if err := visit(name, m.Modules[name].Service); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func renderModuleService(m Manifest, service string) (string, error) {
	order, err := moduleRuntimeOrder(m, service)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\npackage %s\n\nimport (\n\"context\"\n\"fmt\"\n\n\"github.com/tjbdwanghaibo/roost-core/framework/app\"\n\n", generatedHeader, m.servicePackage(service))
	for _, name := range order {
		fmt.Fprintf(&b, "%sModule %q\n", name, m.Project.Module+"/"+m.Modules[name].Runtime.Package)
	}
	b.WriteString(`)
// moduleService 嵌入实际 Service，保留它的 ConfigSchema、Nest 接线等可选接口。
// 模块在全部 Mod 启动后、业务 Init 前按依赖启动；业务先停，再反向停止模块。
type moduleService struct {
 *Service
 stops []moduleStop
 businessStarted bool
}
type moduleStop struct { name string; stop func(context.Context) error }
func NewWithModules() *moduleService { return &moduleService{Service:New()} }
func (s *moduleService) Init(registry *app.Registry) error {
`)
	for _, name := range order {
		fmt.Fprintf(&b, ` {
 stop,err:=%sModule.%s(registry)
 if stop!=nil {s.stops=append(s.stops,moduleStop{name:%q,stop:stop})}
 if err!=nil {return fmt.Errorf("start module %s: %%w",err)}
 }
`, name, m.Modules[name].Runtime.Start, name, name)
	}
	b.WriteString(` s.businessStarted=true
 return s.Service.Init(registry)
}
func (s *moduleService) Shutdown(ctx context.Context) error {
 // 下游未停完，不关闭它可能仍在使用的模块；保留回调供再次停机。
 if s.businessStarted {
  if err:=s.Service.Shutdown(ctx);err!=nil{return err}
  s.businessStarted=false
 }
 for len(s.stops)>0 {
  last:=len(s.stops)-1
  item:=s.stops[last]
  if err:=item.stop(ctx);err!=nil{return fmt.Errorf("stop module %s: %w",item.name,err)}
  s.stops[last]=moduleStop{}
  s.stops=s.stops[:last]
 }
 return nil
}
`)
	return b.String(), nil
}

// 模块安装也会调用 generate：在同一暂存区更新适配层和 bootstrap，失败不留下半套接线。
func generateModuleServices(root string, m Manifest) error {
	hosted := false
	for _, name := range sortedModuleRecordNames(m) {
		module := m.Modules[name]
		if module.Runtime == nil {
			continue
		}
		if _, ok := m.Services[module.Service]; !ok || m.isFrameworkService(module.Service) {
			return fmt.Errorf("module %s is recorded for %q, which is not a declared business service", name, module.Service)
		}
		if err := validateModuleRuntime(module.Runtime); err != nil {
			return fmt.Errorf("module %s: %w", name, err)
		}
		if module.Runtime.Framework != "" {
			// 入口在托管服务的进程里：那个服务必须恰好有一个。
			host, err := frameworkHostService(m, module.Runtime.Framework)
			if err != nil {
				return fmt.Errorf("module %s runtime: %w", name, err)
			}
			if _, err := moduleRuntimeOrder(m, host); err != nil {
				return err
			}
			hosted = true
		}
	}
	var services []string
	for _, name := range sortedServiceNames(m) {
		if hasModuleRuntime(m, name) {
			services = append(services, name)
		}
	}
	if len(services) == 0 && !hosted {
		return nil
	}
	// 与 project sync 使用同一份服务派生信息，不能重写 bootstrap 时丢掉实体 owner / 路由接线。
	m = m.withConfiguredShutdown(root).withServicePackages(root).withEntityOwners(root)
	for _, name := range services {
		body, err := renderModuleService(m, name)
		if err != nil {
			return err
		}
		formatted, err := format.Source([]byte(body))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(root, "internal/service", name, "modules_gen.go"), formatted, 0o644); err != nil {
			return err
		}
	}
	body, err := format.Source([]byte(renderBootstrap(m)))
	if err != nil {
		return err
	}
	path := filepath.Join(root, "internal/bootstrap/generated.go")
	if _, err := os.Stat(path); err != nil {
		return err
	}
	return writeAtomic(path, body, 0o644)
}
