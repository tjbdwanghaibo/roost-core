package roost

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/modules"
	"gopkg.in/yaml.v3"
)

// 业务模块（roost module）：一个业务单元的清单与模板，源码在仓库 modules/<name>/。
// 生成器只产出框架骨架；模块是业务代码，由安装器写进工程，写入后归工程所有。
// 方案见 docs/maintenance/FEATURE-2026-11-10-business-modules.md。

// ModuleSpec 是 modules/<name>/module.yaml。未知键在解码时拒绝。
type ModuleSpec struct {
	Name    string `yaml:"name" json:"name"`
	Summary string `yaml:"summary" json:"summary"`
	// Requires 是必须先装的模块；Conflicts 是不能同装的模块。
	Requires  []string    `yaml:"requires" json:"requires,omitempty"`
	Conflicts []string    `yaml:"conflicts" json:"conflicts,omitempty"`
	Needs     ModuleNeeds `yaml:"needs" json:"needs"`
	// Entity 是模块状态与组件挂靠的实体（PascalCase，如 Player）。没有状态也没有组件的模块可以不写。
	Entity string `yaml:"entity" json:"entity,omitempty"`
	// State 是追加到 Entity 主 DAO 末尾的顶层字段，类型是模块自带的嵌套结构体。
	// 同步与否按顶层字段决定：既有客户端可见状态又有服务端账本的模块写两个字段。
	State []ModuleStateField `yaml:"state" json:"state,omitempty"`
	// Component 是挂在 Entity 上的组件名（PascalCase）；组件号在安装时分配。
	Component string `yaml:"component" json:"component,omitempty"`
	// Protocols / Errcodes 是模板里 {{ID:protocol:<name>}} / {{ID:errcode:<name>}} 用到的名字，号在安装时分配。
	Protocols []string     `yaml:"protocols" json:"protocols,omitempty"`
	Errcodes  []string     `yaml:"errcodes" json:"errcodes,omitempty"`
	Verify    ModuleVerify `yaml:"verify" json:"verify"`

	// Files 是模板在工程里的落点（不带 .tmpl），由 files/ 目录读出，不写在清单里。
	Files []string `yaml:"-" json:"files"`
	// Digest 是清单与全部模板内容的摘要，记进 roost.yaml：之后能看出已装的是不是这一版。
	Digest string `yaml:"-" json:"digest"`
}

// ModuleNeeds 是模块对工程的前提。安装器只检查、不替工程打开：缺什么给出要执行的命令。
type ModuleNeeds struct {
	Mods     []string `yaml:"mods" json:"mods,omitempty"`
	Features []string `yaml:"features" json:"features,omitempty"`
	// Uses 是业务服务必须 uses 的托管框架服务（如 rank、mail）。
	Uses []string `yaml:"uses" json:"uses,omitempty"`
}

// ModuleStateField 是往主 DAO 追加的一个顶层字段。
type ModuleStateField struct {
	Field   string `yaml:"field" json:"field"`
	Type    string `yaml:"type" json:"type"`
	Store   string `yaml:"store" json:"store"`
	Dao     string `yaml:"dao" json:"dao"`
	Comment string `yaml:"comment" json:"comment,omitempty"`
}

// ModuleVerify 是模块自带场景的验收口径：在 Packages 里跑 -run Run，恰好 Pass 条通过、零跳过。
type ModuleVerify struct {
	Packages []string `yaml:"packages" json:"packages"`
	Run      string   `yaml:"run" json:"run"`
	Pass     int      `yaml:"pass" json:"pass"`
}

// InstalledModule 是 roost.yaml 里 modules.<name> 的记录：这个模块在本工程里拥有什么。
// 它是归属清单的数据来源，也是重复安装判等的依据；分配过的号与存储键不回收。
type InstalledModule struct {
	Digest string `yaml:"digest"`
	// IDs 是 kind -> 名字 -> 号（protocol / errcode / component）。
	IDs map[string]map[string]int64 `yaml:"ids,omitempty"`
	// State 是它追加的主 DAO 字段，写成 <Dao>.<Field>。
	State []string `yaml:"state,omitempty"`
	// Files 是它写入的业务文件（工程内相对路径）。
	Files []string `yaml:"files,omitempty"`
}

