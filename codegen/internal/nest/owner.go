package nest

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/template"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entityowner"
)

// 实体归属服务（//roost:entity service=<name>，维护者 2026-11-04 决定）在 Nest 生成里的两件事：
//
//  1. 锁住归属 kind 的 handler 只在归属服务注册。生成的 handler 文件给出 Register<File>NestHandlersOn(mgr)，
//     internal/registry/ownership_gen.go 按服务列出它们，并给出一个 Mod：归属服务的 bootstrap 装它，Provide 时
//     （Nest Mod 已构造引擎、全部 Provide 先于任何 Start）注册到本进程的引擎实例。别的服务不注册，发过去的
//     消息得到“handler 未注册”。未声明归属的 handler 照旧进包级 RegisterNestHandlers，所有进程都有。
//  2. ForeignKinds(service)：别的服务拥有的 kind，供 Remote Manager 在认领与写许可前拒绝（运行期守卫在框架里，
//     见 docs/maintenance/FEATURE-2026-11-04-alliance-service.md §2.10 第 1 项）。
//  3. RouteKinds(service) / PreloadKinds(service)：本服务拥有的 Remote 托管 kind（实体加载后在 Redis 登记路由）与
//     其中声明了 preload=startup 的 kind（起服预加载），同一次扫描交给本服务的 Remote Mod（方案 §2.7、§2.10 第 2 项）。
//
// handler 的归属按锁目标解析（维护者 2026-11-16 决定，整理轮 §3.1）：
//   - 写了 target=<实体名> 且能对上一个实体：按这个实体所在包的归属；
//   - view.<E>Entity（entity 生成器写在 game/view/entity_<实体>.go 的实体总接口）：实体 E 所在包的归属；
//   - view 里的组件窄接口（例如 view.IProfileEntity，嵌入 ProfileHolder）：找出嵌入了这些 Holder 的全部 <E>Entity，
//     它们的归属相同（或都没有）就用它，不同则生成失败，要求写 target=<实体名>；
//   - 类型来自 game/entities/<pkg>（或与 handler 同目录）：这个包里的实体声明了 service= 就归它。
// 同一个 handler 锁住两个不同服务的 kind 时没有进程能执行它，生成期拒绝；一个源文件里的 handler 归属必须一致，
// 因为注册按文件生成。

// ownerScan 是生成工程的归属声明（bootstrap 模式下一次扫描）。
type ownerScan struct {
	owned      []entityowner.Owned
	dirOwners  map[string]string
	moduleRoot string
	modulePath string
	// entities 是全部带标记的实体（含未声明归属的），用来把实体名对回所在包。
	entities []entityowner.Owned
	// viewImport 是 game/view 的 import 路径；viewIfaces 是 view 里每个接口嵌入的 <X>Holder（同包标识符）。
	viewImport string
	viewIfaces map[string][]string
}

func scanOwners(gameDir, moduleRoot, modulePath string) (ownerScan, error) {
	owned, err := entityowner.Scan(gameDir)
	if err != nil {
		return ownerScan{}, err
	}
	dirOwners, err := entityowner.DirOwners(owned)
	if err != nil {
		return ownerScan{}, err
	}
	scan := ownerScan{owned: owned, dirOwners: dirOwners, moduleRoot: moduleRoot, modulePath: modulePath}
	if len(dirOwners) == 0 {
		// 没有任何归属声明时 fileOwner 直接返回，不需要实体表与 view。
		return scan, nil
	}
	if scan.entities, err = entityowner.Entities(gameDir); err != nil {
		return ownerScan{}, err
	}
	viewDir := filepath.Join(gameDir, "view")
	if rel, relErr := filepath.Rel(moduleRoot, viewDir); relErr == nil {
		scan.viewImport = modulePath + "/" + filepath.ToSlash(rel)
	}
	if scan.viewIfaces, err = scanViewInterfaces(viewDir); err != nil {
		return ownerScan{}, err
	}
	return scan, nil
}

// scanViewInterfaces 用 go/parser 读 view 包（不依赖 entity 生成器的代码），返回每个接口类型嵌入的同包 <X>Holder。
// 目录不存在时返回空表。
func scanViewInterfaces(dir string) (map[string][]string, error) {
	out := map[string][]string{}
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok {
					continue
				}
				holders := []string{}
				for _, field := range iface.Methods.List {
					if ident, ok := field.Type.(*ast.Ident); ok && len(field.Names) == 0 && strings.HasSuffix(ident.Name, "Holder") {
						holders = append(holders, ident.Name)
					}
				}
				out[typeSpec.Name.Name] = holders
			}
		}
	}
	return out, nil
}

