package roost

import (
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func hasModuleRuntime(m Manifest, service string) bool {
	for _, module := range m.Modules {
		if module.Service == service && module.Runtime != nil {
			return true
		}
	}
	return false
}

// 只读安装记录，不重新读取模块库：安装之后的业务入口与依赖归工程所有。
func moduleRuntimeOrder(m Manifest, service string) ([]string, error) {
	var names, order []string
	for name, module := range m.Modules {
		if module.Service == service {
			if !validName(name) {
				return nil, fmt.Errorf("invalid module name %q", name)
			}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	state := map[string]int{}
	var visit func(string) error
	visit = func(name string) error {
		if state[name] == 1 {
			return fmt.Errorf("module runtime dependency cycle at %s", name)
		}
		if state[name] == 2 {
			return nil
		}
		module, ok := m.Modules[name]
		if !ok || module.Service != service {
			return fmt.Errorf("module %s is not installed for service %s", name, service)
		}
		if err := validateModuleRuntime(module.Runtime); err != nil {
			return fmt.Errorf("module %s: %w", name, err)
		}
		state[name] = 1
		for _, dep := range module.Requires {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		if module.Runtime != nil {
			order = append(order, name)
		}
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
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
	for name, module := range m.Modules {
		if module.Runtime == nil {
			continue
		}
		if _, ok := m.Services[module.Service]; !ok || m.isFrameworkService(module.Service) {
			return fmt.Errorf("module %s runtime requires a declared business service, got %q", name, module.Service)
		}
	}
	var services []string
	for _, name := range sortedServiceNames(m) {
		if hasModuleRuntime(m, name) {
			services = append(services, name)
		}
	}
	if len(services) == 0 {
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