var (
	modulePascalPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	moduleStorePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// moduleIDPlaceholder 匹配模板里的 {{ID:<kind>:<name>}}。
	moduleIDPlaceholder = regexp.MustCompile(`\{\{ID:([a-z]+):([A-Za-z0-9_]+)\}\}`)
)

// moduleIDKinds 是模块能申请的 ID 种类，与 roost.yaml 的 ids 段同名。
var moduleIDKinds = []string{"component", "errcode", "protocol"}

// loadModuleCatalog 读出全部内置模块，按名字排序。
func loadModuleCatalog() (map[string]ModuleSpec, error) {
	return loadModuleCatalogFS(modules.Files)
}

func loadModuleCatalogFS(source fs.FS) (map[string]ModuleSpec, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read module catalog: %w", err)
	}
	catalog := map[string]ModuleSpec{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		spec, err := loadModuleSpec(source, entry.Name())
		if err != nil {
			return nil, err
		}
		catalog[spec.Name] = spec
	}
	for _, spec := range catalog {
		for _, other := range append(append([]string(nil), spec.Requires...), spec.Conflicts...) {
			if _, ok := catalog[other]; !ok {
				return nil, fmt.Errorf("module %s names unknown module %q", spec.Name, other)
			}
		}
	}
	return catalog, nil
}

func loadModuleSpec(source fs.FS, dir string) (ModuleSpec, error) {
	raw, err := fs.ReadFile(source, path.Join(dir, "module.yaml"))
	if err != nil {
		return ModuleSpec{}, fmt.Errorf("module %s: %w", dir, err)
	}
	var spec ModuleSpec
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return ModuleSpec{}, fmt.Errorf("module %s: decode module.yaml: %w", dir, err)
	}
	if spec.Name != dir {
		return ModuleSpec{}, fmt.Errorf("module %s: module.yaml says name %q; the directory is the name", dir, spec.Name)
	}
	digest := sha256.New()
	digest.Write(raw)
	filesRoot := path.Join(dir, "files")
	err = fs.WalkDir(source, filesRoot, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		if !strings.HasSuffix(file, ".tmpl") {
			return fmt.Errorf("module %s: %s is not a .tmpl file", dir, file)
		}
		body, readErr := fs.ReadFile(source, file)
		if readErr != nil {
			return readErr
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(file, filesRoot+"/"), ".tmpl")
		spec.Files = append(spec.Files, rel)
		digest.Write([]byte(rel))
		digest.Write(body)
		return nil
	})
	// 没有 files/ 目录与空目录同样处理：交给 validate 报“files/ is empty”。
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ModuleSpec{}, fmt.Errorf("module %s: read files: %w", dir, err)
	}
	sort.Strings(spec.Files)
	spec.Digest = hex.EncodeToString(digest.Sum(nil))[:16]
	if err := spec.validate(source); err != nil {
		return ModuleSpec{}, fmt.Errorf("module %s: %w", dir, err)
	}
	return spec, nil
}