// fileOwner 返回 file（目录 fileDir）里全部 handler 的归属服务，没有归属时为 ""。
func (scan ownerScan) fileOwner(file string, fileDir string, funcs []*FuncInfo) (string, error) {
	if len(scan.dirOwners) == 0 {
		return "", nil
	}
	fileOwner, fileOwnerFrom := "", ""
	for _, fn := range funcs {
		owner := ""
		for _, param := range fn.Entities {
			service, err := scan.paramOwner(fileDir, fn.SourceImports, param)
			if err != nil {
				return "", fmt.Errorf("%s: handler %s parameter %s: %w", file, fn.RawName, param.Name, err)
			}
			if service == "" {
				continue
			}
			if owner != "" && owner != service {
				return "", fmt.Errorf("%s: handler %s locks kinds owned by service %s and service %s (//roost:entity service=); "+
					"no process can run it — split it, or send to the other service through its rpc", file, fn.RawName, owner, service)
			}
			owner = service
		}
		if owner != "" && fn.ReceiverType != "" {
			return "", fmt.Errorf("%s: method handler %s targets a kind owned by service %s; method handlers are registered by business code "+
				"and cannot be scoped to the owning service — use a package-level //roost:nest function", file, fn.RawName, owner)
		}
		if owner != "" && fn.NativeStep != "" {
			return "", fmt.Errorf("%s: handler %s is a saga native step on a kind owned by service %s; native steps on service-owned kinds are not supported", file, fn.RawName, owner)
		}
		if fn == funcs[0] {
			fileOwner, fileOwnerFrom = owner, fn.RawName
			continue
		}
		if owner != fileOwner {
			describe := func(service string) string {
				if service == "" {
					return "no owning service"
				}
				return "service " + service
			}
			return "", fmt.Errorf("%s: handler %s belongs to %s but handler %s in the same file belongs to %s; "+
				"registration is generated per file, so put handlers of different owners in different files",
				file, fn.RawName, describe(owner), fileOwnerFrom, describe(fileOwner))
		}
	}
	return fileOwner, nil
}

// paramOwner 解析一个锁目标参数的归属服务，见文件头的规则。target= 对不上任何实体时（例如语义名 target=RoomKind）
// 退回按类型解析。
func (scan ownerScan) paramOwner(fileDir string, imports []ImportInfo, param EntityParam) (string, error) {
	if param.Target != "" {
		if ent, ok := scan.entityNamed(param.Target); ok {
			return scan.dirOwners[ent.Dir], nil
		}
	}
	typeName := strings.TrimLeft(strings.TrimPrefix(param.Type, "[]"), "*")
	qualifier, name, qualified := strings.Cut(typeName, ".")
	if !qualified {
		return scan.dirOwners[fileDir], nil
	}
	for _, imp := range imports {
		alias := imp.Alias
		if alias == "" {
			alias = path.Base(imp.Path)
		}
		if alias != qualifier {
			continue
		}
		if scan.viewImport != "" && imp.Path == scan.viewImport {
			return scan.viewTypeOwner(name, param.Target)
		}
		rel, inModule := strings.CutPrefix(imp.Path, scan.modulePath+"/")
		if !inModule {
			return "", nil
		}
		return scan.dirOwners[filepath.Join(scan.moduleRoot, filepath.FromSlash(rel))], nil
	}
	return "", nil
}

// viewTypeOwner 解析 game/view 里的类型：实体总接口 <E>Entity 对应实体 E；组件窄接口按嵌入它的 Holder 的实体集合。
func (scan ownerScan) viewTypeOwner(name, target string) (string, error) {
	if entityName, ok := strings.CutSuffix(name, "Entity"); ok {
		if ent, ok := scan.entityExact(entityName); ok {
			return scan.dirOwners[ent.Dir], nil
		}
	}
	holders, ok := scan.viewIfaces[name]
	if !ok || len(holders) == 0 {
		// 定义还没生成（或不带 Holder）：I<X>Entity 按命名约定对应 <X>Holder。
		x, ok := strings.CutPrefix(name, "I")
		if !ok {
			return "", nil
		}
		x, ok = strings.CutSuffix(x, "Entity")
		if !ok || x == "" {
			return "", nil
		}
		holders = []string{x + "Holder"}
	}
	// 挂载这些组件的实体：它们的总接口 <E>Entity 嵌入了全部 holders。
	owners := map[string][]string{}
	for _, ent := range scan.entities {
		embedded := scan.viewIfaces[ent.Entity+"Entity"]
		if !containsAll(embedded, holders) {
			continue
		}
		service := scan.dirOwners[ent.Dir]
		owners[service] = append(owners[service], ent.Entity)
	}
	switch len(owners) {
	case 0:
		return "", nil
	case 1:
		for service := range owners {
			return service, nil
		}
	}
	services := make([]string, 0, len(owners))
	for service := range owners {
		services = append(services, service)
	}
	sort.Strings(services)
	var parts []string
	for _, service := range services {
		label := "no owning service"
		if service != "" {
			label = "service " + service
		}
		sort.Strings(owners[service])
		parts = append(parts, fmt.Sprintf("%s (%s)", strings.Join(owners[service], ", "), label))
	}
	hint := ""
	if target != "" {
		hint = fmt.Sprintf("; target=%s names no entity", target)
	}
	return "", fmt.Errorf("view.%s is held by entities of different owners: %s%s — write //roost:nest target=<entity> to pick the entity this handler locks",
		name, strings.Join(parts, "; "), hint)
}

