package roost

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ModuleService 是独立业务进程的装配声明。实体仍以 service= 标记限制写入方，
// 模块只代办服务和 RPC 接线，不把远端权威实体改成本地共享实体。
type ModuleService struct {
	// Existing 给已有服务追加 Mod/RPC 和缺少的配置段，不接管 service.go 或实体所有权。
	Existing bool     `yaml:"existing,omitempty" json:"existing,omitempty"`
	Mods     []string `yaml:"mods" json:"mods"`
	RPCs     []string `yaml:"rpcs" json:"rpcs,omitempty"`
	Entities []string `yaml:"entities" json:"entities,omitempty"`
}

func validateModuleServices(spec ModuleSpec) error {
	owners := map[string]string{}
	for name, service := range spec.Services {
		if !validName(name) {
			return fmt.Errorf("invalid module service %q", name)
		}
		if _, err := resolveMods(service.Mods); err != nil {
			return err
		}
		if service.Existing && len(service.Entities) != 0 {
			return fmt.Errorf("module service %s: existing service extension cannot take entity ownership", name)
		}
		if !service.Existing && !contains(spec.Files, "internal/service/"+name+"/service.go") {
			return fmt.Errorf("module service %s needs its service.go template", name)
		}
		for _, rpc := range service.RPCs {
			if !validName(rpc) || !contains(spec.Files, "internal/rpc/"+rpc+"/"+rpc+".go") {
				return fmt.Errorf("module service %s requires RPC %s template", name, rpc)
			}
		}
		for _, entity := range service.Entities {
			if !contains(spec.Entities, entity) || owners[entity] != "" {
				return fmt.Errorf("module service %s has missing or duplicate entity %s", name, entity)
			}
			owners[entity] = name
		}
	}
	return nil
}

// planModuleServices 只修改副本。新服务的全部模板在模块事务内写入，生成失败时与 manifest 一起撤销。
func planModuleServices(root string, manifest Manifest, target string, specs []ModuleSpec) (Manifest, error) {
	manifest.Services = maps.Clone(manifest.Services)
	manifest.Features = slices.Clone(manifest.Features)
	declared := map[string]bool{}
	for _, spec := range specs {
		if _, installed := manifest.Modules[spec.Name]; installed {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
			service := spec.Services[name]
			if service.Existing {
				existing, ok := manifest.Services[name]
				if !ok {
					return Manifest{}, fmt.Errorf("module %s extends missing service %s", spec.Name, name)
				}
				for _, rpc := range service.RPCs {
					if contains(existing.Rpcs, rpc) {
						return Manifest{}, fmt.Errorf("module %s: RPC %s already belongs to service %s", spec.Name, rpc, name)
					}
				}
				existing.Rpcs = uniqueSorted(append(slices.Clone(existing.Rpcs), service.RPCs...))
				existing.Mods = uniqueSorted(append(slices.Clone(existing.Mods), service.Mods...))
				if _, err := resolveMods(existing.Mods); err != nil {
					return Manifest{}, err
				}
				manifest.Services[name] = existing
				manifest.Features = uniqueSorted(append(manifest.Features, "rpc"))
				continue
			}
			if _, exists := manifest.Services[name]; exists {
				return Manifest{}, fmt.Errorf("module %s cannot own existing service %s", spec.Name, name)
			}
			for _, rel := range []string{"internal/service/" + name, "cmd/" + name, moduleServiceConfig(name)} {
				if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
					return Manifest{}, fmt.Errorf("module %s: existing service directory %s", spec.Name, rel)
				} else if !os.IsNotExist(err) {
					return Manifest{}, err
				}
			}
			manifest.Services[name] = ServiceSpec{Mods: uniqueSorted(service.Mods), Rpcs: uniqueSorted(service.RPCs)}
			if len(service.RPCs) > 0 {
				manifest.Features = uniqueSorted(append(manifest.Features, "rpc"))
			}
		}
		if len(spec.Needs.UsesRPCs) > 0 {
			service := manifest.Services[target]
			service.UsesRpcs = uniqueSorted(append(append([]string(nil), service.UsesRpcs...), spec.Needs.UsesRPCs...))
			manifest.Services[target] = service
		}
		if err := planModuleFrameworkServices(&manifest, target, spec, declared); err != nil {
			return Manifest{}, err
		}
	}
	return manifest.withPortBlocks(), nil
}

