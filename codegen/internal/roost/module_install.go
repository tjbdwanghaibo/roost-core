package roost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/modules"
)

// 模块安装器。plan 与 add 走同一条计算路径：planModules 只读工程、算出要做什么；
// AddModules 按计划执行。执行顺序：
//
//  1. 前置骨架（实体的主 DAO、玩家接入）经已有的 roost add 补齐——它们本身就是完整的工程状态；
//  2. 逐个模块：写入模板 → 往主 DAO 末尾追加状态字段 → 接组件 → 为自带实体补 lifecycle；
//  3. 把归属记录写进 roost.yaml 的 modules；
//  4. 运行全部生成器（GenerateTransactional：暂存区里生成，成功才提交）。
//
// 第 2～4 步任何一步失败，模块写过的业务文件与 roost.yaml 恢复到第 2 步之前；第 1 步的骨架保留。
// 已安装的模块（roost.yaml 里有记录）不再处理：重复执行没有变化。

// ModulePlan 是一次 plan / add 的结果，也是 --json 的输出（agent 读它）。
type ModulePlan struct {
	// Service 是模块装进的业务服务（运行 Nest 的那个）。
	Service string `json:"service"`
	// Prerequisites 是安装前要先执行的 roost add 命令（plan 只列出，add 会执行）。
	Prerequisites []string         `json:"prerequisites"`
	Modules       []ModulePlanItem `json:"modules"`
	// Glue 是这次要写的联动文件：两个模块都装着（其中至少一个是这次装的）时，把一方接到另一方的登记点上。
	Glue []ModuleGlueItem `json:"glue"`
	// Applied 为 true 表示这份计划已经执行（roost module add 的输出）。
	Applied bool `json:"applied"`
}

// ModulePlanItem 是计划里的一个模块。
type ModulePlanItem struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	// Status：install（将安装 / 已在本次安装）、installed（之前已装、与内置版本相同）、
	// installed_other_version（之前已装、内置模板之后变过；安装器不改动已装的模块）。
	Status   string   `json:"status"`
	Requires []string `json:"requires"`
	// IDs 是 kind -> 名字 -> 号。
	IDs map[string]map[string]int64 `json:"ids"`
	// State 是追加的主 DAO 字段（<Dao>.<Field> <dao 标签>）；Component 是接到实体上的组件（<Entity>.<Component>）。
	State     []string `json:"state"`
	DAOs      []string `json:"daos,omitempty"`
	Component string   `json:"component,omitempty"`
	Files     []string `json:"files"`
	// Replaces 是 Files 里会替换掉的生成桩（仍是没改过的生成内容）。
	Replaces []string                 `json:"replaces,omitempty"`
	Verify   ModuleVerify             `json:"verify"`
	Runtime  *ModuleRuntime           `json:"runtime,omitempty"`
	Sagas    []ModuleSaga             `json:"sagas,omitempty"`
	Services map[string]ModuleService `json:"services,omitempty"`
	Timer    *ModuleTimerPlan         `json:"timer,omitempty"`
	// Next 是清单里的安装后提示（ModuleSpec.Next），工程名字占位已经替换。只有这次要装的模块带着它：
	// 已装的模块不再提示。安装器不执行它们。
	Next []string `json:"next,omitempty"`

	spec ModuleSpec
}

// ModuleGlueItem 是一组联动文件：Module 的 with/<With>/ 下的文件，归 Module 所有。
type ModuleGlueItem struct {
	Module  string        `json:"module"`
	With    string        `json:"with"`
	Summary string        `json:"summary"`
	Files   []string      `json:"files"`
	Verify  *ModuleVerify `json:"verify,omitempty"`

	spec ModuleSpec
	ids  map[string]map[string]int64
}

const (
	moduleStatusInstall        = "install"
	moduleStatusInstalled      = "installed"
	moduleStatusInstalledOther = "installed_other_version"
)

// ModuleOptions 是 plan / add 的选项。
type ModuleOptions struct {
	// Service 在工程有多个运行 Nest 的业务服务时指定装进哪一个。
	Service string
	// SkipSelfSync 不替工程打开 access.player.self_sync（模块清单 access.self_sync 的那一步）。
	// 工程自己管 Player 的同步时用：一个 Nest 只有一个 EntitySync 观察者，自建了 entitysync Manager
	// （game-demo 的场景同步）再打开自身视图会在启动时冲突。
	SkipSelfSync bool
	Stdout       io.Writer
	Stderr       io.Writer
	// 新建配方已有外层依赖解析边界；普通 module add 仍随生成执行 tidy。
	deferDependencies bool
}

// PlanModules 算出安装 names（连同依赖）要做的事，不改动工程。
func PlanModules(root string, names []string, options ModuleOptions) (ModulePlan, error) {
	catalog, err := loadModuleCatalog()
	if err != nil {
		return ModulePlan{}, err
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		return ModulePlan{}, err
	}
	return planModules(root, manifest, catalog, names, options)
}

