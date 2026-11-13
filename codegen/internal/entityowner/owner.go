// Package entityowner 读实体的归属服务声明（//roost:entity ... service=<name>）。
//
// 归属只在实体标记上声明一次（维护者 2026-11-04 决定：不在 roost.yaml 再写一份）。声明了 service= 的 kind
// 只由那个服务写：生成器只在它的 Nest 引擎上注册这个 kind 的 handler，运行期别的服务的 Remote Manager 把它列为
// 外来 kind（ForeignKinds），拒绝认领与写许可。没有声明的 kind 保持原行为：任何带 Nest 的服务都注册、都能写。
//
// nest 生成器（按归属拆 handler 注册）与 roost 命令（服务名校验、bootstrap、add lifecycle、doctor）共用这里的
// 一次扫描，规则只有一份。
package entityowner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/marker"
)

// Owned 是一个声明了归属服务的实体。
type Owned struct {
	// Entity 是实体结构体名，例如 "Guild"。
	Entity string
	// Kind 是实体包里的 EntityKind 常量名（标记的 entityKind=，缺省 EntityKind<Entity>）。
	Kind string
	// Service 是 service= 的值。
	Service string
	// Remote 表示 remote=managed。
	Remote bool
	// Preload 表示 preload=startup。Remote 托管 kind（必须同时声明 service=）：归属服务起服时加载 owner 是本 sid 的
	// 全部实体并登记路由（联盟服务方案 §2.7“起服预加载”）。本地 kind（维护者 2026-11-13）：加载本服库（dbscope=sid）
	// 里这个 kind 的全部未删除实体，由 wiring/dataengine.LocalPreloadMod 执行（见 LocalPreloads）。
	Preload bool
	// Order 是本地预加载的批次（preload_order=<N>，缺省 0，越小越先）；只对本地 preload=startup 的实体有意义。
	Order int
	// NoPersist 表示 noPersist=true。
	NoPersist bool
	// Dir 是实体包目录（绝对路径）。
	Dir string
	// Position 是标记所在的 file:line，用于报错。
	Position string
}

// PreloadStartup 是 preload= 唯一的取值：起服预加载。
const PreloadStartup = "startup"

// MaxPreloadOrder 是 preload_order= 的上限（取值 0～MaxPreloadOrder）。
const MaxPreloadOrder = 1000

// Scan 读 gameDir/entities/*/ 下全部实体标记，返回声明了 service= 的实体，按 Dir、Entity 排序。
// 这是生成工程放实体的唯一位置（roost add entity、id 检查与 remote 宿主检查读同一处）。
func Scan(gameDir string) ([]Owned, error) {
	all, err := scanAll(gameDir)
	if err != nil {
		return nil, err
	}
	var out []Owned
	for _, item := range all {
		if item.Service != "" {
			out = append(out, item)
		}
	}
	return out, nil
}

// LocalPreloads 返回声明了 preload=startup 的本地（非 remote=managed）实体，按 Dir、Entity 排序；Service 是它的
// service=，没写时为 ""（由哪个服务预加载在工程层按 roost.yaml 决定，见 roost 的 localPreloadServices）。
func LocalPreloads(gameDir string) ([]Owned, error) {
	all, err := scanAll(gameDir)
	if err != nil {
		return nil, err
	}
	var out []Owned
	for _, item := range all {
		if item.Preload && !item.Remote {
			out = append(out, item)
		}
	}
	return out, nil
}

// scanAll 返回声明了 service= 或 preload= 的实体。
func scanAll(gameDir string) ([]Owned, error) {
	paths, err := filepath.Glob(filepath.Join(gameDir, "entities", "*", "*.go"))
	if err != nil {
		return nil, err
	}
	var out []Owned
	for _, path := range paths {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") || strings.HasSuffix(name, "_gen_wire.go") {
			continue
		}
		found, err := scanFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return out[i].Entity < out[j].Entity
	})
	return out, nil
}

