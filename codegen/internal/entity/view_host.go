package entity

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/genutil"
)

// 宿主方法（view 的 <X>Holder 里访问器以外的方法）由实体实现。其中“把实体 DAO 的某个字段交给组件”的一类
// 由生成器写进 <实体>_gen_wire.go（纯 DAO 交接），规则：
//
//  1. 方法无参、只有一个返回值，返回值类型等于实体某个热 DAO（不是 cold）上某个字段的 getter 返回类型：
//     结构体字段 X T / X *T 的 getter 返回 *db.T，基础类型字段 X int32 的 getter 返回 int32。
//     切片、map、别的包里的类型不参与。
//  2. 在类型相同的字段里先按名字找：方法名等于字段名（AttributeBase ← AttributeBase）；返回结构体指针时
//     再试“组件名 + 字段名”等于方法名（组件 Attribute 的 AttributeGear ← Gear）；正好一个就用它。
//  3. 名字都对不上时，返回类型是 DAO 包里的结构体指针，就按类型找唯一的字段（BagState ← Bag，类型 *db.BagState）。
//     基础类型（int32、string……）只认方法名与字段名完全相同：同类型的基础字段往往不止一个、含义各不相同
//     （AttributeLevel 是档案等级，不是 DAO 里随便哪个 Level），去前缀或按类型猜都会把无关字段接上。
//  4. 有歧义或找不到就不生成，写进编译期断言上方的“需要手写”清单；实体包里已经手写了同名方法时也不生成。
//
// 生成的形式：func (e *Player) BagState() *db.BagState { return e.dao.GetBag() }。

// HostMethod 是生成器写出的一个纯 DAO 交接宿主方法。
type HostMethod struct {
	Name, Holder string
	ResultType   string // 用接线文件的导入别名写出的返回类型
	DaoField     string // 实体结构体里 DAO 字段名（e.dao）
	DaoGetter    string // DAO 上的 getter（GetBag）
	Source       string // 注释用："PlayerDao.Bag"
}

// ManualHost 是要手写的一个宿主方法。
type ManualHost struct {
	Name, Holder, Signature string
	Missing                 bool   // 实体包里还没有这个方法
	Note                    string // 没能生成的原因（有歧义时写清候选）
}

// daoFieldInfo 是实体一个热 DAO 上可以交接的字段。
type daoFieldInfo struct {
	EntityField, DaoType, Name string
	Key                        string // 返回类型的规范形式（与 viewTypeKey 一致）
	Render                     string // 用接线文件的导入别名写出的返回类型
	Struct                     bool
}

// resolveHostMethods 按 embedded 的 Holder 解出宿主方法，填 ent.HostMethods / ent.ManualHosts。
func resolveHostMethods(ent *EntityDef, target viewTarget, embedded []string) error {
	fields, err := entityDaoFields(*ent, target)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, holderName := range embedded {
		holder := target.holders[holderName]
		for _, host := range holder.Hosts {
			if seen[host.Name] {
				continue
			}
			seen[host.Name] = true
			manual := ManualHost{Name: host.Name, Holder: holder.Name, Signature: host.Signature, Missing: !hasMethod(*ent, host.Name)}
			if hasMethod(*ent, host.Name) {
				ent.ManualHosts = append(ent.ManualHosts, manual)
				continue
			}
			field, note := matchDaoHandoff(host, holder.Component, fields)
			if field == nil {
				manual.Note = note
				ent.ManualHosts = append(ent.ManualHosts, manual)
				continue
			}
			ent.HostMethods = append(ent.HostMethods, HostMethod{
				Name: host.Name, Holder: holder.Name, ResultType: field.Render,
				DaoField: field.EntityField, DaoGetter: "Get" + field.Name, Source: field.DaoType + "." + field.Name,
			})
		}
	}
	return nil
}

// matchDaoHandoff 按文件头的规则 2、3 找字段；找不到或有歧义时返回 nil 和原因。
func matchDaoHandoff(host viewHostMethod, component string, fields []daoFieldInfo) (*daoFieldInfo, string) {
	if host.Result == "" {
		return nil, ""
	}
	var sameType []daoFieldInfo
	for _, field := range fields {
		if field.Key == host.Result {
			sameType = append(sameType, field)
		}
	}
	byNames := []func(daoFieldInfo) bool{func(field daoFieldInfo) bool { return field.Name == host.Name }}
	if host.Struct {
		byNames = append(byNames, func(field daoFieldInfo) bool { return component+field.Name == host.Name })
	}
	for _, byName := range byNames {
		var hits []daoFieldInfo
		for _, field := range sameType {
			if byName(field) {
				hits = append(hits, field)
			}
		}
		if len(hits) == 1 {
			return &hits[0], ""
		}
		if len(hits) > 1 {
			return nil, "DAO 里有多个同名字段：" + daoFieldNames(hits)
		}
	}
	switch {
	case len(sameType) == 0:
		return nil, ""
	case !host.Struct:
		return nil, "DAO 里有这个类型的字段但名字不同，基础类型只认完全同名"
	case len(sameType) == 1:
		return &sameType[0], ""
	default:
		return nil, "DAO 里有多个这个类型的字段（" + daoFieldNames(sameType) + "），名字都对不上"
	}
}

