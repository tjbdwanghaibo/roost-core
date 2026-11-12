package roost

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	}
	return manifest.withPortBlocks(), nil
}

func moduleServiceConfig(name string) string { return "configs/service/config." + name + ".yaml" }

// 新增业务服务与 game 在同一开发目录运行，且缺省 SID 都是 1000。
// 配置随模块一起安装，给每个服务自己的 WAL 目录；生产示例仍使用每实例独占的持久卷。
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
		if loc := dataEngineWALSection.FindStringIndex(body); loc != nil {
			body = body[:loc[1]] + "    dir: data/wal/" + name + "\n" + body[loc[1]:]
		}
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