func scanFile(path string) ([]Owned, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	structs := map[int]string{}
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
			if _, isStruct := typeSpec.Type.(*ast.StructType); isStruct {
				structs[fset.Position(typeSpec.Pos()).Line] = typeSpec.Name.Name
			}
		}
	}
	var out []Owned
	for _, group := range file.Comments {
		for _, comment := range group.List {
			body, ok := marker.Cut(comment.Text, "entity")
			if !ok || (body != "" && body[0] != ' ' && body[0] != '\t') {
				continue
			}
			position := fset.Position(comment.Pos())
			params, err := marker.Entity.Parse(body)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: //roost:entity %w", position.Filename, position.Line, err)
			}
			where := fmt.Sprintf("%s:%d", position.Filename, position.Line)
			service, declared := params["service"]
			remote := strings.EqualFold(strings.TrimSpace(params["remote"]), "managed")
			// preload= 是实体类型的显式策略（维护者 2026-11-04：“没有规则就不加载，需要 entity 显示加规则”）。只有一个取值；
			// 预加载的范围是“所有权记录的 owner 是本 sid 的实体”，所以只对归属某个服务的 Remote 托管 kind 有意义。
			preload, hasPreload := params["preload"]
			noPersist := strings.EqualFold(strings.TrimSpace(params["noPersist"]), "true")
			order, err := preloadOrder(where, params, hasPreload, remote)
			if err != nil {
				return nil, err
			}
			if hasPreload {
				lifetime := strings.ToLower(strings.TrimSpace(params["lifetime"]))
				switch {
				case strings.TrimSpace(preload) != PreloadStartup:
					return nil, fmt.Errorf("%s: //roost:entity preload=%q is not a preload policy; the only one is preload=%s (omit it to load on demand)", where, preload, PreloadStartup)
				case remote && !declared:
					return nil, fmt.Errorf("%s: //roost:entity preload=%s on a remote=managed entity needs service=<name>: the owning service preloads "+
						"the entities whose durable Remote owner is its sid", where, PreloadStartup)
				case !remote && (noPersist || lifetime == "ephemeral" || lifetime == "runtime_rebuild" || lifetime == "runtime-rebuild" || lifetime == "rebuild"):
					return nil, fmt.Errorf("%s: //roost:entity preload=%s on an entity that is not stored (noPersist=true or an ephemeral / runtime_rebuild lifetime): "+
						"a local preload loads this server's stored documents, and this entity has none", where, PreloadStartup)
				}
			}
			if !declared && !hasPreload {
				continue
			}
			service = strings.TrimSpace(service)
			if declared && !validServiceName(service) {
				return nil, fmt.Errorf("%s: //roost:entity service=%q is not a service name (lower snake case, as in roost.yaml services)", where, service)
			}
			// 与 entity 生成器同一条附着规则：结构体在标记下面两行以内。
			entityName := structs[position.Line+1]
			if entityName == "" {
				entityName = structs[position.Line+2]
			}
			if entityName == "" {
				return nil, fmt.Errorf("%s: //roost:entity marker is not followed by a struct declaration", where)
			}
			kind := strings.TrimSpace(params["entityKind"])
			if kind == "" {
				kind = "EntityKind" + entityName
			}
			out = append(out, Owned{
				Entity:    entityName,
				Kind:      kind,
				Service:   service,
				Remote:    remote,
				Preload:   hasPreload,
				Order:     order,
				NoPersist: noPersist,
				Dir:       filepath.Dir(path),
				Position:  where,
			})
		}
	}
	return out, nil
}

// preloadOrder 读 preload_order=：只能和本地实体上的 preload=startup 一起写，取值 0～MaxPreloadOrder 的整数。
func preloadOrder(where string, params map[string]string, hasPreload, remote bool) (int, error) {
	raw, has := params["preload_order"]
	if !has {
		return 0, nil
	}
	switch {
	case !hasPreload:
		return 0, fmt.Errorf("%s: //roost:entity preload_order= needs preload=%s: it orders the startup preload, and this entity is loaded on demand", where, PreloadStartup)
	case remote:
		return 0, fmt.Errorf("%s: //roost:entity preload_order= applies to local entities; a remote=managed kind is preloaded by its owning service "+
			"from the ownership records in one pass, with no order", where)
	}
	order, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || order < 0 || order > MaxPreloadOrder {
		return 0, fmt.Errorf("%s: //roost:entity preload_order=%q is not an integer from 0 to %d (smaller loads first; kinds with the same order load together)", where, raw, MaxPreloadOrder)
	}
	return order, nil
}

// validServiceName 与 roost.yaml 的服务名同一形状：小写字母开头，小写字母、数字、下划线。
func validServiceName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// DirOwners 把实体包目录映射到归属服务。同一个包里的实体声明了不同服务时这个包的类型归属不明确，
// 返回错误：handler 的目标类型按包解析归属，包内不能有两种答案。
func DirOwners(owned []Owned) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range owned {
		if previous, ok := out[item.Dir]; ok && previous != item.Service {
			return nil, fmt.Errorf("%s: entity %s declares service=%s, but another entity in the same package declares service=%s; "+
				"handler targets are resolved by package, so put entities owned by different services in different packages",
				item.Position, item.Entity, item.Service, previous)
		}
		out[item.Dir] = item.Service
	}
	return out, nil
}

// Services 返回至少拥有一个 kind 的服务，排序去重。
func Services(owned []Owned) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range owned {
		if !seen[item.Service] {
			seen[item.Service] = true
			out = append(out, item.Service)
		}
	}
	sort.Strings(out)
	return out
}

// ServiceOf 返回名为 entity（结构体名，大小写不敏感，或 snake 形式的包名）的实体的归属服务，未声明时为 ""。
func ServiceOf(owned []Owned, entity string) string {
	for _, item := range owned {
		if strings.EqualFold(item.Entity, entity) || strings.EqualFold(filepath.Base(item.Dir), entity) ||
			strings.EqualFold(strings.ReplaceAll(filepath.Base(item.Dir), "_", ""), strings.ReplaceAll(entity, "_", "")) {
			return item.Service
		}
	}
	return ""
}