func daoFieldNames(fields []daoFieldInfo) string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.DaoType+"."+field.Name)
	}
	return strings.Join(names, "、")
}

// entityDaoFields 读出实体各热 DAO 在 <DAO 包>/def 里的定义字段。DAO 不在本工程里、定义找不到时跳过（不做交接）。
func entityDaoFields(ent EntityDef, target viewTarget) ([]daoFieldInfo, error) {
	var out []daoFieldInfo
	for _, dao := range ent.Daos {
		alias := qualifier(dao.TypeName)
		if dao.Cold || alias == "" {
			continue
		}
		importPath := ""
		for _, imp := range ent.Imports {
			if imp.Alias == alias {
				importPath = imp.Path
			}
		}
		rel, ok := strings.CutPrefix(importPath, target.Module+"/")
		if !ok {
			continue
		}
		daoType := lastTypeSegment(dao.TypeName)
		defs, err := parseDaoDefinition(filepath.Join(target.Root, filepath.FromSlash(rel), "def"), daoType)
		if err != nil {
			return nil, err
		}
		for _, def := range defs {
			field := daoFieldInfo{EntityField: dao.FieldName, DaoType: daoType, Name: def.name}
			switch {
			case def.basic != "":
				field.Key, field.Render = def.basic, def.basic
			case def.structName != "":
				field.Key, field.Render, field.Struct = "*"+importPath+"."+def.structName, "*"+alias+"."+def.structName, true
			default:
				continue
			}
			out = append(out, field)
		}
	}
	return out, nil
}

type daoDefField struct {
	name, basic, structName string
}

// parseDaoDefinition 在 defDir 里找结构体 daoType，返回它的字段：basic 是预声明基础类型名，structName 是同一
// 定义包里声明的结构体（getter 返回它的指针）；别的形式两者都为空。目录不存在时返回空。
func parseDaoDefinition(defDir, daoType string) ([]daoDefField, error) {
	paths, err := filepath.Glob(filepath.Join(defDir, "*.go"))
	if err != nil {
		return nil, err
	}
	structs := map[string]*ast.StructType{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
			}
		}
	}
	dao := structs[daoType]
	if dao == nil {
		return nil, nil
	}
	var out []daoDefField
	for _, field := range dao.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		if field.Tag != nil {
			if tag, err := strconv.Unquote(field.Tag.Value); err == nil && extractTagValue("`"+tag+"`", "dao") == "-" {
				continue
			}
		}
		def := daoDefField{}
		expr := field.Type
		if star, ok := expr.(*ast.StarExpr); ok {
			expr = star.X
			if ident, ok := expr.(*ast.Ident); !ok || structs[ident.Name] == nil {
				continue
			}
		}
		if ident, ok := expr.(*ast.Ident); ok {
			switch {
			case structs[ident.Name] != nil:
				def.structName = ident.Name
			case isPredeclared(ident.Name) && ident.Name != "error" && ident.Name != "any":
				def.basic = ident.Name
			}
		}
		for _, name := range field.Names {
			def.name = name.Name
			out = append(out, def)
		}
	}
	return out, nil
}

// holderEntitiesFile 是 game/view 里每个 <X>Holder 的窄接口 I<X>Entity（生成）。
const holderEntitiesFile = "holder_entities_gen.go"

// generateHolderEntities 写出（或在没有 Holder 时删掉）game/view/holder_entities_gen.go。
// I<X>Entity 与 view 里手写的类型重名时报错：实体总接口是 <E>Entity，组件窄接口加 I 前缀正是为了避开它（room 组件与 Room 实体）。
func generateHolderEntities(target viewTarget, force bool) (string, bool, error) {
	path := filepath.Join(target.Dir, holderEntitiesFile)
	if len(target.holders) == 0 {
		removed, err := retireEntityFile(path)
		return path, removed, err
	}
	names := make([]string, 0, len(target.holders))
	for name := range target.holders {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString(entityGeneratedHeader + "\n")
	b.WriteString("package view\n\nimport \"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n")
	for _, name := range names {
		holder := target.holders[name]
		iface := "I" + holder.Component + "Entity"
		if file, clash := target.declared[iface]; clash {
			return "", false, fmt.Errorf("%s: game/view/%s already declares %s, the generated narrow interface of %s; rename the hand-written type", target.Dir, file, iface, name)
		}
		fmt.Fprintf(&b, "\n// %s 是只用 %s 组件的 Nest handler 的参数类型：挂了这个组件的实体都满足它。只在实体被 Nest 锁住期间使用。\n", iface, holder.Component)
		fmt.Fprintf(&b, "type %s interface {\n\tentity.IThreadSafeEntity\n\t%s\n}\n", iface, name)
	}
	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return "", false, fmt.Errorf("format %s: %w", holderEntitiesFile, err)
	}
	if err := os.MkdirAll(target.Dir, 0o755); err != nil {
		return "", false, err
	}
	changed, err := genutil.WriteIfChanged(path, formatted, force)
	return path, changed, err
}