// entityExact 按结构体名精确找实体（view.<E>Entity 的 E 就是结构体名）。
func (scan ownerScan) entityExact(name string) (entityowner.Owned, bool) {
	for _, ent := range scan.entities {
		if ent.Entity == name {
			return ent, true
		}
	}
	return entityowner.Owned{}, false
}

// entityNamed 按 target= 的写法找实体：结构体名（大小写不敏感）或实体包名（可带下划线），与 entityowner.ServiceOf 同一规则。
func (scan ownerScan) entityNamed(name string) (entityowner.Owned, bool) {
	for _, ent := range scan.entities {
		dir := filepath.Base(ent.Dir)
		if strings.EqualFold(ent.Entity, name) || strings.EqualFold(dir, name) ||
			strings.EqualFold(strings.ReplaceAll(dir, "_", ""), strings.ReplaceAll(name, "_", "")) {
			return ent, true
		}
	}
	return entityowner.Owned{}, false
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

type ownedService struct {
	Service string
	Funcs   []string // alias.Register<File>NestHandlersOn
}

type ownedKind struct {
	Expr    string // alias.EntityKindGuild
	Service string
	Preload bool // preload=startup
}

// Remote 托管的归属 kind 才进 ForeignKinds：RemoteEntityMod 在 Init 时要求它们是已注册的 Remote 托管 kind；
// 本地 kind 的 service= 只影响 handler 注册。

type ownershipFile struct {
	Imports  []bootstrapPackage
	Services []ownedService
	Kinds    []ownedKind
}

// generateOwnership 写 internal/registry/ownership_gen.go；没有任何 kind 声明归属时删除它（工程回到原行为）。
func generateOwnership(scan ownerScan, regs map[string][]bootstrapRegistration, outFile string, force bool) (bool, error) {
	if len(scan.owned) == 0 {
		if err := os.Remove(outFile); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}
	used := map[string]string{}
	aliasOf := func(importPath string) string {
		for alias, existing := range used {
			if existing == importPath {
				return alias
			}
		}
		alias := uniqueBootstrapAlias(importPath, used)
		used[alias] = importPath
		return alias
	}
	var data ownershipFile
	for _, service := range entityowner.Services(scan.owned) {
		entry := ownedService{Service: service}
		list := append([]bootstrapRegistration(nil), regs[service]...)
		sort.Slice(list, func(i, j int) bool {
			if list[i].ImportPath != list[j].ImportPath {
				return list[i].ImportPath < list[j].ImportPath
			}
			return list[i].RegisterFunc < list[j].RegisterFunc
		})
		for _, reg := range list {
			entry.Funcs = append(entry.Funcs, aliasOf(reg.ImportPath)+"."+reg.RegisterFunc+"On")
		}
		data.Services = append(data.Services, entry)
	}
	for _, item := range scan.owned {
		if !item.Remote {
			continue
		}
		importPath, err := importPathForDir(scan.moduleRoot, scan.modulePath, item.Dir)
		if err != nil {
			return false, err
		}
		kind := item.Kind
		if !strings.Contains(kind, ".") {
			kind = aliasOf(importPath) + "." + kind
		}
		data.Kinds = append(data.Kinds, ownedKind{Expr: kind, Service: item.Service, Preload: item.Preload})
	}
	for alias, importPath := range used {
		data.Imports = append(data.Imports, bootstrapPackage{Alias: alias, ImportPath: importPath})
	}
	sort.Slice(data.Imports, func(i, j int) bool { return data.Imports[i].ImportPath < data.Imports[j].ImportPath })
	tmpl, err := template.New("ownership").Parse(ownershipTemplate)
	if err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return false, err
	}
	content, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("format ownership: %w\n%s", err, buf.String())
	}
	if !force {
		if existing, err := os.ReadFile(outFile); err == nil && md5.Sum(existing) == md5.Sum(content) {
			return false, nil
		}
	}
	return true, os.WriteFile(outFile, content, 0o644)
}