func planModules(root string, manifest Manifest, catalog map[string]ModuleSpec, names []string, options ModuleOptions) (ModulePlan, error) {
	if len(names) == 0 {
		return ModulePlan{}, errors.New("name at least one module; run: roost module list")
	}
	order, err := moduleInstallOrder(catalog, names)
	if err != nil {
		return ModulePlan{}, err
	}
	requestedService := options.Service
	if requestedService == "" {
		// 联盟等模块会增加 Nest 服务；后续 Player 模块仍默认装进既有的玩家接入服务。
		requestedService = manifest.Access["player"].Service
	}
	service, err := nestService(manifest, requestedService, "roost module add", "modules run their business in Nest transactions")
	if err != nil {
		return ModulePlan{}, err
	}
	manifest, err = planModuleServices(root, manifest, service, order)
	if err != nil {
		return ModulePlan{}, err
	}
	plan := ModulePlan{Service: service, Prerequisites: []string{}, Modules: []ModulePlanItem{}, Glue: []ModuleGlueItem{}}
	allocator, err := newModuleIDAllocator(root, manifest)
	if err != nil {
		return ModulePlan{}, err
	}
	timerPlans := map[string]*ModuleTimerPlan{}
	pending := map[string]bool{}
	pendingSagas := map[string]string{}
	newEntities := map[string]bool{}
	for _, spec := range order {
		if _, installed := manifest.Modules[spec.Name]; !installed {
			for _, name := range spec.Entities {
				newEntities[name] = true
			}
		}
	}
	for _, spec := range order {
		item := ModulePlanItem{
			Name: spec.Name, Summary: spec.Summary, Requires: append([]string{}, spec.Requires...), Runtime: spec.Runtime,
			IDs: map[string]map[string]int64{}, State: []string{}, Files: spec.ownedFiles(), Verify: spec.Verify, spec: spec,
		}
		if installed, ok := manifest.Modules[spec.Name]; ok {
			if installed.Service != service {
				return ModulePlan{}, fmt.Errorf("module %s belongs to service %s, not %s", spec.Name, installed.Service, service)
			}
			item.Runtime = installed.Runtime
			item.Requires = append([]string{}, installed.Requires...)
			item.Status = moduleStatusInstalled
			if installed.Digest != spec.Digest {
				item.Status = moduleStatusInstalledOther
			}
			item.IDs, item.State, item.DAOs, item.Files = installed.IDs, installed.State, installed.DAOs, installed.Files
			plan.Modules = append(plan.Modules, item)
			continue
		}
		for _, other := range spec.Conflicts {
			if _, installed := manifest.Modules[other]; installed {
				return ModulePlan{}, fmt.Errorf("module %s cannot be installed: it conflicts with installed module %s", spec.Name, other)
			}
		}
		if err := checkModuleNeeds(manifest, service, spec); err != nil {
			return ModulePlan{}, err
		}
		if spec.SyncPacker != "" && !newEntities[spec.Entity] {
			_, path, err := resolveEntitySource(root, spec.Entity)
			if err != nil {
				return ModulePlan{}, err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return ModulePlan{}, err
			}
			if _, err := moduleSyncMarker(raw, path, spec.Entity, spec.SyncPacker); err != nil {
				return ModulePlan{}, err
			}
		}
		if len(spec.DAOs) > 0 && !newEntities[spec.Entity] {
			if _, _, err := moduleDAOFields(root, manifest, spec); err != nil {
				return ModulePlan{}, err
			}
		}
		item.Status = moduleStatusInstall
		item.Next = moduleNextHints(manifest, service, spec)
		item.Sagas = spec.Sagas
		item.Services = spec.Services
		if err := checkModuleSagaConflicts(root, manifest, spec); err != nil {
			return ModulePlan{}, err
		}
		for _, definition := range spec.Sagas {
			if owner := pendingSagas[definition.Name]; owner != "" {
				return ModulePlan{}, fmt.Errorf("modules %s and %s both own saga %s", owner, spec.Name, definition.Name)
			}
			pendingSagas[definition.Name] = spec.Name
		}
		// 自带实体的定义和 lifecycle 必须全新，不能把工程已有的实体据为己有。
		for _, name := range spec.Entities {
			for _, rel := range append(moduleEntityDefinitions(name), moduleEntityLifecycle(name)) {
				if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
					return ModulePlan{}, fmt.Errorf("module %s: owned entity %s conflicts with existing %s", spec.Name, name, rel)
				} else if !os.IsNotExist(err) {
					return ModulePlan{}, err
				}
			}
		}
		for _, command := range modulePrerequisites(root, manifest, service, spec, options, newEntities[spec.Entity]) {
			if !pending[command] {
				pending[command] = true
				plan.Prerequisites = append(plan.Prerequisites, command)
			}
		}
		if item.IDs, err = allocator.allocate(spec); err != nil {
			return ModulePlan{}, fmt.Errorf("module %s: %w", spec.Name, err)
		}
		if len(spec.TimerTypes) > 0 {
			if item.Timer, err = planModuleTimer(root, manifest, service, spec, newEntities[spec.Entity], allocator, timerPlans, item.IDs); err != nil {
				return ModulePlan{}, fmt.Errorf("module %s: %w", spec.Name, err)
			}
		}
		for _, field := range spec.State {
			item.State = append(item.State, fmt.Sprintf("%sDao.%s (%s)", spec.Entity, field.Field, field.Dao))
		}
		item.DAOs = moduleDAONames(spec)
		if spec.Component != "" {
			item.Component = spec.Entity + "." + spec.Component
		}
		renderManifest := manifest
		if item.Timer != nil {
			renderManifest.Timers = maps.Clone(manifest.Timers)
			if renderManifest.Timers == nil {
				renderManifest.Timers = map[string]TimerSpec{}
			}
			renderManifest.Timers[item.Timer.Entity] = item.Timer.Spec
		}
		for _, rel := range spec.Files {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
				body, renderErr := renderModuleFile(renderManifest, service, spec, "files", rel, item.IDs)
				if renderErr != nil {
					return ModulePlan{}, renderErr
				}
				current, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if readErr != nil {
					return ModulePlan{}, readErr
				}
				if bytes.Equal(current, body) {
					continue
				}
				if contains(spec.Replaces, rel) {
					pristine, stubErr := isUntouchedGeneratedStub(manifest, rel, current)
					if stubErr != nil {
						return ModulePlan{}, stubErr
					}
					if pristine {
						item.Replaces = append(item.Replaces, rel)
						continue
					}
					return ModulePlan{}, fmt.Errorf("module %s: %s has been edited since it was generated; the module would replace it, and the installer does not overwrite business code (keep your implementation and do not install %s, or move your changes aside first)", spec.Name, rel, spec.Name)
				}
				return ModulePlan{}, fmt.Errorf("module %s: %s already exists and is not this module's file; the installer does not overwrite business files (rename or remove it, or keep your own implementation and do not install %s)", spec.Name, rel, spec.Name)
			} else if !os.IsNotExist(err) {
				return ModulePlan{}, err
			} else if contains(spec.Replaces, rel) && stubComesFromPrerequisite(manifest, rel, pending) {
				// 文件现在还没有，但这次安装的前置命令会先把桩生成出来、模块再替换它：计划里照样写明。
				item.Replaces = append(item.Replaces, rel)
			}
		}
		plan.Modules = append(plan.Modules, item)
	}
	if plan.Glue, err = planModuleGlue(root, manifest, catalog, service, plan.Modules); err != nil {
		return ModulePlan{}, err
	}
	return plan, nil
}

