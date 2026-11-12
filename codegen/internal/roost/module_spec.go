package roost

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

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
	Requires  []string       `yaml:"requires" json:"requires,omitempty"`
	Conflicts []string       `yaml:"conflicts" json:"conflicts,omitempty"`
	Needs     ModuleNeeds    `yaml:"needs" json:"needs"`
	Access    ModuleAccess   `yaml:"access" json:"access"`
	Runtime   *ModuleRuntime `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	// Sagas 复用 add saga 的定义与实体回执生成；步骤业务和启动入口由模块模板提供。
	Sagas []ModuleSaga `yaml:"sagas,omitempty" json:"sagas,omitempty"`
	// Services 声明新业务服务，或通过 existing 为已有服务追加 Mod/RPC。
	Services map[string]ModuleService `yaml:"services,omitempty" json:"services,omitempty"`
	// Entity 是模块状态与组件挂靠的实体（PascalCase，如 Player）。没有状态也没有组件的模块可以不写。
	Entity string `yaml:"entity" json:"entity,omitempty"`
	// Entities 是模块自带的实体：模板提供 Entity 与 DAO 定义，安装器分配实体号、补 lifecycle。
	// 与 Entity（挂靠目标）分开：例如 turn 可以挂到 room 模块拥有的 Room 上。
	Entities []string `yaml:"entities" json:"entities,omitempty"`
	// State 是追加到 Entity 主 DAO 末尾的顶层字段，类型是模块自带的嵌套结构体。
	// 同步与否按顶层字段决定：既有客户端可见状态又有服务端账本的模块写两个字段。
	State []ModuleStateField `yaml:"state" json:"state,omitempty"`
	// DAOs 把模块提供的独立 DAO 接到 Entity；加载与事务参与者由正式 entity/lifecycle 生成器接线。
	DAOs []ModuleDAOField `yaml:"daos" json:"daos,omitempty"`
	// Component 是挂在 Entity 上的组件名（PascalCase）；组件号在安装时分配。
	Component string `yaml:"component" json:"component,omitempty"`
	// Components 为自带实体的其他组件分配类型号，聚合字段由实体模板声明。
	Components []string `yaml:"components,omitempty" json:"components,omitempty"`
	// SyncPacker 把模块自带的有限视图打包器接到目标实体；不覆盖已有业务打包器。
	SyncPacker string `yaml:"sync_packer,omitempty" json:"sync_packer,omitempty"`
	// TimerTypes 在 Entity 的持久定时器中分配类型号；定时器组件提供 register<模块组件>Timers(*entitytimer.Handlers)。
	TimerTypes []string `yaml:"timer_types" json:"timer_types,omitempty"`
	// Protocols / Errcodes 是模板里 {{ID:protocol:<name>}} / {{ID:errcode:<name>}} 用到的名字，号在安装时分配。
	Protocols []string `yaml:"protocols" json:"protocols,omitempty"`
	Errcodes  []string `yaml:"errcodes" json:"errcodes,omitempty"`
	// Replaces 是模块要接管的生成桩（工程内路径，必须也在 files/ 里）：生成器建一次、默认拒绝一切、
	// 留给项目实现的文件，例如托管服务的 internal/service/chat/collaborators.go、玩家 TCP 的鉴权桩
	// internal/access/player/tcp/auth.go（安装器认识的种类见 generatedStubKinds）。只有它仍是没改过的
	// 生成桩时才替换；业务改过就拒绝安装。
	Replaces []string     `yaml:"replaces" json:"replaces,omitempty"`
	Verify   ModuleVerify `yaml:"verify" json:"verify"`
	// With 是与别的模块的联动：键是另一个模块的名字，文件在 with/<另一个模块>/ 下。两个模块都装着时
	// 才写进工程（不论先装哪个），用来把本模块接到对方留的登记点上；只装了其中一个时什么都不写。
	With map[string]ModuleWith `yaml:"with" json:"with,omitempty"`
	// Next 是安装之后还要工程自己做的事：安装器不代办的配置、要跑一次的运维命令、端点在哪里。每条一行，
	// `roost module plan / add` 在这次要装的模块下原样列出（只替换 {{PROJECT}}、{{GAME_SERVICE}} 这类
	// 工程名字占位，不能用 {{ID:...}}）。它是给人和 agent 看的提示，安装器从不执行它；
	// 完整的说明仍写在模块的 README 里，这里只放“装完马上要做的那几步”。
	Next []string `yaml:"next" json:"next,omitempty"`

	// Files 是模板在工程里的落点（不带 .tmpl），由 files/ 目录读出，不写在清单里。
	Files []string `yaml:"-" json:"files"`
	// Digest 是清单与全部模板内容的摘要，记进 roost.yaml：之后能看出已装的是不是这一版。
	Digest string `yaml:"-" json:"digest"`
}

// ModuleRuntime 声明模块的服务级启动入口。函数返回 stop（可为 nil）与 error；
// 返回 error 时仍可同时返回 stop，App 的失败停机路径会调用它。
type ModuleRuntime struct {
	Package string `yaml:"package" json:"package"` // 工程内 import 路径，如 game/skillbook
	Start   string `yaml:"start" json:"start"`     // func(*app.Registry) (func(context.Context) error, error)
	// Framework 不写时入口在模块装进的业务服务上启动（生成的 modules_gen.go，业务 Init 之前）。
	// 写一个托管框架服务的种类（roost.yaml 里 services.<名字>.framework 的取值，例如 account）时，入口改在
	// 托管那个框架服务的进程里启动：bootstrap 在它的 Mod 列表末尾加一个 wiring/modulehost 的 Mod，
	// 在全部 Mod 启动之后、Service.Init 之前调入口，停机按反序。这时清单的 needs.mods 是对那个托管服务的要求
	// （例如登录入口要 account 进程带 web Mod），不是对业务服务的。模块的文件仍按落点写进工程，
	// 安装记录的 service 仍是业务服务。
	Framework string `yaml:"framework,omitempty" json:"framework,omitempty"`
}

// ModuleNeeds 是模块对工程的前提。可确定接线的 Mod 由 moduleCanAddMod 代办，其余缺项给出接入命令。
type ModuleNeeds struct {
	Mods     []string `yaml:"mods" json:"mods,omitempty"`
	Features []string `yaml:"features" json:"features,omitempty"`
	// Uses 是业务服务必须 uses 的托管框架服务（如 rank、mail）。
	Uses     []string `yaml:"uses" json:"uses,omitempty"`
	UsesRPCs []string `yaml:"uses_rpcs" json:"uses_rpcs,omitempty"`
}

// ModuleAccess 是模块要求的玩家接入形态。与 Needs 不同，这些由安装器代办：它们是确定的框架骨架
// （roost add transport tcp）与一个开关（access.player.self_sync），没有项目要做的取舍。
type ModuleAccess struct {
	// Transport 为 tcp 时，工程还没有玩家 TCP 接入就先执行 roost add transport tcp。
	Transport string `yaml:"transport" json:"transport,omitempty"`
	// SelfSync 为 true 时打开 access.player.self_sync：主 DAO 里标了 sync 的字段推给玩家本人。
	SelfSync bool `yaml:"self_sync" json:"self_sync,omitempty"`
	// Lifecycle 为 true 时，Entity 还没有 lifecycle（game/lifecycle/<entity>.go，加载或创建实体的入口）
	// 就先执行 roost add lifecycle <Entity>。进入游戏的端点靠它建档。
	Lifecycle bool `yaml:"lifecycle" json:"lifecycle,omitempty"`
}

// ModuleStateField 是往主 DAO 追加的一个顶层字段。
type ModuleStateField struct {
	Field   string `yaml:"field" json:"field"`
	Type    string `yaml:"type" json:"type"`
	Store   string `yaml:"store" json:"store"`
	Dao     string `yaml:"dao" json:"dao"`
	Comment string `yaml:"comment" json:"comment,omitempty"`
}

// ModuleDAOField 引用工程 db 包中的 DAO（可为框架 DAO 的类型别名），不复制其状态。
type ModuleDAOField struct {
	Field string `yaml:"field" json:"field"`
	Type  string `yaml:"type" json:"type"`
	Tag   string `yaml:"tag" json:"tag"`
}

// ModuleWith 是模块与另一个模块的一组联动文件。
type ModuleWith struct {
	Summary string `yaml:"summary" json:"summary"`
	// Verify 是联动自带场景的验收口径，可以不写。测试名以 TestWith<本模块> 开头，
	// 与两个模块各自的 -run 都不重叠，所以两边各自的场景数不随对方装没装而变。
	Verify *ModuleVerify `yaml:"verify" json:"verify,omitempty"`
	// Files 是联动文件在工程里的落点（不带 .tmpl），由 with/<另一个模块>/ 目录读出。
	Files []string `yaml:"-" json:"files"`
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
	Digest   string         `yaml:"digest"`
	Service  string         `yaml:"service,omitempty"`
	Runtime  *ModuleRuntime `yaml:"runtime,omitempty"`
	Requires []string       `yaml:"requires,omitempty"`
	// TimerEntity 是 timer 类型号的作用域（实体 snake 名）；其他 ID 仍为工程级。
	TimerEntity string `yaml:"timer_entity,omitempty"`
	// IDs 是 kind -> 名字 -> 号（entity / protocol / errcode / component / timer）。
	IDs map[string]map[string]int64 `yaml:"ids,omitempty"`
	// State 是它追加的主 DAO 字段，写成 <Dao>.<Field>。
	State []string `yaml:"state,omitempty"`
	// DAOs 记录附加 DAO 的实体字段、类型和集合，便于归属核对。
	DAOs []string `yaml:"daos,omitempty"`
	// Files 是它写入的业务文件（工程内相对路径）。
	Files []string `yaml:"files,omitempty"`
	// With 是它已经写进工程的联动文件：另一个模块的名字 -> 文件。有键就表示这一对已经接过，不再处理。
	With map[string][]string `yaml:"with,omitempty"`
}

var (
	modulePascalPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	moduleStorePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// moduleIDPlaceholder 匹配模板里的 {{ID:<kind>:<name>}}。
	moduleIDPlaceholder = regexp.MustCompile(`\{\{ID:([a-z]+):([A-Za-z0-9_]+)\}\}`)
)

// moduleIDKinds 是模块能申请的 ID 种类；timer 在实体内分配，其他种类对应 roost.yaml 的 ids 段。
var moduleIDKinds = []string{"entity", "component", "errcode", "protocol", "timer"}

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
	// owners 是每个落点归谁：两个模块（或一个模块与一组联动）不能写同一个文件——安装时后写的会被
	// “落点已被占用”拒绝，同一次安装里则会互相覆盖。
	owners := map[string]string{}
	claim := func(rel, owner string) error {
		if previous, taken := owners[rel]; taken {
			return fmt.Errorf("%s and %s both ship %s", previous, owner, rel)
		}
		owners[rel] = owner
		return nil
	}
	for _, name := range sortedModuleNames(catalog) {
		spec := catalog[name]
		for _, other := range append(append([]string(nil), spec.Requires...), spec.Conflicts...) {
			if _, ok := catalog[other]; !ok {
				return nil, fmt.Errorf("module %s names unknown module %q", spec.Name, other)
			}
		}
		for _, rel := range spec.ownedFiles() {
			if err := claim(rel, "module "+name); err != nil {
				return nil, err
			}
		}
		for _, other := range spec.withNames() {
			if _, ok := catalog[other]; !ok {
				return nil, fmt.Errorf("module %s has with/%s, but there is no module %q", spec.Name, other, other)
			}
			for _, rel := range spec.With[other].Files {
				if err := claim(rel, fmt.Sprintf("module %s with %s", name, other)); err != nil {
					return nil, err
				}
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
	// with/<另一个模块>/ 下是联动文件；目录与清单的 with 段必须一一对应（validate 检查）。
	withRoot := path.Join(dir, "with")
	others, err := fs.ReadDir(source, withRoot)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ModuleSpec{}, fmt.Errorf("module %s: read with: %w", dir, err)
	}
	for _, other := range others {
		if !other.IsDir() {
			return ModuleSpec{}, fmt.Errorf("module %s: with/%s is not a directory; glue files live in with/<other module>/", dir, other.Name())
		}
		declared, ok := spec.With[other.Name()]
		if !ok {
			return ModuleSpec{}, fmt.Errorf("module %s: with/%s exists but module.yaml has no with.%s entry", dir, other.Name(), other.Name())
		}
		otherRoot := path.Join(withRoot, other.Name())
		err = fs.WalkDir(source, otherRoot, func(file string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			if !strings.HasSuffix(file, ".tmpl") {
				return fmt.Errorf("%s is not a .tmpl file", file)
			}
			body, readErr := fs.ReadFile(source, file)
			if readErr != nil {
				return readErr
			}
			rel := strings.TrimSuffix(strings.TrimPrefix(file, otherRoot+"/"), ".tmpl")
			declared.Files = append(declared.Files, rel)
			digest.Write([]byte("with/" + other.Name() + "/" + rel))
			digest.Write(body)
			return nil
		})
		if err != nil {
			return ModuleSpec{}, fmt.Errorf("module %s: read with/%s: %w", dir, other.Name(), err)
		}
		sort.Strings(declared.Files)
		spec.With[other.Name()] = declared
	}
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
	if (len(spec.State) > 0 || len(spec.DAOs) > 0 || spec.Component != "" || spec.SyncPacker != "") && !modulePascalPattern.MatchString(spec.Entity) {
		return fmt.Errorf("entity %q must be the PascalCase Entity the state and component attach to", spec.Entity)
	}
	if spec.Component != "" && !modulePascalPattern.MatchString(spec.Component) {
		return fmt.Errorf("component %q must be PascalCase", spec.Component)
	}
	if len(spec.TimerTypes) > 0 {
		if spec.Component == "" || !modulePascalPattern.MatchString(spec.Entity) {
			return fmt.Errorf("timer_types requires entity and component")
		}
		seen := map[string]bool{}
		for _, name := range spec.TimerTypes {
			if !modulePascalPattern.MatchString(name) || seen[name] {
				return fmt.Errorf("timer_types: %q must be a unique PascalCase name", name)
			}
			seen[name] = true
		}
	}
	if err := validateModuleRuntime(spec.Runtime); err != nil {
		return err
	}
	if err := validateModuleNext(spec.Next); err != nil {
		return err
	}
	if err := validateModuleSagas(spec); err != nil {
		return err
	}
	if err := validateModuleServices(spec); err != nil {
		return err
	}
	seenComponents := map[string]bool{spec.Component: true}
	for _, name := range spec.Components {
		if !modulePascalPattern.MatchString(name) || seenComponents[name] {
			return fmt.Errorf("components: invalid or duplicate name %q", name)
		}
		seenComponents[name] = true
	}
	seenEntities := map[string]bool{}
	for _, name := range spec.Entities {
		if !modulePascalPattern.MatchString(name) || seenEntities[name] {
			return fmt.Errorf("entities: %q must be a unique PascalCase name", name)
		}
		seenEntities[name] = true
		for _, rel := range moduleEntityDefinitions(name) {
			if !contains(spec.Files, rel) {
				return fmt.Errorf("entity %s requires template %s", name, rel)
			}
		}
		if contains(spec.Files, moduleEntityLifecycle(name)) {
			return fmt.Errorf("entity %s lifecycle is supplied by the installer, not files/", name)
		}
	}
	if spec.SyncPacker != "" && !modulePascalPattern.MatchString(spec.SyncPacker) {
		return fmt.Errorf("sync_packer %q must be an exported factory name", spec.SyncPacker)
	}
	prefix := toPascal(spec.Name)
	seenDAOFields, seenDAOTags := map[string]bool{}, map[string]bool{}
	for _, field := range spec.DAOs {
		if !token.IsIdentifier(field.Field) || field.Field == "_" || !strings.HasPrefix(field.Field, spec.Name) {
			return fmt.Errorf("DAO field %q must be a Go identifier starting with %s", field.Field, spec.Name)
		}
		if !modulePascalPattern.MatchString(field.Type) || !moduleStorePattern.MatchString(field.Tag) || !strings.HasPrefix(field.Tag, spec.Name) {
			return fmt.Errorf("DAO %s needs an exported db type and a tag starting with %s", field.Field, spec.Name)
		}
		if seenDAOFields[field.Field] || seenDAOTags[field.Tag] || field.Field == toSnake(spec.Component) {
			return fmt.Errorf("DAO %s duplicates a field, collection or component", field.Field)
		}
		seenDAOFields[field.Field], seenDAOTags[field.Tag] = true, true
	}
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
	if spec.Access.Transport != "" && spec.Access.Transport != "tcp" {
		return fmt.Errorf("access.transport %q is not supported; supported: tcp", spec.Access.Transport)
	}
	for _, rel := range spec.Replaces {
		if !contains(spec.Files, rel) {
			return fmt.Errorf("replaces %s, but files/ has no template for it", rel)
		}
	}
	if len(spec.Verify.Packages) == 0 || spec.Verify.Run == "" || spec.Verify.Pass <= 0 {
		return fmt.Errorf("verify needs packages, run and a positive pass count")
	}
	if _, err := regexp.Compile(spec.Verify.Run); err != nil {
		return fmt.Errorf("verify.run: %w", err)
	}
	declared := spec.declaredIDs()
	for _, other := range spec.withNames() {
		with := spec.With[other]
		if other == spec.Name || contains(spec.Conflicts, other) {
			return fmt.Errorf("with.%s: a module cannot have glue with itself or with a module it conflicts with", other)
		}
		if strings.TrimSpace(with.Summary) == "" {
			return fmt.Errorf("with.%s: summary is required", other)
		}
		if len(with.Files) == 0 {
			return fmt.Errorf("with.%s: with/%s/ has no .tmpl file", other, other)
		}
		if with.Verify != nil {
			if len(with.Verify.Packages) == 0 || with.Verify.Pass <= 0 {
				return fmt.Errorf("with.%s: verify needs packages, run and a positive pass count", other)
			}
			// 联动的场景与两个模块各自的场景分开数：名字不落进任何模块自己的 -run（^Test<Module>）。
			if want := "^TestWith" + prefix; !strings.HasPrefix(with.Verify.Run, want) {
				return fmt.Errorf("with.%s: verify.run %q must start with %s", other, with.Verify.Run, want)
			}
			if _, err := regexp.Compile(with.Verify.Run); err != nil {
				return fmt.Errorf("with.%s: verify.run: %w", other, err)
			}
		}
		for _, rel := range with.Files {
			body, err := fs.ReadFile(source, path.Join(spec.Name, "with", other, rel+".tmpl"))
			if err != nil {
				return err
			}
			// 联动文件只能用模块已经分配到的号：它可能在模块装好很久之后才写进工程。
			for _, match := range moduleIDPlaceholder.FindAllSubmatch(body, -1) {
				if kind, name := string(match[1]), string(match[2]); !declared[kind][name] {
					return fmt.Errorf("with/%s/%s uses {{ID:%s:%s}} but module.yaml does not declare it", other, rel, kind, name)
				}
			}
		}
	}
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

// withNames 是有联动文件的另一方模块名，按字典序。
func (spec ModuleSpec) withNames() []string {
	names := make([]string, 0, len(spec.With))
	for name := range spec.With {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// declaredIDs 是清单声明要分配的号：kind -> 名字集合。
func (spec ModuleSpec) declaredIDs() map[string]map[string]bool {
	declared := map[string]map[string]bool{"entity": {}, "component": {}, "errcode": {}, "protocol": {}, "timer": {}}
	for _, name := range spec.TimerTypes {
		declared["timer"][name] = true
	}
	for _, name := range spec.Entities {
		declared["entity"][name] = true
	}
	if spec.Component != "" {
		declared["component"][spec.Component] = true
	}
	for _, name := range spec.Components {
		declared["component"][name] = true
	}
	for _, name := range spec.Errcodes {
		declared["errcode"][name] = true
	}
	for _, name := range spec.Protocols {
		declared["protocol"][name] = true
	}
	return declared
}

func moduleEntityDefinitions(name string) []string {
	return []string{"game/entities/" + toSnake(name) + "/entity.go", "db/def/" + toSnake(name) + ".go"}
}

func moduleEntityLifecycle(name string) string {
	return "game/lifecycle/" + toSnake(name) + ".go"
}

// ownedFiles 包括模板和安装时补出的 lifecycle；计划、冲突检查、回滚与归属使用同一份清单。
func (spec ModuleSpec) ownedFiles() []string {
	files := append([]string(nil), spec.Files...)
	for _, saga := range spec.Sagas {
		files = append(files, moduleSagaDefinition(saga.Name))
	}
	for _, name := range spec.Entities {
		files = append(files, moduleEntityLifecycle(name))
	}
	sort.Strings(files)
	return files
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

// 安装后提示的上限：它是装完马上要做的几步，不是文档。超过的内容写进模块的 README。
const (
	moduleNextMaxHints = 8
	moduleNextMaxRunes = 240
)

// validateModuleNext 检查清单的 next：条数与每条的长度有上限，每条是非空的一行，不带 {{ID:...}}
// （提示里没有分配号的上下文，带了只会原样打印出一个占位符）。
func validateModuleNext(hints []string) error {
	if len(hints) > moduleNextMaxHints {
		return fmt.Errorf("next lists %d hints; at most %d (the rest belongs in the module's README)", len(hints), moduleNextMaxHints)
	}
	for index, hint := range hints {
		trimmed := strings.TrimSpace(hint)
		switch {
		case trimmed == "":
			return fmt.Errorf("next[%d] is empty", index)
		case strings.ContainsAny(trimmed, "\r\n"):
			return fmt.Errorf("next[%d] spans several lines; one hint is one line", index)
		case utf8.RuneCountInString(trimmed) > moduleNextMaxRunes:
			return fmt.Errorf("next[%d] is %d characters; at most %d", index, utf8.RuneCountInString(trimmed), moduleNextMaxRunes)
		case moduleIDPlaceholder.MatchString(trimmed):
			return fmt.Errorf("next[%d] uses an {{ID:...}} placeholder; hints only know the project names", index)
		}
	}
	return nil
}

var moduleRuntimePackagePattern = regexp.MustCompile(`^game/[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`)

func validateModuleRuntime(runtime *ModuleRuntime) error {
	if runtime == nil {
		return nil
	}
	if !moduleRuntimePackagePattern.MatchString(runtime.Package) {
		return fmt.Errorf("runtime.package %q must be a project-local game package", runtime.Package)
	}
	if !modulePascalPattern.MatchString(runtime.Start) {
		return fmt.Errorf("runtime.start %q must name an exported Go function", runtime.Start)
	}
	if runtime.Framework != "" {
		if _, known := frameworkCatalog[runtime.Framework]; !known {
			return fmt.Errorf("runtime.framework %q is not a framework service; known: %s", runtime.Framework, strings.Join(frameworkServiceNames(), ", "))
		}
	}
	return nil
}
