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
	// Dir 是实体包目录（绝对路径）。
	Dir string
	// Position 是标记所在的 file:line，用于报错。
	Position string
}

// Scan 读 gameDir/entities/*/ 下全部实体标记，返回声明了 service= 的实体，按 Dir、Entity 排序。
// 这是生成工程放实体的唯一位置（roost add entity、id 检查与 remote 宿主检查读同一处）。
func Scan(gameDir string) ([]Owned, error) {
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
			service, declared := params["service"]
			if !declared {
				continue
			}
			service = strings.TrimSpace(service)
			where := fmt.Sprintf("%s:%d", position.Filename, position.Line)
			if !validServiceName(service) {
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
				Entity:   entityName,
				Kind:     kind,
				Service:  service,
				Remote:   strings.EqualFold(strings.TrimSpace(params["remote"]), "managed"),
				Dir:      filepath.Dir(path),
				Position: where,
			})
		}
	}
	return out, nil
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