// planModuleGlue 算出这次要写的联动文件。一对模块的联动在第二个装上的那一次写：两个都在场
// （已装或这次装）、其中至少一个是这次装的、并且这一对还没有接过。两个都早已装好的不补——
// 已装的源码归工程所有，可能已经改过，安装器不回头往里加东西。
func planModuleGlue(root string, manifest Manifest, catalog map[string]ModuleSpec, service string, items []ModulePlanItem) ([]ModuleGlueItem, error) {
	present := map[string]bool{}
	installing := map[string]bool{}
	ids := map[string]map[string]map[string]int64{}
	for name, installed := range manifest.Modules {
		present[name] = true
		ids[name] = installed.IDs
	}
	for _, item := range items {
		present[item.Name] = true
		ids[item.Name] = item.IDs
		installing[item.Name] = item.Status == moduleStatusInstall
	}
	glue := []ModuleGlueItem{}
	for _, owner := range sortedModuleNames(catalog) {
		spec := catalog[owner]
		if !present[owner] {
			continue
		}
		for _, other := range spec.withNames() {
			if !present[other] || !(installing[owner] || installing[other]) {
				continue
			}
			if _, done := manifest.Modules[owner].With[other]; done {
				continue
			}
			with := spec.With[other]
			item := ModuleGlueItem{Module: owner, With: other, Summary: with.Summary, Files: append([]string{}, with.Files...), Verify: with.Verify, spec: spec, ids: ids[owner]}
			for _, rel := range with.Files {
				current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				body, err := renderModuleFile(manifest, service, spec, path.Join("with", other), rel, item.ids)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(current, body) {
					return nil, fmt.Errorf("module %s with %s: %s already exists and is not this glue file; the installer does not overwrite business files (rename or remove it)", owner, other, rel)
				}
			}
			glue = append(glue, item)
		}
	}
	return glue, nil
}

