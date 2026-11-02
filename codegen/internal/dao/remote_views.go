package dao

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const remoteViewsFileName = "gen_remote_views.go"

var remoteViewName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

func parseRemoteViewTag(tag *ast.BasicLit) ([]string, error) {
	if tag == nil {
		return nil, nil
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return nil, err
	}
	value, ok := reflect.StructTag(raw).Lookup("remoteview")
	if !ok {
		return nil, nil
	}
	seen := map[string]bool{}
	var views []string
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if !remoteViewName.MatchString(name) {
			return nil, fmt.Errorf("invalid remoteview name %q; use lowercase snake_case names", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate remoteview name %q", name)
		}
		seen[name] = true
		views = append(views, name)
	}
	return views, nil
}

type remoteViewField struct {
	field            FieldDef
	typ, copy, shape string
}
type remoteViewDef struct {
	name, scope, schemaIdentity string
	fields                      []remoteViewField
	masks                       []string
}
type remoteViewGenerator struct {
	defs     *Definitions
	nested   map[string]string // 类型的完整形状，同时避免重复生成。
	visiting map[string]bool
	names    map[string]bool
	code     bytes.Buffer
}

// buildRemoteViews 只生成顶层字段白名单，不根据客户端 Sync 标记推断权限。
// 嵌套字段整体投影其中的持久字段；生成普通值和深拷贝，不携带 DirtyHook 或权威 DAO 指针。
func buildRemoteViews(defs *Definitions, pkg string) ([]byte, error) {
	g := remoteViewGenerator{defs: defs, nested: map[string]string{}, visiting: map[string]bool{}, names: map[string]bool{}}
	for _, d := range defs.Daos {
		g.names[d.Name] = true
	}
	for _, n := range defs.Nested {
		g.names[n.Name] = true
		for _, f := range n.Fields {
			if len(f.RemoteViews) > 0 {
				return nil, fmt.Errorf("nested %s.%s: remoteview belongs on a top-level DAO field", n.Name, f.Name)
			}
		}
	}
	for _, d := range defs.RedisDaos {
		for _, f := range d.Fields {
			if len(f.RemoteViews) > 0 {
				return nil, fmt.Errorf("redis DAO %s.%s: remoteview requires a persistent Mongo DAO", d.Name, f.Name)
			}
		}
	}
	count := 0
	for _, d := range defs.Daos {
		groups := map[string][]FieldDef{}
		for _, f := range d.Fields {
			if len(f.RemoteViews) > 0 && (d.NoCollection || !f.Tag.Persist || f.StoreSkip) {
				return nil, fmt.Errorf("DAO %s.%s: remoteview requires a persisted field", d.Name, f.Name)
			}
			for _, name := range f.RemoteViews {
				groups[name] = append(groups[name], f)
			}
		}
		names := make([]string, 0, len(groups))
		for n := range groups {
			names = append(names, n)
		}
		sort.Strings(names)
		var views []remoteViewDef
		for _, name := range names {
			v := remoteViewDef{name: strings.TrimSuffix(strings.TrimSuffix(d.Name, "Dao"), "DAO") + viewTitle(name) + "Snapshot", scope: "remoteview:" + d.Coll + ":" + name}
			if err := g.reserve(v.name); err != nil {
				return nil, err
			}
			var shape strings.Builder
			for _, f := range groups[name] {
				vf, err := g.field(f, "d", d.Name)
				if err != nil {
					return nil, fmt.Errorf("DAO %s.%s: %w", d.Name, f.Name, err)
				}
				v.fields = append(v.fields, vf)
				v.masks = append(v.masks, fieldMaskName(d.Name, f.Name))
				fmt.Fprintf(&shape, "%s:%s:%s;", f.Name, f.Key, vf.shape)
			}
			hash := sha256.Sum256([]byte(shape.String()))
			v.schemaIdentity = fmt.Sprintf("%s:bson:%x", v.scope, hash)
			views = append(views, v)
			g.emitView(d, v, name)
			count++
		}
		if len(views) > 0 {
			g.emitDAO(d, views)
		}
	}
	if count == 0 {
		return nil, nil
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "%s\npackage %s\nimport (\n\"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n\"go.mongodb.org/mongo-driver/v2/bson\"\n)\n", generatedHeader, pkg)
	out.Write(g.code.Bytes())
	out.WriteString(remoteViewCopyHelpers)
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format remote views: %w", err)
	}
	return formatted, nil
}

