package dataengine

import (
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/x/bsonx/bsoncore"
)

// StoredKeySet 是一个生成 DAO 的持久文档结构体（<dao>PersistDoc，见 DESIGN-TODO D13）声明的全部键，包括嵌套类型
// 的 wire 文档里的键。生成的 RestorePersisted 在解码前用它检查库里的文档：文档里有、结构体里没有的键——字段改了名、
// 删了、改成 nopersist——业务字段读不到它的值，这里把它报出来。
//
// 只告警，不拒绝加载，也不读入（维护者 2026-10-19 决定：“旧字段就不 load 了，给一个业务层的约束或者通知就可以了”）：
// 解码器忽略结构体之外的键，DAO 内存里没有它们，下一次整文档写回就把它们从库里去掉（单字段补丁只 $set 自己的键，
// 不碰它们，但它们也不再被读取）。要保留数据，业务给改名的字段写原 bson 名，或在提升 schema 之前离线迁移
// （docs/framework/SCHEMA-MIGRATION.md §1）。同一进程里同一 DAO 的同一个键只打一次 WARN，不按实体刷屏；每次出现都
// 计入 dataengine.dao.unknown_stored_key.total（标签只有 dao）。定义里有、文档里没有的键是“加字段”的正常情况
// （解码给零值），不报。顶层以 _ 开头的键是框架元数据（_id、_schema、_version、_last_tx、_deleted、_deleted_at 等），
// 不报；生成器保证 DAO 字段的键不以 _ 开头。
//
// 嵌套按解码层级检查：结构体字段进入子文档，map 的值、切片的元素逐个检查，路径里用 * 代替真实的 map 键与下标，
// 保持日志与去重的低基数。自己实现 bson 解码（Unmarshaler / ValueUnmarshaler）的类型、time.Time、没有导出字段的
// 结构体、interface 值不深入。
type StoredKeySet struct {
	dao      string
	plan     *storedKeyPlan
	reported sync.Map // 路径 -> struct{}：已经告警过的未知键
}

type storedKeyPlan struct {
	fields map[string]storedKeyField
	// anyKey 是 inline map 字段：任何键都由它接收，不存在未知键。
	anyKey bool
}

// storedKeyField 说明一个已知键的值怎样继续检查：doc 非空时值本身是带这些键的子文档；each 非空时值是 map（子文档）
// 或数组，其中每个值 / 元素是带这些键的子文档。两者都空就不深入。
type storedKeyField struct {
	doc  *storedKeyPlan
	each *storedKeyPlan
}

// NewStoredKeySet 从文档结构体 T 的 bson 标签推出键集合（与 bson 解码器同一套规则：标签名，缺省为小写字段名，
// inline 展开，"-" 跳过）。T 一般是生成的持久文档结构体，所以键集合与写入、读取用的是同一份声明。
func NewStoredKeySet[T any](dao string) *StoredKeySet {
	builder := storedKeyPlanBuilder{seen: make(map[reflect.Type]*storedKeyPlan)}
	plan := builder.doc(reflect.TypeFor[T]())
	if plan == nil {
		plan = &storedKeyPlan{anyKey: true}
	}
	return &StoredKeySet{dao: dao, plan: plan}
}

// WarnUnknown 检查一份持久文档，对其中第一次见到的未知键打印 WARN。raw 不是合法 BSON 时什么也不做：随后的解码会
// 返回真正的错误。它不修改 raw，也不影响加载结果。
func (s *StoredKeySet) WarnUnknown(raw []byte) {
	if s == nil {
		return
	}
	walkStoredKeys(bsoncore.Document(raw), s.plan, true, nil, func(path string) {
		metrics.IncCounter("dataengine.dao.unknown_stored_key.total", metrics.Labels{"dao": s.dao}, 1)
		if _, seen := s.reported.LoadOrStore(path, struct{}{}); seen {
			return
		}
		attrs := []any{"dao", s.dao, "key", path}
		if id, ok := bson.Raw(raw).Lookup("_id").Int64OK(); ok {
			attrs = append(attrs, "first_id", id)
		}
		attrs = append(attrs, "hint", "the field was renamed, removed or made nopersist: this key is no longer read, and the next full write of the document deletes it; "+
			"to keep the data give the field its old stored name with bson:\"...\", or migrate offline first (docs/framework/SCHEMA-MIGRATION.md). Logged once per key per process")
		slog.Warn("dataengine: stored document has a key the DAO does not declare; it is no longer read", attrs...)
	})
}

// UnknownKeys 返回 raw 里的未知键路径（按出现顺序，可能重复），不告警、不计数；供测试与诊断工具使用。
func (s *StoredKeySet) UnknownKeys(raw []byte) []string {
	if s == nil {
		return nil
	}
	var paths []string
	walkStoredKeys(bsoncore.Document(raw), s.plan, true, nil, func(path string) { paths = append(paths, path) })
	return paths
}