// checkModuleNeeds 检查不能自动补齐的前提。可代办的 Mod 复用 roost add mod，列入可审阅的前置命令。
func checkModuleNeeds(manifest Manifest, service string, spec ModuleSpec) error {
	var problems []string
	for _, feature := range spec.Needs.Features {
		if !contains(manifest.Features, feature) && feature != "attribute" && !(feature == "saga" && len(spec.Sagas) > 0) {
			problems = append(problems, fmt.Sprintf("feature %q is off: add it to features in roost.yaml and run roost project sync", feature))
		}
	}
	// needs.mods 对的是模块入口所在的服务：缺省是业务服务；runtime.framework 写了托管服务时是托管它的那个服务。
	modsService, err := moduleModsService(manifest, service, spec)
	if err != nil {
		return fmt.Errorf("module %s cannot be installed yet: %w", spec.Name, err)
	}
	resolved, err := resolveMods(effectiveServiceMods(manifest, modsService))
	if err != nil {
		return err
	}
	for _, mod := range spec.Needs.Mods {
		if !moduleModPresent(resolved, mod) && !contains(manifest.SharedMods, mod) && !moduleCanAddMod(mod) {
			problems = append(problems, fmt.Sprintf("service %s has no %s mod: roost add mod %s -service %s", modsService, mod, mod, modsService))
		}
	}
	for _, used := range spec.Needs.Uses {
		if !contains(manifest.Services[service].Uses, used) {
			problems = append(problems, fmt.Sprintf("service %s does not use the %s framework service: declare services.%s.framework: %s and add %s to services.%s.uses in roost.yaml, then roost project sync (roost help services)", service, used, used, used, used, service))
		}
	}
	for _, used := range spec.Needs.UsesRPCs {
		if !contains(manifest.Services[service].UsesRpcs, used) {
			problems = append(problems, fmt.Sprintf("service %s does not use RPC %s", service, used))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("module %s cannot be installed yet:\n  - %s", spec.Name, strings.Join(problems, "\n  - "))
	}
	return nil
}

// modulePrerequisites 列出缺少的框架骨架与本次会启用的能力；自身同步随模块原子安装。
func modulePrerequisites(root string, manifest Manifest, service string, spec ModuleSpec, options ModuleOptions, newEntity bool) []string {
	var commands []string
	if contains(spec.Needs.Features, "attribute") && !hasFeature(manifest, "attribute") {
		commands = append(commands, "enable features.attribute in roost.yaml; roost project sync")
	}
	if modsService, err := moduleModsService(manifest, service, spec); err == nil {
		for _, mod := range moduleMissingMods(manifest, service, spec) {
			commands = append(commands, "roost add mod "+mod+" -service "+modsService)
		}
	}
	for _, saga := range spec.Sagas {
		commands = append(commands, "install saga "+saga.Name+" definition and entity receipts in the module transaction")
	}
	for name := range spec.Services {
		commands = append(commands, "install business service "+name+" in the module transaction")
	}
	if len(spec.State) > 0 && !newEntity {
		if source, err := locateDaoStruct(root, spec.Entity+"Dao"); err == nil && source == nil {
			commands = append(commands, fmt.Sprintf("roost add dao %s --entity %s", spec.Entity, spec.Entity))
		}
	}
	access, hasAccess := manifest.Access["player"]
	needsAccess := len(spec.Protocols) > 0 || spec.Access.Transport != "" || spec.Access.SelfSync
	if needsAccess && !hasAccess {
		commands = append(commands, "roost add access player --service "+service)
	}
	if spec.Access.Transport != "" && !contains(access.Transports, spec.Access.Transport) {
		commands = append(commands, "roost add transport "+spec.Access.Transport)
	}
	if spec.Access.SelfSync && !access.SelfSync && !options.SkipSelfSync {
		commands = append(commands, "enable access.player.self_sync after module declarations; sync in the same installation transaction")
	}
	if spec.Access.Lifecycle && !newEntity && !moduleLifecycleExists(root, spec.Entity) {
		commands = append(commands, fmt.Sprintf("roost add lifecycle %s --service %s", spec.Entity, service))
	}
	return commands
}

// 只代办有确定接线和工程隔离配置的 Mod。外部依赖仍通过 plan 和开发环境配置明确展示。
// web（业务 HTTP 监听）在此列：接线是固定的一行，roost add mod 写下的 web.* 配置缺省只绑回环地址，
// 生产绑非回环要工程显式写 web.allow_public_addr，代办不会替工程把端点暴露出去。
func moduleCanAddMod(name string) bool {
	return name == "configdata" || name == "heartbeat" || name == "directory" || name == "remote_entity" || name == "remote_mirror" || name == "saga" || name == "web"
}

// moduleModsService 是清单的 needs.mods 所指的服务：模块的入口在哪个服务的进程里启动，Mod 就得在哪个服务上。
// 没有 runtime.framework 的模块是它装进的业务服务；有的是托管那个框架服务的服务（工程没有托管它时返回错误）。
func moduleModsService(manifest Manifest, service string, spec ModuleSpec) (string, error) {
	if spec.Runtime == nil || spec.Runtime.Framework == "" {
		return service, nil
	}
	return frameworkHostService(manifest, spec.Runtime.Framework)
}

func moduleModPresent(resolved []string, name string) bool {
	return contains(resolved, name) || name == "remote_mirror" && contains(resolved, "remote_entity")
}

func moduleMissingMods(manifest Manifest, service string, spec ModuleSpec) []string {
	service, err := moduleModsService(manifest, service, spec)
	if err != nil {
		return nil // checkModuleNeeds 会报告。
	}
	resolved, err := resolveMods(effectiveServiceMods(manifest, service))
	if err != nil {
		return nil // checkModuleNeeds 会报告配置错误。
	}
	var missing []string
	for _, mod := range spec.Needs.Mods {
		if moduleCanAddMod(mod) && !moduleModPresent(resolved, mod) && !contains(manifest.SharedMods, mod) {
			missing = append(missing, mod)
		}
	}
	return missing
}

// moduleNextHints 渲染清单的安装后提示：只替换工程名字占位（{{MODULE}}、{{PROJECT}}、{{GAME_SERVICE}}、
// {{GAME_SERVICE_PKG}}），与模板里的写法相同。没有提示时返回 nil，JSON 里不出现这个键。
func moduleNextHints(manifest Manifest, service string, spec ModuleSpec) []string {
	if len(spec.Next) == 0 {
		return nil
	}
	vars := demoVars{module: manifest.Project.Module, gameService: service, project: manifest.Project.Name}
	hints := make([]string, 0, len(spec.Next))
	for _, hint := range spec.Next {
		hints = append(hints, vars.apply(strings.TrimSpace(hint)))
	}
	return hints
}

// moduleLifecycleExists 报告实体的 lifecycle 文件是否已在（roost add lifecycle 的产物）。
func moduleLifecycleExists(root, entityName string) bool {
	_, err := os.Stat(filepath.Join(root, "game", "lifecycle", toSnake(entityName)+".go"))
	return err == nil
}

// AddModules 安装 names（连同依赖）。返回执行后的计划；Applied 为 true。
func AddModules(ctx context.Context, root string, names []string, options ModuleOptions) (ModulePlan, error) {
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	plan, err := PlanModules(root, names, options)
	if err != nil {
		return ModulePlan{}, err
	}
	installing := false
	newEntities := map[string]bool{}
	for _, item := range plan.Modules {
		installing = installing || item.Status == moduleStatusInstall
		if item.Status == moduleStatusInstall {
			for _, name := range item.spec.Entities {
				newEntities[name] = true
			}
		}
	}
	if !installing {
		plan.Applied = true
		return plan, nil
	}
	// 1. 前置骨架。它们各自是完整的工程状态，失败时不回退已经成功的那几条。
	for _, item := range plan.Modules {
		if item.Status != moduleStatusInstall {
			continue
		}
		if err := ensureModulePrerequisites(root, plan.Service, item.spec, options, newEntities[item.spec.Entity]); err != nil {
			return ModulePlan{}, fmt.Errorf("module %s: %w", item.Name, err)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		return ModulePlan{}, err
	}
	var serviceSpecs []ModuleSpec
	for _, item := range plan.Modules {
		if item.Status == moduleStatusInstall {
			serviceSpecs = append(serviceSpecs, item.spec)
		}
	}
	beforeServices := manifest
	manifest, err = planModuleServices(root, manifest, plan.Service, serviceSpecs)
	if err != nil {
		return ModulePlan{}, err
	}
	// 2～3. 模块自己的写入。先把会动到的文件全部记下来，失败时原样恢复。
	touched := []string{ManifestName}
	for _, item := range plan.Modules {
		if item.Status != moduleStatusInstall {
			continue
		}
		paths, err := moduleTouchedFiles(root, item.spec, newEntities[item.spec.Entity])
		if err != nil {
			return ModulePlan{}, fmt.Errorf("module %s: %w", item.Name, err)
		}
		touched = append(touched, paths...)
		for service := range item.Services {
			touched = append(touched, moduleServiceConfig(service), "configs/service/config."+service+".prod.example.yaml", kubernetesSecretExampleRel(service))
		}
		sagaPaths, err := moduleSagaTouchedFiles(root, item.spec, newEntities)
		if err != nil {
			return ModulePlan{}, err
		}
		touched = append(touched, sagaPaths...)
		if item.Timer != nil {
			touched = append(touched, newTimerNames(manifest.Project.Module, item.Timer.Entity, item.Timer.Spec).handlersFile())
			// 模块可能只带回调、没有自己的 State；新增定时器仍会改主 DAO，必须独立备份。
			if !newEntities[item.spec.Entity] {
				source, err := locateDaoStruct(root, item.Timer.Spec.Dao)
				if err != nil {
					return ModulePlan{}, err
				}
				if source == nil {
					return ModulePlan{}, fmt.Errorf("module %s timer DAO %s is missing", item.Name, item.Timer.Spec.Dao)
				}
				touched = append(touched, relativeSlash(root, source.path))
			}
		}
	}
	for _, glue := range plan.Glue {
		touched = append(touched, glue.Files...)
	}
	backups, err := captureFiles(root, touched)
	if err != nil {
		return ModulePlan{}, err
	}
	fail := func(cause error) (ModulePlan, error) {
		return ModulePlan{}, restoreFiles(root, backups, cause)
	}
	if manifest.Modules == nil {
		manifest.Modules = map[string]InstalledModule{}
	}
	for _, item := range plan.Modules {
		if item.Status != moduleStatusInstall {
			continue
		}
		// 类型号先放进 manifest 供模板渲染；DAO 与组件接线在模板写完后执行。
		if item.Timer != nil {
			if manifest.Timers == nil {
				manifest.Timers = map[string]TimerSpec{}
			}
			manifest.Timers[item.Timer.Entity] = item.Timer.Spec
		}
		record, err := writeModule(root, manifest, plan.Service, item)
		if err != nil {
			return fail(fmt.Errorf("module %s: %w", item.Name, err))
		}
		if item.Timer != nil {
			if err := wireModuleTimer(root, manifest, item); err != nil {
				return fail(fmt.Errorf("module %s timer: %w", item.Name, err))
			}
			record.TimerEntity = item.Timer.Entity
		}
		if err := writeModuleServiceConfigs(root, beforeServices, manifest, item.spec); err != nil {
			return fail(err)
		}
		manifest.Modules[item.Name] = record
	}
	// 模块的实体与组件全部就位后补回执；多个 Saga 共用一个实体回执适配。
	for _, item := range plan.Modules {
		if item.Status == moduleStatusInstall {
			if err := installModuleSagas(root, &manifest, item.spec); err != nil {
				return fail(err)
			}
		}
	}
	// 联动文件在全部模块写完之后写：它们同时引用两边的包。归属记在拥有它的那个模块名下。
	for _, glue := range plan.Glue {
		for _, rel := range glue.Files {
			body, err := renderModuleFile(manifest, plan.Service, glue.spec, path.Join("with", glue.With), rel, glue.ids)
			if err != nil {
				return fail(err)
			}
			if err := writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), body, 0o644); err != nil {
				return fail(fmt.Errorf("module %s with %s: %w", glue.Module, glue.With, err))
			}
		}
		record := manifest.Modules[glue.Module]
		if record.With == nil {
			record.With = map[string][]string{}
		}
		record.With[glue.With] = append([]string(nil), glue.Files...)
		manifest.Modules[glue.Module] = record
	}
	// 模块可能同时补 packer 与附加 DAO。全部业务声明就位后再开启自身同步，
	// 避免中途 project sync 用未完成的默认 packer 校验；开关也纳入同一次回滚。
	selfSyncChanged := false
	if !options.SkipSelfSync {
		for _, item := range plan.Modules {
			if item.Status != moduleStatusInstall || !item.spec.Access.SelfSync {
				continue
			}
			access := manifest.Access["player"]
			if !access.SelfSync {
				access.SelfSync = true
				manifest.Access["player"] = access
				selfSyncChanged = true
			}
		}
	}
	if err := saveManifest(root, manifest); err != nil {
		return fail(err)
	}
	// 4. 生成。失败时生成物没有提交（暂存区），业务文件与 roost.yaml 恢复。
	if err := GenerateTransactional(ctx, root, GenerateOptions{Stdout: options.Stdout, deferDependencies: options.deferDependencies, syncProject: selfSyncChanged || len(newEntities) > 0 || modulePlanHasTimers(plan) || modulePlanHasHostedRuntime(plan) || modulePlanHasServices(plan)}, options.Stderr); err != nil {
		return fail(fmt.Errorf("generate after installing modules: %w", err))
	}
	plan.Applied = true
	return plan, nil
}