func viewTitle(name string) string {
	var out strings.Builder
	for _, part := range strings.Split(name, "_") {
		out.WriteString(strings.ToUpper(part[:1]))
		out.WriteString(part[1:])
	}
	return out.String()
}
func (g *remoteViewGenerator) reserve(name string) error {
	if g.names[name] {
		return fmt.Errorf("remoteview generated type %s conflicts with another declaration", name)
	}
	g.names[name] = true
	return nil
}
func (g *remoteViewGenerator) field(f FieldDef, receiver, daoName string) (remoteViewField, error) {
	if f.Name == "RemoteScope" || f.Name == "RemoteSchema" {
		return remoteViewField{}, fmt.Errorf("field %s conflicts with a generated view method", f.Name)
	}
	expr, err := parser.ParseExpr(f.TypeStr)
	if err != nil {
		return remoteViewField{}, err
	}
	source := receiver + "." + safeFieldVarName(f.Name)
	if f.Kind == KindMap {
		if daoName != "" {
			source = receiver + "." + mapHelperName(daoName, f.Name) + "()"
		} else {
			source += "RawMap()"
		}
	}
	typ, copy, shape, err := g.shape(expr, source)
	return remoteViewField{field: f, typ: typ, copy: copy, shape: shape}, err
}

// shape 返回投影类型、复制表达式和递归 schema 描述。未识别的命名/外部类型不猜测其底层形状。
func (g *remoteViewGenerator) shape(expr ast.Expr, source string) (typ, copy, shape string, err error) {
	switch t := expr.(type) {
	case *ast.Ident:
		if isBasicType(t.Name) {
			return t.Name, source, t.Name, nil
		}
		shape, err = g.ensureNested(t.Name)
		if err != nil {
			return "", "", "", err
		}
		return t.Name + "RemoteSnapshot", "remoteViewCopy" + t.Name + "(" + source + ")", shape, nil
	case *ast.StarExpr:
		child, ok := t.X.(*ast.Ident)
		if !ok || !isNestedType(g.defs, child.Name) {
			return "", "", "", fmt.Errorf("remoteview supports pointers only to declared nested types")
		}
		shape, err = g.ensureNested(child.Name)
		if err != nil {
			return "", "", "", err
		}
		return "*" + child.Name + "RemoteSnapshot", "remoteViewCopy" + child.Name + "Ptr(" + source + ")", "*" + shape, nil
	case *ast.ArrayType:
		if t.Len != nil {
			return "", "", "", fmt.Errorf("remoteview fixed arrays are unsupported")
		}
		ct, cc, cs, e := g.shape(t.Elt, "value")
		if e != nil {
			return "", "", "", e
		}
		return "[]" + ct, fmt.Sprintf("remoteViewCopySlice(%s, func(value %s) %s { return %s })", source, exprString(t.Elt), ct, cc), "[]" + cs, nil
	case *ast.MapType:
		key, ok := t.Key.(*ast.Ident)
		if !ok || !isBasicType(key.Name) {
			return "", "", "", fmt.Errorf("remoteview map key must be a basic type")
		}
		// BSON 的 map 键必须能无损表示成文档字符串；不接受浮点、布尔键。
		if key.Name != "string" && !strings.Contains("|int|int8|int16|int32|int64|uint|uint8|uint16|uint32|uint64|byte|rune|", "|"+key.Name+"|") {
			return "", "", "", fmt.Errorf("unsupported remoteview map key %s", key.Name)
		}
		ct, cc, cs, e := g.shape(t.Value, "value")
		if e != nil {
			return "", "", "", e
		}
		return "map[" + key.Name + "]" + ct, fmt.Sprintf("remoteViewCopyMap(%s, func(value %s) %s { return %s })", source, exprString(t.Value), ct, cc), "map[" + key.Name + "]" + cs, nil
	default:
		return "", "", "", fmt.Errorf("unsupported remoteview type %s", exprString(expr))
	}
}
func (g *remoteViewGenerator) ensureNested(name string) (string, error) {
	if shape, ok := g.nested[name]; ok {
		return shape, nil
	}
	if g.visiting[name] {
		return "", fmt.Errorf("remoteview recursive nested type %s is unsupported", name)
	}
	var nested *NestedDef
	for i := range g.defs.Nested {
		if g.defs.Nested[i].Name == name {
			nested = &g.defs.Nested[i]
			break
		}
	}
	if nested == nil {
		return "", fmt.Errorf("unsupported remoteview type %s; use basic fields or a declared nested struct", name)
	}
	if err := g.reserve(name + "RemoteSnapshot"); err != nil {
		return "", err
	}
	g.visiting[name] = true
	var fields []remoteViewField
	var shape strings.Builder
	fmt.Fprintf(&shape, "%s{", name)
	for _, f := range nested.Fields {
		if !f.Tag.Persist {
			continue
		}
		vf, err := g.field(f, "s", "")
		if err != nil {
			return "", err
		}
		fields = append(fields, vf)
		fmt.Fprintf(&shape, "%s:%s:%s;", f.Name, f.Key, vf.shape)
	}
	shape.WriteString("}")
	g.emitStruct(name+"RemoteSnapshot", fields)
	fmt.Fprintf(&g.code, "func remoteViewCopy%s(s %s) %sRemoteSnapshot { return %sRemoteSnapshot{\n", name, name, name, name)
	for _, f := range fields {
		fmt.Fprintf(&g.code, "%s: %s,\n", f.field.Name, f.copy)
	}
	g.code.WriteString("} }\n")
	fmt.Fprintf(&g.code, "func remoteViewCopy%sPtr(s *%s) *%sRemoteSnapshot { if s==nil {return nil}; v:=remoteViewCopy%s(*s);return &v }\n", name, name, name, name)
	delete(g.visiting, name)
	g.nested[name] = shape.String()
	return shape.String(), nil
}
func (g *remoteViewGenerator) emitStruct(name string, fields []remoteViewField) {
	fmt.Fprintf(&g.code, "// %s 是独立只读快照；修改它不会写回 DAO，不可作为写权限凭据。\ntype %s struct {\n", name, name)
	for _, f := range fields {
		fmt.Fprintf(&g.code, "%s %s `bson:%q`\n", f.field.Name, f.typ, f.field.Key)
	}
	g.code.WriteString("}\n")
}
func (g *remoteViewGenerator) emitView(d DaoDef, v remoteViewDef, view string) {
	g.emitStruct(v.name, v.fields)
	fmt.Fprintf(&g.code, "func (%s) RemoteScope() uint64 { return uint64(entity.RemoteSnapshotScope(%q)) }\n", v.name, v.scope)
	fmt.Fprintf(&g.code, "func (%s) RemoteSchema(kind entity.EntityKind) uint32 { return entity.RemoteSnapshotSchema(kind,entity.RemoteSnapshotScope(%q)) }\n", v.name, v.schemaIdentity)
	fmt.Fprintf(&g.code, "// %sSnapshot 必须在拥有者的 Entity 锁内调用；容器和嵌套值均复制。\nfunc (d *%s) %sSnapshot() %s { return %s{\n", viewTitle(view), d.Name, viewTitle(view), v.name, v.name)
	for _, f := range v.fields {
		fmt.Fprintf(&g.code, "%s: %s,\n", f.field.Name, f.copy)
	}
	g.code.WriteString("} }\n")
	fmt.Fprintf(&g.code, "func Decode%s(data []byte) (%s,error) { var v %s; err:=bson.Unmarshal(data,&v);return v,err }\n", v.name, v.name, v.name)
}
func (g *remoteViewGenerator) emitDAO(d DaoDef, views []remoteViewDef) {
	fmt.Fprintf(&g.code, "var _ entity.RemoteDAOViewProvider = (*%s)(nil)\n", d.Name)
	fmt.Fprintf(&g.code, "func (*%s) RemoteViewDefinitions(kind entity.EntityKind) []entity.RemoteDAOViewDefinition { return []entity.RemoteDAOViewDefinition{\n", d.Name)
	for _, v := range views {
		fmt.Fprintf(&g.code, "{Scope:uint32((%s{}).RemoteScope()), Schema:(%s{}).RemoteSchema(kind), Decode:func(data []byte)(any,error){return Decode%s(data)}},\n", v.name, v.name, v.name)
	}
	g.code.WriteString("} }\n")
	fmt.Fprintf(&g.code, "// BuildRemoteViews 只看本次事务 mask；投影命中后发送完整视图，避免缺失字段和容器增量歧义。\nfunc (d *%s) BuildRemoteViews(id int64,kind entity.EntityKind,mask uint64)([]entity.RemoteSnapshotRecord,error) {\nvar records []entity.RemoteSnapshotRecord\n", d.Name)
	for _, v := range views {
		viewName := strings.TrimPrefix(v.scope, "remoteview:"+d.Coll+":")
		fmt.Fprintf(&g.code, "if mask & (%s) != 0 {\ndata,err:=bson.Marshal(d.%sSnapshot());if err!=nil {return nil,err}\nrecords=append(records,entity.RemoteSnapshotRecord{Key:entity.RemoteSnapshotKey{EntityID:id,Kind:kind,Scope:uint32((%s{}).RemoteScope())},Schema:(%s{}).RemoteSchema(kind),Codec:1,Full:true,Data:data,Checksum:entity.RemoteSnapshotChecksum(data)})\n}\n", strings.Join(v.masks, " | "), viewTitle(viewName), v.name, v.name)
	}
	g.code.WriteString("return records,nil\n}\n")
}

const remoteViewCopyHelpers = `
func remoteViewCopySlice[A,B any](src []A, copyValue func(A) B) []B {
 if src==nil {return nil}; dst:=make([]B,len(src));for i,v:=range src {dst[i]=copyValue(v)};return dst
}
func remoteViewCopyMap[K comparable,A,B any](src map[K]A, copyValue func(A) B) map[K]B {
 if src==nil {return nil};dst:=make(map[K]B,len(src));for k,v:=range src {dst[k]=copyValue(v)};return dst
}
`