// walkStoredKeys 逐个读取 doc 的元素（不解码值、不分配，除非要进入子文档或报告），未知键交给 report。
func walkStoredKeys(doc bsoncore.Document, plan *storedKeyPlan, top bool, path []string, report func(string)) {
	if plan == nil || plan.anyKey {
		return
	}
	_, rem, ok := bsoncore.ReadLength(doc)
	if !ok {
		return
	}
	for len(rem) > 1 {
		element, next, ok := bsoncore.ReadElement(rem)
		if !ok {
			return
		}
		rem = next
		key, err := element.KeyBytesErr()
		if err != nil {
			return
		}
		if top && len(key) > 0 && key[0] == '_' {
			continue
		}
		field, known := plan.fields[string(key)]
		if !known {
			report(joinStoredKeyPath(path, string(key)))
			continue
		}
		if field.doc == nil && field.each == nil {
			continue
		}
		value := element.Value()
		if value.Type != bsoncore.TypeEmbeddedDocument && value.Type != bsoncore.TypeArray {
			continue
		}
		child := append(path[:len(path):len(path)], string(key))
		if field.doc != nil {
			if value.Type == bsoncore.TypeEmbeddedDocument {
				walkStoredKeys(bsoncore.Document(value.Data), field.doc, false, child, report)
			}
			continue
		}
		walkEachStoredDocument(bsoncore.Document(value.Data), field.each, append(child, "*"), report)
	}
}

// walkEachStoredDocument 检查 map 的每个值或数组的每个元素（二者在 BSON 里都是文档）。
func walkEachStoredDocument(container bsoncore.Document, plan *storedKeyPlan, path []string, report func(string)) {
	_, rem, ok := bsoncore.ReadLength(container)
	if !ok {
		return
	}
	for len(rem) > 1 {
		element, next, ok := bsoncore.ReadElement(rem)
		if !ok {
			return
		}
		rem = next
		if value := element.Value(); value.Type == bsoncore.TypeEmbeddedDocument {
			walkStoredKeys(bsoncore.Document(value.Data), plan, false, path, report)
		}
	}
}

func joinStoredKeyPath(path []string, key string) string {
	if len(path) == 0 {
		return key
	}
	return strings.Join(path, ".") + "." + key
}

type storedKeyPlanBuilder struct {
	seen map[reflect.Type]*storedKeyPlan
}

var (
	bsonUnmarshalerType      = reflect.TypeFor[bson.Unmarshaler]()
	bsonValueUnmarshalerType = reflect.TypeFor[bson.ValueUnmarshaler]()
	timeType                 = reflect.TypeFor[time.Time]()
)

// doc 返回结构体 t 作为子文档时的键集合；不深入的类型返回 nil。递归类型在构建中途返回同一个（尚未填完的）计划。
func (b *storedKeyPlanBuilder) doc(t reflect.Type) *storedKeyPlan {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeType ||
		reflect.PointerTo(t).Implements(bsonUnmarshalerType) || reflect.PointerTo(t).Implements(bsonValueUnmarshalerType) {
		return nil
	}
	if plan, ok := b.seen[t]; ok {
		return plan
	}
	plan := &storedKeyPlan{fields: make(map[string]storedKeyField)}
	b.seen[t] = plan
	exported := false
	b.addFields(plan, t, &exported)
	if !exported {
		delete(b.seen, t)
		return nil
	}
	return plan
}

func (b *storedKeyPlanBuilder) addFields(plan *storedKeyPlan, t reflect.Type, exported *bool) {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		*exported = true
		name, inline, skip := bsonFieldTag(field)
		if skip {
			continue
		}
		if inline || (field.Anonymous && name == "") {
			inner := field.Type
			for inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			switch inner.Kind() {
			case reflect.Struct:
				b.addFields(plan, inner, exported)
			case reflect.Map:
				plan.anyKey = true
			}
			if inline {
				continue
			}
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		plan.fields[name] = b.field(field.Type)
	}
}

// field 说明类型 t 的值要不要深入检查。
func (b *storedKeyPlanBuilder) field(t reflect.Type) storedKeyField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return storedKeyField{doc: b.doc(t)}
	case reflect.Map, reflect.Slice, reflect.Array:
		if t.Kind() != reflect.Map && t.Elem().Kind() == reflect.Uint8 {
			return storedKeyField{} // []byte / [N]byte 是二进制值
		}
		return storedKeyField{each: b.doc(t.Elem())}
	default:
		return storedKeyField{}
	}
}

// bsonFieldTag 按 bson 解码器的规则读字段标签：bson:"name,opts"；没有 bson 键但整个标签不含冒号时整个标签就是它。
func bsonFieldTag(field reflect.StructField) (name string, inline, skip bool) {
	tag, ok := field.Tag.Lookup("bson")
	if !ok && !strings.Contains(string(field.Tag), ":") && len(field.Tag) > 0 {
		tag = string(field.Tag)
	}
	if tag == "-" {
		return "", false, true
	}
	name, options, _ := strings.Cut(tag, ",")
	for _, option := range strings.Split(options, ",") {
		if option == "inline" {
			inline = true
		}
	}
	return name, inline, false
}