// modulePlanHasHostedRuntime 报告这次要装的模块里有没有入口落在托管服务上的（runtime.framework）：
// 那个服务多了一个 Mod，它的停机预算（shutdown.total_timeout、部署的宽限期）要在同一次安装里刷新。
func modulePlanHasHostedRuntime(plan ModulePlan) bool {
	for _, item := range plan.Modules {
		if item.Status == moduleStatusInstall && item.Runtime != nil && item.Runtime.Framework != "" {
			return true
		}
	}
	return false
}

// ensureModulePrerequisites 经已有的 roost add 补齐模块要求的框架骨架；已经有的不动。
func ensureModulePrerequisites(root, service string, spec ModuleSpec, options ModuleOptions, newEntity bool) error {
	warnings := options.Stderr
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	// attribute 只开启正式生成能力，业务 profile 仍由模块提供，不先造一个要覆盖的示例。
	if contains(spec.Needs.Features, "attribute") && !hasFeature(manifest, "attribute") {
		before, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return err
		}
		manifest.Features = uniqueSorted(append(manifest.Features, "attribute"))
		if err := commitManifestSync(root, before, manifest); err != nil {
			return err
		}
	}
	if missing := moduleMissingMods(manifest, service, spec); len(missing) > 0 {
		modsService, err := moduleModsService(manifest, service, spec)
		if err != nil {
			return err
		}
		for _, mod := range missing {
			if err := addModuleMod(root, modsService, mod, warnings); err != nil {
				return fmt.Errorf("roost add mod %s -service %s: %w", mod, modsService, err)
			}
		}
	}
	if len(spec.State) > 0 && !newEntity {
		source, err := locateDaoStruct(root, spec.Entity+"Dao")
		if err != nil {
			return err
		}
		if source == nil {
			if _, err := Add(root, AddOptions{Kind: "dao", Name: spec.Entity, Entity: spec.Entity, Warnings: warnings}); err != nil {
				return fmt.Errorf("roost add dao %s --entity %s: %w", spec.Entity, spec.Entity, err)
			}
		}
	}
	if spec.Access.Lifecycle && !newEntity && !moduleLifecycleExists(root, spec.Entity) {
		if _, err := Add(root, AddOptions{Kind: "lifecycle", Name: spec.Entity, Service: service, Warnings: warnings}); err != nil {
			return fmt.Errorf("roost add lifecycle %s --service %s: %w", spec.Entity, service, err)
		}
	}
	needsAccess := len(spec.Protocols) > 0 || spec.Access.Transport != "" || spec.Access.SelfSync
	if !needsAccess {
		return nil
	}
	manifest, err = LoadManifest(root)
	if err != nil {
		return err
	}
	if _, enabled := manifest.Access["player"]; !enabled {
		if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: service, Warnings: warnings}); err != nil {
			return fmt.Errorf("roost add access player --service %s: %w", service, err)
		}
	}
	if spec.Access.Transport != "" {
		if manifest, err = LoadManifest(root); err != nil {
			return err
		}
		if !contains(manifest.Access["player"].Transports, spec.Access.Transport) {
			if _, err := Add(root, AddOptions{Kind: "transport", Name: spec.Access.Transport, Warnings: warnings}); err != nil {
				return fmt.Errorf("roost add transport %s: %w", spec.Access.Transport, err)
			}
		}
	}

	return nil
}

