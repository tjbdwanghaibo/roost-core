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
// 删了、改成 nopersist——业务字段读不到它的值，这里把它报出来。值本身不丢：结构体的 UnknownStoredKeys 把它带在
// 内存里，整文档写回时原样写回，直到经 engine.CleanupStoredKeys 显式清理。
//
// 只告警，不拒绝加载（维护者 2026-10-19 决定）：同一进程里同一 DAO 的同一个键只打一次 WARN，不按实体刷屏；
// 每次出现都计入 dataengine.dao.unknown_stored_key.total（标签只有 dao）。定义里有、文档里没有的键是“加字段”的
// 正常情况（解码给零值），不报。顶层以 _ 开头的键是框架元数据（_id、_schema、_version、_last_tx、_deleted、
// _deleted_at 等），不报；生成器保证 DAO 字段的键不以 _ 开头。
//
// 嵌套按解码层级检查：结构体字段进入子文档，map 的值、切片的元素逐个检查，路径里用 * 代替真实的 map 键与下标，
// 保持日志与去重的低基数。自己实现 bson 解码（Unmarshaler / ValueUnmarshaler）的类型、time.Time、没有导出字段的
// 结构体、interface 值不深入。
type StoredKeySet struct {
	dao      string
	plan     *storedKeyPlan
	reported sync.Map // 路径 -> struct{}：已经告警过的未知键
}

// UnknownStoredKeys 是一份持久文档里 DAO 不认识的键与原样的值（DESIGN-TODO D13，维护者 2026-10-19 方案 A）。
// 生成的持久文档结构体以 bson:",inline" 字段携带它：解码时收下结构体之外的键，编码时原样写回。加载后 DAO
// 把它留在内存里（不暴露给业务字段），整文档写回、回滚快照、Remote 托管实体的 data 信封都带着它，所以字段改名
// 后没加 bson 名、删字段、改 nopersist 时，库里的旧值不会因为下一次整文档写入而消失，看到 WARN 之后还能迁移补救。
// 清理只经显式入口（engine.CleanupStoredKeys），框架从不自动删除。
type UnknownStoredKeys map[string]bson.RawValue

// Persisted 返回加载后要保留的部分：去掉以 _ 开头的顶层键（_version、_last_tx、_deleted 等框架元数据，由存储
// 层每次写入时重新给出），值复制一份，不引用解码缓冲区。没有剩余时返回 nil。
func (keys UnknownStoredKeys) Persisted() UnknownStoredKeys {
	var kept UnknownStoredKeys
	for key, value := range keys {
		if strings.HasPrefix(key, "_") {
			continue
		}
		if kept == nil {
			kept = make(UnknownStoredKeys, len(keys))
		}
		kept[key] = bson.RawValue{Type: value.Type, Value: append([]byte(nil), value.Value...)}
	}
	return kept
}

// Clone 复制键与值（嵌套类型的 wire 文档用它：嵌套子文档里没有框架元数据，以 _ 开头的键也照样保留）。
func (keys UnknownStoredKeys) Clone() UnknownStoredKeys {
	if len(keys) == 0 {
		return nil
	}
	out := make(UnknownStoredKeys, len(keys))
	for key, value := range keys {
		out[key] = bson.RawValue{Type: value.Type, Value: append([]byte(nil), value.Value...)}
	}
	return out
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
		attrs = append(attrs, "hint", "the field was renamed, removed or made nopersist: no field reads its stored value (it is kept and written back, not lost); "+
			"keep the old stored name with bson:\"...\" on the field, or migrate offline and clean the old key up (docs/framework/SCHEMA-MIGRATION.md). Logged once per key per process")
		slog.Warn("dataengine: stored document has a key the DAO does not declare", attrs...)
	})
}

// Declares 报告点分路径 path（与 WarnUnknown 报出的路径同形，* 表示 map 的值或切片的元素）是不是文档结构体
// 声明的键。清理入口用它拒绝删除仍在使用的键。
func (s *StoredKeySet) Declares(path string) bool {
	if s == nil || path == "" {
		return false
	}
	plan := s.plan
	segments := strings.Split(path, ".")
	for i := 0; i < len(segments); i++ {
		if plan == nil || plan.anyKey {
			return false
		}
		field, ok := plan.fields[segments[i]]
		if !ok {
			return false
		}
		if i == len(segments)-1 {
			return true
		}
		switch {
		case field.doc != nil:
			plan = field.doc
		case field.each != nil && segments[i+1] == "*":
			plan = field.each
			i++
		default:
			return false
		}
	}
	return false
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
	unknownStoredKeysType    = reflect.TypeFor[UnknownStoredKeys]()
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
		if skip || field.Type == unknownStoredKeysType {
			// UnknownStoredKeys 是收未知键的 inline map，它本身不声明任何键。
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