// validate 检查清单自身，以及清单与模板的一致：模板里用到的每个 ID 占位都在清单里声明过，
// 清单里声明的每个名字都被模板用到（多写一个就会白占一个号）。
func (spec ModuleSpec) validate(source fs.FS) error {
	if !validName(spec.Name) {
		return fmt.Errorf("invalid module name %q (lowercase letters, digits and _)", spec.Name)
	}
	if strings.TrimSpace(spec.Summary) == "" {
		return fmt.Errorf("summary is required")
	}
	if len(spec.Files) == 0 {
		return fmt.Errorf("files/ is empty")
	}
	for _, other := range append(append([]string(nil), spec.Requires...), spec.Conflicts...) {
		if other == spec.Name {
			return fmt.Errorf("a module cannot require or conflict with itself")
		}
	}
	if (len(spec.State) > 0 || spec.Component != "") && !modulePascalPattern.MatchString(spec.Entity) {
		return fmt.Errorf("entity %q must be the PascalCase Entity the state and component attach to", spec.Entity)
	}
	if spec.Component != "" && !modulePascalPattern.MatchString(spec.Component) {
		return fmt.Errorf("component %q must be PascalCase", spec.Component)
	}
	prefix := toPascal(spec.Name)
	seenFields := map[string]bool{}
	for _, field := range spec.State {
		// 字段名带模块名：两个模块不会往同一个 DAO 追加同名字段，归属一眼可见。
		if !modulePascalPattern.MatchString(field.Field) || !strings.HasPrefix(field.Field, prefix) {
			return fmt.Errorf("state field %q must be PascalCase and start with %s", field.Field, prefix)
		}
		if !modulePascalPattern.MatchString(field.Type) {
			return fmt.Errorf("state field %s: type %q must be a nested struct type the module defines in db/def", field.Field, field.Type)
		}
		if !moduleStorePattern.MatchString(field.Store) || !strings.HasPrefix(field.Store, spec.Name) {
			return fmt.Errorf("state field %s: store key %q must start with %s", field.Field, field.Store, spec.Name)
		}
		if !strings.Contains(field.Dao, "persist") {
			return fmt.Errorf("state field %s: dao %q must state persist / nopersist (and sync / nosync)", field.Field, field.Dao)
		}
		if seenFields[field.Field] {
			return fmt.Errorf("state field %s is listed twice", field.Field)
		}
		seenFields[field.Field] = true
	}
	if len(spec.Verify.Packages) == 0 || spec.Verify.Run == "" || spec.Verify.Pass <= 0 {
		return fmt.Errorf("verify needs packages, run and a positive pass count")
	}
	if _, err := regexp.Compile(spec.Verify.Run); err != nil {
		return fmt.Errorf("verify.run: %w", err)
	}
	declared := spec.declaredIDs()
	used := map[string]map[string]bool{}
	for _, rel := range spec.Files {
		body, err := fs.ReadFile(source, path.Join(spec.Name, "files", rel+".tmpl"))
		if err != nil {
			return err
		}
		for _, match := range moduleIDPlaceholder.FindAllSubmatch(body, -1) {
			kind, name := string(match[1]), string(match[2])
			if !declared[kind][name] {
				return fmt.Errorf("%s uses {{ID:%s:%s}} but module.yaml does not declare it", rel, kind, name)
			}
			if used[kind] == nil {
				used[kind] = map[string]bool{}
			}
			used[kind][name] = true
		}
	}
	for kind, names := range declared {
		for name := range names {
			if !used[kind][name] {
				return fmt.Errorf("module.yaml declares %s %q but no template uses {{ID:%s:%s}}", kind, name, kind, name)
			}
		}
	}
	return nil
}

// declaredIDs 是清单声明要分配的号：kind -> 名字集合。
func (spec ModuleSpec) declaredIDs() map[string]map[string]bool {
	declared := map[string]map[string]bool{"component": {}, "errcode": {}, "protocol": {}}
	if spec.Component != "" {
		declared["component"][spec.Component] = true
	}
	for _, name := range spec.Errcodes {
		declared["errcode"][name] = true
	}
	for _, name := range spec.Protocols {
		declared["protocol"][name] = true
	}
	return declared
}

// moduleInstallOrder 把 requested 连同它们的依赖排成安装顺序（依赖在前），并拒绝环与互斥。
func moduleInstallOrder(catalog map[string]ModuleSpec, requested []string) ([]ModuleSpec, error) {
	var order []ModuleSpec
	state := map[string]int{} // 1 = 正在访问，2 = 已排入
	var visit func(name string, from string) error
	visit = func(name, from string) error {
		spec, ok := catalog[name]
		if !ok {
			if from != "" {
				return fmt.Errorf("module %s requires unknown module %q", from, name)
			}
			return fmt.Errorf("unknown module %q; run: roost module list", name)
		}
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("module dependency cycle at %q", name)
		}
		state[name] = 1
		requires := append([]string(nil), spec.Requires...)
		sort.Strings(requires)
		for _, dependency := range requires {
			if err := visit(dependency, name); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, spec)
		return nil
	}
	for _, name := range requested {
		if err := visit(toSnake(name), ""); err != nil {
			return nil, err
		}
	}
	selected := map[string]bool{}
	for _, spec := range order {
		selected[spec.Name] = true
	}
	for _, spec := range order {
		for _, other := range spec.Conflicts {
			if selected[other] {
				return nil, fmt.Errorf("modules %s and %s cannot be installed together", spec.Name, other)
			}
		}
	}
	return order, nil
}

// sortedModuleNames 是目录里的模块名，按字典序。
func sortedModuleNames(catalog map[string]ModuleSpec) []string {
	names := make([]string, 0, len(catalog))
	for name := range catalog {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