const ownershipTemplate = `// Code generated by tool/nest. DO NOT EDIT.

package registry

import (
	"fmt"
	"slices"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
{{- range .Imports }}
	{{ .Alias }} "{{ .ImportPath }}"
{{- end }}
)

// remoteKindOwners 是声明了归属服务（//roost:entity service=<name>）的 Remote 托管 kind：只由归属服务写，
// 别的服务的 RemoteEntityMod 经 ForeignKinds 拒绝认领与写许可。本地 kind 的归属只影响 handler 注册，不在表里。
var remoteKindOwners = map[entity.EntityKind]string{
{{- range .Kinds }}
	{{ .Expr }}: "{{ .Service }}",
{{- end }}
}

// startupPreloadKinds 是声明了 preload=startup 的归属 Remote kind：归属服务起服时加载 owner 是本 sid 的这些实体
// 并登记路由。没有声明的 kind 只在第一次访问时冷加载。
var startupPreloadKinds = map[entity.EntityKind]bool{
{{- range .Kinds }}{{ if .Preload }}
	{{ .Expr }}: true,
{{- end }}{{ end }}
}

// ownedNestHandlers 按归属服务列出只在该服务的 Nest 引擎实例上注册的 handler（锁住归属 kind 的 handler）。
var ownedNestHandlers = map[string][]func(*nest.NestMgr) error{
{{- range .Services }}
	"{{ .Service }}": { {{- range $i, $f := .Funcs }}{{ if $i }}, {{ end }}{{ $f }}{{ end -}} },
{{- end }}
}

// ForeignKinds 返回别的服务拥有、service 不得写的 Remote 托管 kind（排序）。bootstrap 把它交给 service 的
// RemoteEntityMod（kitremoteentity.WithForeignKinds），认领与写许可前拒绝（entity.ErrRemoteKindNotOwned），
// 防止别的服务的进程先写到一个新实体、把它认领走。
func ForeignKinds(service string) []entity.EntityKind {
	var out []entity.EntityKind
	for kind, owner := range remoteKindOwners {
		if owner != service {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}

// RouteKinds 返回 service 拥有的 Remote 托管 kind（排序）：这些实体在 service 的实例上加载后，在 Redis 登记
// “实体 ID → sid + 所有权代际”的路由提示，//roost:rpc route 的调用方据此找到 owner（联盟服务方案 §2.7）。
func RouteKinds(service string) []entity.EntityKind {
	var out []entity.EntityKind
	for kind, owner := range remoteKindOwners {
		if owner == service {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
	return out
}

// PreloadKinds 返回 RouteKinds(service) 里声明了 preload=startup 的 kind（排序）：service 起服时、在路由 rpc 的
// handler 注册之前加载 owner 是本 sid 的这些实体。
func PreloadKinds(service string) []entity.EntityKind {
	var out []entity.EntityKind
	for _, kind := range RouteKinds(service) {
		if startupPreloadKinds[kind] {
			out = append(out, kind)
		}
	}
	return out
}

// NewOwnedNestHandlersMod 返回 service 的归属 handler 注册 Mod。bootstrap 只给拥有 kind 的服务装它：Provide 时
// （Nest Mod 已构造引擎，全部 Provide 先于任何 Start）把 ownedNestHandlers[service] 注册到本进程的引擎实例。
func NewOwnedNestHandlersMod(service string) app.Mod { return &ownedNestHandlersMod{service: service} }

type ownedNestHandlersMod struct{ service string }

func (m *ownedNestHandlersMod) Name() app.ModName { return app.ModName("nest.owned_handlers") }

// DependsOn 让 Provide 排在 Nest Mod 之后（引擎在那里构造）。
func (m *ownedNestHandlersMod) DependsOn() []app.ModName { return []app.ModName{mods.ModNest} }

func (m *ownedNestHandlersMod) Init(*viper.Viper) error { return nil }

func (m *ownedNestHandlersMod) Provide(r *app.Registry) error {
	engine, ok := app.Lookup[*nest.NestMgr](r, mods.ModNest)
	if !ok || engine == nil {
		return fmt.Errorf("owned nest handlers for service %s: capability %q not found; the service needs the nest mod", m.service, mods.ModNest)
	}
	for _, register := range ownedNestHandlers[m.service] {
		if err := register(engine); err != nil {
			return fmt.Errorf("owned nest handlers for service %s: %w", m.service, err)
		}
	}
	return nil
}

func (m *ownedNestHandlersMod) Start() error { return nil }
func (m *ownedNestHandlersMod) Stop()        {}

var (
	_ app.Mod                   = (*ownedNestHandlersMod)(nil)
	_ app.ModDependencyProvider = (*ownedNestHandlersMod)(nil)
)
`