// moduleTouchedFiles 是安装一个模块会写的业务文件：模板的落点、实体的主 DAO 定义、实体聚合文件。
func moduleTouchedFiles(root string, spec ModuleSpec, newEntity bool) ([]string, error) {
	paths := spec.ownedFiles()
	if newEntity {
		// 依赖模块的实体也可能尚未写入；其固定落点已经由自带实体声明校验。
		return append(paths, moduleEntityDefinitions(spec.Entity)...), nil
	}
	if len(spec.State) > 0 {
		source, err := locateDaoStruct(root, spec.Entity+"Dao")
		if err != nil {
			return nil, err
		}
		if source == nil {
			return nil, fmt.Errorf("DAO %sDao is not defined in db/def", spec.Entity)
		}
		paths = append(paths, relativeSlash(root, source.path))
	}
	if spec.Component != "" || spec.SyncPacker != "" || len(spec.DAOs) > 0 {
		_, entityPath, err := resolveEntitySource(root, spec.Entity)
		if err != nil {
			return nil, err
		}
		paths = append(paths, relativeSlash(root, entityPath))
	}
	return paths, nil
}

// writeModule 先写模板，再追加字段、接组件，最后补自带实体的 lifecycle。所有写入都在外层回滚边界内。
func writeModule(root string, manifest Manifest, service string, item ModulePlanItem) (InstalledModule, error) {
	spec := item.spec
	record := InstalledModule{Digest: spec.Digest, IDs: item.IDs, Files: spec.ownedFiles(), Service: service, Runtime: spec.Runtime, Requires: append([]string(nil), spec.Requires...)}
	for _, rel := range spec.Files {
		body, err := renderModuleFile(manifest, service, spec, "files", rel, item.IDs)
		if err != nil {
			return InstalledModule{}, err
		}
		if err := writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), body, 0o644); err != nil {
			return InstalledModule{}, err
		}
	}
	if len(spec.State) > 0 {
		source, err := locateDaoStruct(root, spec.Entity+"Dao")
		if err != nil {
			return InstalledModule{}, err
		}
		if source == nil {
			return InstalledModule{}, fmt.Errorf("DAO %sDao is not defined in db/def", spec.Entity)
		}
		fields := make([]daoFieldSpec, 0, len(spec.State))
		for _, field := range spec.State {
			fields = append(fields, daoFieldSpec{Name: field.Field, Type: field.Type, Store: field.Store, Dao: field.Dao, Comment: field.Comment})
			record.State = append(record.State, spec.Entity+"Dao."+field.Field)
		}
		updated, err := source.appendFields(root, fields)
		if err != nil {
			return InstalledModule{}, err
		}
		if err := writeAtomic(source.path, updated, 0o644); err != nil {
			return InstalledModule{}, err
		}
	}
	if spec.Component != "" {
		entityName, entityPath, err := resolveEntitySource(root, spec.Entity)
		if err != nil {
			return InstalledModule{}, err
		}
		raw, err := os.ReadFile(entityPath)
		if err != nil {
			return InstalledModule{}, err
		}
		updated, _, err := wireComponentField(raw, entityPath, toPascal(entityName), toSnake(spec.Component), spec.Component)
		if err != nil {
			return InstalledModule{}, err
		}
		if err := writeAtomic(entityPath, updated, 0o644); err != nil {
			return InstalledModule{}, err
		}
	}
	if spec.SyncPacker != "" {
		if err := wireModuleSync(root, spec); err != nil {
			return InstalledModule{}, err
		}
	}
	if len(spec.DAOs) > 0 {
		path, updated, err := moduleDAOFields(root, manifest, spec)
		if err != nil {
			return InstalledModule{}, err
		}
		if err := writeAtomic(path, updated, 0o644); err != nil {
			return InstalledModule{}, err
		}
		record.DAOs = moduleDAONames(spec)
	}
	for _, name := range spec.Entities {
		if _, err := addEntityLifecycle(root, manifest, AddOptions{Name: name, Service: spec.entityService(name, service)}); err != nil {
			return InstalledModule{}, err
		}
	}
	return record, nil
}

func moduleDAONames(spec ModuleSpec) []string {
	var names []string
	for _, field := range spec.DAOs {
		names = append(names, fmt.Sprintf("%s.%s: %s (%s)", spec.Entity, field.Field, field.Type, field.Tag))
	}
	return names
}

// moduleDAOFields 在 plan 和 add 共用同一份冲突检查，plan 不写磁盘。
func moduleDAOFields(root string, manifest Manifest, spec ModuleSpec) (string, []byte, error) {
	_, path, err := resolveEntitySource(root, spec.Entity)
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	for _, field := range spec.DAOs {
		raw, err = wireNamedDAOField(raw, path, spec.Entity, manifest.Project.Module+"/db", field.Field, field.Tag, field.Type)
		if err != nil {
			return "", nil, fmt.Errorf("module %s: %w", spec.Name, err)
		}
	}
	return path, raw, nil
}

