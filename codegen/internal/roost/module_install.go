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
	Component string   `json:"component,omitempty"`
	Files     []string `json:"files"`
	// Replaces 是 Files 里会替换掉的生成桩（仍是没改过的生成内容）。
	Replaces []string       `json:"replaces,omitempty"`
	Verify   ModuleVerify   `json:"verify"`
	Runtime  *ModuleRuntime `json:"runtime,omitempty"`

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
	service, err := nestService(manifest, options.Service, "roost module add", "modules run their business in Nest transactions")
	if err != nil {
		return ModulePlan{}, err
	}
	plan := ModulePlan{Service: service, Prerequisites: []string{}, Modules: []ModulePlanItem{}, Glue: []ModuleGlueItem{}}
	allocator, err := newModuleIDAllocator(root, manifest)
	if err != nil {
		return ModulePlan{}, err
	}
	pending := map[string]bool{}
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
			item.IDs, item.State, item.Files = installed.IDs, installed.State, installed.Files
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
		item.Status = moduleStatusInstall
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
		for _, field := range spec.State {
			item.State = append(item.State, fmt.Sprintf("%sDao.%s (%s)", spec.Entity, field.Field, field.Dao))
		}
		if spec.Component != "" {
			item.Component = spec.Entity + "." + spec.Component
		}
		for _, rel := range spec.Files {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
				body, renderErr := renderModuleFile(manifest, service, spec, "files", rel, item.IDs)
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

// checkModuleNeeds 检查模块对工程的前提。只检查不代办：改 roost.yaml 的装配是工程的决定。
func checkModuleNeeds(manifest Manifest, service string, spec ModuleSpec) error {
	var problems []string
	for _, feature := range spec.Needs.Features {
		if !contains(manifest.Features, feature) {
			problems = append(problems, fmt.Sprintf("feature %q is off: add it to features in roost.yaml and run roost project sync", feature))
		}
	}
	resolved, err := resolveMods(effectiveServiceMods(manifest, service))
	if err != nil {
		return err
	}
	for _, mod := range spec.Needs.Mods {
		if !contains(resolved, mod) && !contains(manifest.SharedMods, mod) {
			problems = append(problems, fmt.Sprintf("service %s has no %s mod: roost add mod %s -service %s", service, mod, mod, service))
		}
	}
	for _, used := range spec.Needs.Uses {
		if !contains(manifest.Services[service].Uses, used) {
			problems = append(problems, fmt.Sprintf("service %s does not use the %s framework service: declare services.%s.framework: %s and add %s to services.%s.uses in roost.yaml, then roost project sync (roost help services)", service, used, used, used, used, service))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("module %s cannot be installed yet:\n  - %s", spec.Name, strings.Join(problems, "\n  - "))
	}
	return nil
}

// modulePrerequisites 是模块要求、工程里还没有的框架骨架，写成会执行的 roost add 命令。
func modulePrerequisites(root string, manifest Manifest, service string, spec ModuleSpec, options ModuleOptions, newEntity bool) []string {
	var commands []string
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
		commands = append(commands, "set access.player.self_sync: true in roost.yaml; roost project sync")
	}
	if spec.Access.Lifecycle && !newEntity && !moduleLifecycleExists(root, spec.Entity) {
		commands = append(commands, fmt.Sprintf("roost add lifecycle %s --service %s", spec.Entity, service))
	}
	return commands
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
		record, err := writeModule(root, manifest, plan.Service, item)
		if err != nil {
			return fail(fmt.Errorf("module %s: %w", item.Name, err))
		}
		manifest.Modules[item.Name] = record
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
	if err := saveManifest(root, manifest); err != nil {
		return fail(err)
	}
	// 4. 生成。失败时生成物没有提交（暂存区），业务文件与 roost.yaml 恢复。
	if err := GenerateTransactional(ctx, root, GenerateOptions{Stdout: options.Stdout, syncProject: len(newEntities) > 0}, options.Stderr); err != nil {
		return fail(fmt.Errorf("generate after installing modules: %w", err))
	}
	plan.Applied = true
	return plan, nil
}

// ensureModulePrerequisites 经已有的 roost add 补齐模块要求的框架骨架；已经有的不动。
func ensureModulePrerequisites(root, service string, spec ModuleSpec, options ModuleOptions, newEntity bool) error {
	warnings := options.Stderr
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
	manifest, err := LoadManifest(root)
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
	if spec.Access.SelfSync && !options.SkipSelfSync {
		// 与手工改 roost.yaml 再 project sync 相同：写清单并同步，同步失败时清单恢复。
		manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			return err
		}
		if manifest, err = LoadManifest(root); err != nil {
			return err
		}
		if access := manifest.Access["player"]; !access.SelfSync {
			access.SelfSync = true
			manifest.Access["player"] = access
			if err := commitManifestSync(root, manifestBefore, manifest); err != nil {
				return fmt.Errorf("turn on access.player.self_sync: %w", err)
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
	if spec.Component != "" {
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
	for _, name := range spec.Entities {
		if _, err := addEntityLifecycle(root, manifest, AddOptions{Name: name, Service: service}); err != nil {
			return InstalledModule{}, err
		}
	}
	return record, nil
}

// renderModuleFile 渲染一个模板：工程名字占位、分配到的号，Go 文件再格式化一遍。
// dir 是模板在模块目录里的位置：模块自己的文件是 "files"，联动文件是 "with/<另一个模块>"。
func renderModuleFile(manifest Manifest, service string, spec ModuleSpec, dir, rel string, ids map[string]map[string]int64) ([]byte, error) {
	raw, err := fs.ReadFile(modules.Files, path.Join(spec.Name, dir, rel+".tmpl"))
	if err != nil {
		return nil, fmt.Errorf("module %s: read template %s: %w", spec.Name, rel, err)
	}
	body := demoVars{module: manifest.Project.Module, gameService: service, project: manifest.Project.Name}.apply(string(raw))
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
	// 已装模块记录里的号即使对应文件被业务删掉了也不再发出去：分配过的号不回收。
	for _, installed := range manifest.Modules {
		for kind, names := range installed.IDs {
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

// isUntouchedGeneratedStub 报告工程里的 rel 是否仍是生成器当初写下、之后没被改过的桩。
// 目前认识一种：托管框架服务的 internal/service/<name>/collaborators.go（默认拒绝一切，留给项目实现）。
// 判定是逐字节的：与生成器现在会写出的内容相同才算；改过一个字符就是业务代码。
func isUntouchedGeneratedStub(manifest Manifest, rel string, current []byte) (bool, error) {
	const prefix, suffix = "internal/service/", "/collaborators.go"
	if !strings.HasPrefix(rel, prefix) || !strings.HasSuffix(rel, suffix) {
		return false, nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(rel, prefix), suffix)
	if !manifest.isFrameworkService(name) {
		return false, nil
	}
	stub := renderFrameworkCollaborators(manifest, name)
	if bytes.Equal(current, []byte(stub)) {
		return true, nil
	}
	formatted, err := format.Source([]byte(stub))
	if err != nil {
		return false, fmt.Errorf("format the generated %s stub: %w", rel, err)
	}
	return bytes.Equal(current, formatted), nil
}
