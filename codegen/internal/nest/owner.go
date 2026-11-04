package nest

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"go/format"
	"os"
	"path"
	"path/filepath"
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
//
// handler 的归属按目标参数类型所在的包解析：类型来自 game/entities/<pkg>，这个包里的实体声明了 service= 就归它。
// 同一个 handler 锁住两个不同服务的 kind 时没有进程能执行它，生成期拒绝；一个源文件里的 handler 归属必须一致，
// 因为注册按文件生成。

// ownerScan 是生成工程的归属声明（bootstrap 模式下一次扫描）。
type ownerScan struct {
	owned      []entityowner.Owned
	dirOwners  map[string]string
	moduleRoot string
	modulePath string
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
	return ownerScan{owned: owned, dirOwners: dirOwners, moduleRoot: moduleRoot, modulePath: modulePath}, nil
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
			service := scan.typeOwner(fileDir, fn.SourceImports, param.Type)
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

// typeOwner 解析 handler 目标参数类型（guild.IGuildEntity / *guild.Guild / 本包的 IFoo）所在包的归属服务。
func (scan ownerScan) typeOwner(fileDir string, imports []ImportInfo, typeName string) string {
	typeName = strings.TrimLeft(strings.TrimPrefix(typeName, "[]"), "*")
	qualifier, _, qualified := strings.Cut(typeName, ".")
	if !qualified {
		return scan.dirOwners[fileDir]
	}
	for _, imp := range imports {
		alias := imp.Alias
		if alias == "" {
			alias = path.Base(imp.Path)
		}
		if alias != qualifier {
			continue
		}
		rel, inModule := strings.CutPrefix(imp.Path, scan.modulePath+"/")
		if !inModule {
			return ""
		}
		return scan.dirOwners[filepath.Join(scan.moduleRoot, filepath.FromSlash(rel))]
	}
	return ""
}

type ownedService struct {
	Service string
	Funcs   []string // alias.Register<File>NestHandlersOn
}

type ownedKind struct {
	Expr    string // alias.EntityKindGuild
	Service string
}

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
		importPath, err := importPathForDir(scan.moduleRoot, scan.modulePath, item.Dir)
		if err != nil {
			return false, err
		}
		kind := item.Kind
		if !strings.Contains(kind, ".") {
			kind = aliasOf(importPath) + "." + kind
		}
		data.Kinds = append(data.Kinds, ownedKind{Expr: kind, Service: item.Service})
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

// entityOwners 是声明了归属服务的 kind（//roost:entity service=<name>）：这些 kind 只由归属服务写。
// 没有列出的 kind 保持原行为，任何带 Nest 的服务都能写。
var entityOwners = map[entity.EntityKind]string{
{{- range .Kinds }}
	{{ .Expr }}: "{{ .Service }}",
{{- end }}
}

// ownedNestHandlers 按归属服务列出只在该服务的 Nest 引擎实例上注册的 handler（锁住归属 kind 的 handler）。
var ownedNestHandlers = map[string][]func(*nest.NestMgr) error{
{{- range .Services }}
	"{{ .Service }}": { {{- range $i, $f := .Funcs }}{{ if $i }}, {{ end }}{{ $f }}{{ end -}} },
{{- end }}
}

// ForeignKinds 返回别的服务拥有、service 不得写的 kind（排序）。Remote Manager 用它在认领与写许可前拒绝，
// 防止别的服务的进程先写到一个新实体、把它认领走。
func ForeignKinds(service string) []entity.EntityKind {
	var out []entity.EntityKind
	for kind, owner := range entityOwners {
		if owner != service {
			out = append(out, kind)
		}
	}
	slices.Sort(out)
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