// renderModuleFile 渲染一个模板：工程名字占位、分配到的号，Go 文件再格式化一遍。
// dir 是模板在模块目录里的位置：模块自己的文件是 "files"，联动文件是 "with/<另一个模块>"。
func renderModuleFile(manifest Manifest, service string, spec ModuleSpec, dir, rel string, ids map[string]map[string]int64) ([]byte, error) {
	raw, err := fs.ReadFile(modules.Files, path.Join(spec.Name, dir, rel+".tmpl"))
	if err != nil {
		return nil, fmt.Errorf("module %s: read template %s: %w", spec.Name, rel, err)
	}
	body := demoVars{module: manifest.Project.Module, gameService: service, project: manifest.Project.Name}.apply(string(raw))
	if timer, ok := manifest.Timers[toSnake(spec.Entity)]; ok {
		body = strings.ReplaceAll(body, "{{TIMER_COMPONENT}}", newTimerNames(manifest.Project.Module, toSnake(spec.Entity), timer).CompType)
	}
	var missing []string
	body = moduleIDPlaceholder.ReplaceAllStringFunc(body, func(placeholder string) string {
		match := moduleIDPlaceholder.FindStringSubmatch(placeholder)
		id, ok := ids[match[1]][match[2]]
		if !ok {
			missing = append(missing, placeholder)
			return placeholder
		}
		return fmt.Sprintf("%d", id)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("module %s: %s: no id allocated for %s", spec.Name, rel, strings.Join(missing, ", "))
	}
	if strings.HasSuffix(rel, ".go") {
		formatted, err := format.Source([]byte(body))
		if err != nil {
			return nil, fmt.Errorf("module %s: format %s: %w", spec.Name, rel, err)
		}
		return formatted, nil
	}
	return []byte(body), nil
}

// moduleIDAllocator 在一次计划里给多个模块发号：先扫一遍工程里已用的号，之后发出去的号记在内存里，
// 所以同一次计划里的两个模块不会拿到同一个号（NextID 每次只看磁盘）。
type moduleIDAllocator struct {
	manifest Manifest
	used     map[string]map[int64]bool
}

func newModuleIDAllocator(root string, manifest Manifest) (*moduleIDAllocator, error) {
	uses, err := ScanIDs(root)
	if err != nil {
		return nil, err
	}
	allocator := &moduleIDAllocator{manifest: manifest, used: map[string]map[int64]bool{}}
	for _, use := range uses {
		allocator.reserve(use.Kind, use.ID)
	}
	for _, timer := range manifest.Timers {
		allocator.reserve("component", timer.ID)
	}
	// 已装模块记录里的号即使对应文件被业务删掉了也不再发出去：分配过的号不回收。
	for _, installed := range manifest.Modules {
		for kind, names := range installed.IDs {
			if kind == "timer" {
				continue
			}
			for _, id := range names {
				allocator.reserve(kind, id)
			}
		}
	}
	return allocator, nil
}

func (a *moduleIDAllocator) reserve(kind string, id int64) {
	if a.used[kind] == nil {
		a.used[kind] = map[int64]bool{}
	}
	a.used[kind][id] = true
}

// moduleProtocolGroup 是模块协议所在的协议组。模块不新开组。
const moduleProtocolGroup = "game"

func (a *moduleIDAllocator) next(kind string) (int64, error) {
	space, ok := a.manifest.IDs[kind]
	if !ok {
		return 0, fmt.Errorf("id space %q is not configured in roost.yaml", kind)
	}
	selected := IDRange{Min: space.Min, Max: space.Max}
	if kind == "protocol" {
		group, exists := space.Groups[moduleProtocolGroup]
		if !exists {
			return 0, fmt.Errorf("ids.protocol has no %q group in roost.yaml; modules allocate their messages there", moduleProtocolGroup)
		}
		selected = group
	}
	if selected.Min <= 0 || selected.Max < selected.Min {
		return 0, fmt.Errorf("id space %s has no usable range", kind)
	}
	for id := selected.Min; id <= selected.Max; id++ {
		if !a.used[kind][id] {
			a.reserve(kind, id)
			return id, nil
		}
	}
	return 0, fmt.Errorf("id space %s is exhausted", kind)
}

// allocate 按清单声明的名字发号，名字按字典序，所以同一份清单在同样的工程上总是得到同样的号。
func (a *moduleIDAllocator) allocate(spec ModuleSpec) (map[string]map[string]int64, error) {
	ids := map[string]map[string]int64{}
	declared := spec.declaredIDs()
	for _, kind := range moduleIDKinds {
		if kind == "timer" {
			continue
		}
		names := make([]string, 0, len(declared[kind]))
		for name := range declared[kind] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			id, err := a.next(kind)
			if err != nil {
				return nil, err
			}
			if ids[kind] == nil {
				ids[kind] = map[string]int64{}
			}
			ids[kind][name] = id
		}
	}
	return ids, nil
}

// daoStructSource 是 db/def 里一个 DAO 结构体的位置。
type daoStructSource struct {
	path      string
	raw       []byte
	fset      *token.FileSet
	structure *ast.StructType
	daoType   string
}

// daoFieldSpec 是要追加的一个 DAO 顶层字段。
type daoFieldSpec struct {
	Name, Type, Store, Dao, Comment string
}

// locateDaoStruct 在 db/def 的非测试 Go 文件里找名为 daoType 的结构体；没有时返回 nil。
func locateDaoStruct(root, daoType string) (*daoStructSource, error) {
	dir := filepath.Join(root, "db", "def")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read DAO definitions: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, raw, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", relativeSlash(root, file), err)
		}
		if structure := findStruct(parsed, daoType); structure != nil {
			return &daoStructSource{path: file, raw: raw, fset: fset, structure: structure, daoType: daoType}, nil
		}
	}
	return nil, nil
}

// fieldType 返回结构体里名为 name 的字段的类型与整段 tag；没有时 ok 为 false。
func (source *daoStructSource) fieldType(name string) (typeText, tag string, ok bool) {
	for _, field := range source.structure.Fields.List {
		for _, ident := range field.Names {
			if ident.Name != name {
				continue
			}
			if field.Tag != nil {
				tag = strings.Trim(field.Tag.Value, "`")
			}
			return exprString(source.fset, field.Type), tag, true
		}
	}
	return "", "", false
}