// planModuleFrameworkServices 补上模块用到、工程还没有的托管框架服务（维护者 2026-11-13：生成工程只带用到的服务）：
//
//   - needs.uses 的每一项：没有这个服务就声明 services.<name>.framework: <name>；并加进业务服务 target 的 uses；
//   - runtime.framework：工程里没有任何服务托管这个种类时同样声明并加进 uses；已有托管它的服务（名字可以不同）就用它；
//   - 同名的是业务服务时报错，不把业务进程改成托管进程。
//
// 入口落在托管服务上的模块（runtime.framework）要的可代办 Mod（needs.mods，例如 portal 的 web），
// 托管服务是这次才声明的，就直接写进新声明的 mods：前置命令（roost add mod）在模块事务之前、对着真实的 roost.yaml 执行，
// 那时这个服务还不存在。declared 记这次新声明的服务名，跨模块共用（login 声明 account、同一批的 portal 再给它加 web）。
// 新服务的配置、部署与协作者文件由安装最后一步的 project sync 写出，与 roost.yaml 一起在模块事务里提交或回滚。
func planModuleFrameworkServices(manifest *Manifest, target string, spec ModuleSpec, declared map[string]bool) error {
	declare := func(name string) error {
		if _, known := frameworkCatalog[name]; !known {
			return nil // checkModuleNeeds 报告。
		}
		added, err := declareFrameworkService(manifest, target, name)
		if err != nil {
			return fmt.Errorf("module %s: %w", spec.Name, err)
		}
		if added {
			declared[name] = true
		}
		return nil
	}
	for _, used := range spec.Needs.Uses {
		if err := declare(used); err != nil {
			return err
		}
	}
	if spec.Runtime == nil || spec.Runtime.Framework == "" {
		return nil
	}
	framework := spec.Runtime.Framework
	hosted := false
	for _, service := range manifest.Services {
		if strings.TrimSpace(service.Framework) == framework {
			hosted = true
		}
	}
	if !hosted {
		if err := declare(framework); err != nil {
			return err
		}
	}
	host, err := frameworkHostService(*manifest, framework)
	if err != nil || !declared[host] {
		return nil // 已有的托管服务缺 Mod 时走前置命令；托管服务不唯一由 checkModuleNeeds 报告。
	}
	hostSpec := manifest.Services[host]
	for _, mod := range spec.Needs.Mods {
		if moduleCanAddMod(mod) && !contains(hostSpec.Mods, mod) {
			hostSpec.Mods = uniqueSorted(append(append([]string(nil), hostSpec.Mods...), mod))
		}
	}
	manifest.Services[host] = hostSpec
	return nil
}

// newFrameworkServices 是 after 比 before 多出来的托管框架服务，按名字排序。
func newFrameworkServices(before, after Manifest) []string {
	added := []string{}
	for _, name := range sortedServiceNames(after) {
		if _, existed := before.Services[name]; !existed && after.isFrameworkService(name) {
			added = append(added, name)
		}
	}
	return added
}

func moduleServiceConfig(name string) string { return "configs/service/config." + name + ".yaml" }

// 新增业务服务与 game 在同一开发目录运行，且缺省 SID 都是 1000。开发配置不写 dataengine.wal.dir：
// 运行期缺省 data/wal/<服务>/<sid> 已按服务分目录；生产示例仍使用每实例独占的持久卷。
func writeModuleServiceConfigs(root string, before, manifest Manifest, spec ModuleSpec) error {
	for _, name := range slices.Sorted(maps.Keys(spec.Services)) {
		if spec.Services[name].Existing {
			if _, existed := before.Services[name]; existed {
				if _, err := appendModConfigSections(root, before, manifest, name, io.Discard); err != nil {
					return err
				}
			}
			continue
		}
		body := renderServiceConfig(manifest, name, false)
		if err := writeAtomic(filepath.Join(root, moduleServiceConfig(name)), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (spec ModuleSpec) entityService(entity, fallback string) string {
	for name, service := range spec.Services {
		if contains(service.Entities, entity) {
			return name
		}
	}
	return fallback
}

func modulePlanHasServices(plan ModulePlan) bool {
	for _, item := range plan.Modules {
		if item.Status == moduleStatusInstall && len(item.Services) > 0 {
			return true
		}
	}
	return false
}