// appendFields 把 fields 追加到结构体末尾（文本插在右括号前，保留业务注释），返回格式化后的整个文件。
// 追加在末尾是契约：同步的 PB 字段号按定义顺序排，插在中间会让后面的字段号移位。
// 同名字段已经按同样的类型与标签存在时跳过；同名但类型或标签不同、存储键已被别的字段占用时拒绝。
func (source *daoStructSource) appendFields(root string, fields []daoFieldSpec) ([]byte, error) {
	storeOwners := map[string]string{}
	for _, field := range source.structure.Fields.List {
		if field.Tag == nil {
			continue
		}
		if store := reflectStructTag(strings.Trim(field.Tag.Value, "`"), "store"); store != "" {
			for _, ident := range field.Names {
				storeOwners[store] = ident.Name
			}
		}
	}
	var text strings.Builder
	for _, field := range fields {
		tag := fmt.Sprintf(`store:%q dao:%q`, field.Store, field.Dao)
		if existingType, existingTag, ok := source.fieldType(field.Name); ok {
			if existingType != field.Type || reflectStructTag(existingTag, "store") != field.Store || reflectStructTag(existingTag, "dao") != field.Dao {
				return nil, fmt.Errorf("%s: %s.%s already exists as %s `%s`; the module needs it as %s `%s`", relativeSlash(root, source.path), source.daoType, field.Name, existingType, existingTag, field.Type, tag)
			}
			continue
		}
		if owner, taken := storeOwners[field.Store]; taken {
			return nil, fmt.Errorf("%s: store key %q is already used by %s.%s", relativeSlash(root, source.path), field.Store, source.daoType, owner)
		}
		storeOwners[field.Store] = field.Name
		text.WriteString("\n")
		if comment := strings.TrimSpace(field.Comment); comment != "" {
			text.WriteString("\t// " + comment + "\n")
		}
		text.WriteString(fmt.Sprintf("\t%s %s `%s`\n", field.Name, field.Type, tag))
	}
	if text.Len() == 0 {
		return source.raw, nil
	}
	closing := source.fset.Position(source.structure.Fields.Closing).Offset
	edited := append(append(append([]byte(nil), source.raw[:closing]...), []byte(text.String())...), source.raw[closing:]...)
	formatted, err := format.Source(edited)
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", relativeSlash(root, source.path), err)
	}
	return formatted, nil
}

// generatedStubKind 是安装器认识的一种生成桩：生成器建一次、默认拒绝一切、留给项目实现的文件。
// 模块清单的 replaces 只能接管这里列出的种类；加一种新的桩就在 generatedStubKinds 里加一项。
type generatedStubKind struct {
	// name 给人看（测试与报错里点名是哪一种）。
	name string
	// render 返回生成器现在会给 rel 写出的桩内容（格式化之前）；rel 不是这一种桩时 ok 为 false。
	render func(manifest Manifest, rel string) (stub string, ok bool)
	// prerequisite 是会写下这种桩的前置命令（modulePrerequisites 给出的原文）；建工程时就有的桩为空。
	// 工程里还没有这个文件、而这次安装要先执行这条命令时，桩由它生成、随即被模块替换。
	prerequisite string
}

// generatedStubKinds 是安装器认识的全部生成桩种类。
var generatedStubKinds = []generatedStubKind{
	{name: "hosted framework service collaborators", render: frameworkCollaboratorsStub},
	{name: "player TCP authenticator", render: playerTCPAuthenticatorStub, prerequisite: "roost add transport tcp"},
}

// stubComesFromPrerequisite 报告 rel 是不是一个“现在还没有、但待执行的前置命令会生成”的桩。
func stubComesFromPrerequisite(manifest Manifest, rel string, pending map[string]bool) bool {
	for _, kind := range generatedStubKinds {
		if _, ok := kind.render(manifest, rel); ok {
			return kind.prerequisite != "" && pending[kind.prerequisite]
		}
	}
	return false
}

// frameworkCollaboratorsStub：托管框架服务的 internal/service/<name>/collaborators.go（默认拒绝一切）。
func frameworkCollaboratorsStub(manifest Manifest, rel string) (string, bool) {
	const prefix, suffix = "internal/service/", "/collaborators.go"
	if !strings.HasPrefix(rel, prefix) || !strings.HasSuffix(rel, suffix) {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(rel, prefix), suffix)
	if !manifest.isFrameworkService(name) {
		return "", false
	}
	return renderFrameworkCollaborators(manifest, name), true
}

// playerTCPAuthenticatorPath 是 roost add transport tcp 写下的鉴权桩（一律答 gateway.ErrUnauthenticated）。
const playerTCPAuthenticatorPath = "internal/access/player/tcp/auth.go"

// playerTCPAuthenticatorStub：玩家 TCP 接入的鉴权桩。内容与工程无关，所以不看 manifest。
func playerTCPAuthenticatorStub(_ Manifest, rel string) (string, bool) {
	if rel != playerTCPAuthenticatorPath {
		return "", false
	}
	return renderPlayerTCPAuthenticator(), true
}

// isUntouchedGeneratedStub 报告工程里的 rel 是否仍是生成器当初写下、之后没被改过的桩。
// 认识的种类见 generatedStubKinds；不是其中任何一种的文件一律不算桩。
// 判定是逐字节的：与生成器现在会写出的内容相同才算；改过一个字符就是业务代码。
func isUntouchedGeneratedStub(manifest Manifest, rel string, current []byte) (bool, error) {
	for _, kind := range generatedStubKinds {
		stub, ok := kind.render(manifest, rel)
		if !ok {
			continue
		}
		if bytes.Equal(current, []byte(stub)) {
			return true, nil
		}
		formatted, err := format.Source([]byte(stub))
		if err != nil {
			return false, fmt.Errorf("format the generated %s stub (%s): %w", rel, kind.name, err)
		}
		return bytes.Equal(current, formatted), nil
	}
	return false, nil
}
