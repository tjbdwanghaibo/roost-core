// Package configschema 是服务配置的声明（维护者决定 A4 ①，docs/feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md）。
//
// 每个 Mod 把自己读的配置键写成一个带 tag 的结构体：字段类型就是键的类型，tag 写键名、缺省值、范围 / 枚举、
// 是否必填、说明。同一份声明有三个用途：
//
//   - 运行时读取：Decode 按声明一次读完，没写的键取缺省值，写了的检查类型、范围、枚举、必填，全部错误一次报出；
//   - App 启动前的检查：把本服务全部 Mod 的声明合并，在任何 Mod Init 之前检查（Schema.Check）；
//   - 生成器与 doctor：生成器按声明写配置段（StarterYAML），doctor 按声明检查工程里的配置文件。
//
// 这个包只依赖标准库：生成器（codegen 层）不导入它生成的运行时，只能导入这样的叶子包
// （与 configdata/rules 同理，根包 TestSharedConfigRulesStayALeaf 守住）。
package configschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind 是一个键的值类型。
type Kind string

const (
	KindBool     Kind = "bool"
	KindInt      Kind = "int"
	KindFloat    Kind = "float"
	KindDuration Kind = "duration"
	KindString   Kind = "string"
	KindStrings  Kind = "strings"
	// KindMap 是 map 字段本身（例如 saga.steps）：只要求写了就是映射，下面的键按 `*` 通配声明。
	KindMap Kind = "map"
	// KindSection 是带说明的嵌套段，只用于生成配置时在段名上方写注释，不参与检查。
	KindSection Kind = "section"
)

// Key 是一个配置键的声明。字段都是导出的普通值，生成器可以把它写成 Go 字面量（kitconfig_gen.go）。
type Key struct {
	// Name 是完整的点分键名（小写）；map 字段下的键在对应段写 `*`，例如 saga.steps.*.*.timeout。
	Name string
	Kind Kind
	// Default 是没写这个键时的值（YAML 写法）；空串表示类型零值。
	Default string
	// Starter 为 true 时生成器把这个键写进配置文件，值是 Example（tag `example` 存在，可以是空串）。
	Starter bool
	Example string
	// Min / Max 是闭区间界（按 Kind 解析）；空串表示不限。
	Min, Max string
	// Enum 是字符串键的可选值（小写）。
	Enum     []string
	Required bool
	// Secret 标记敏感字段；生产环境必须是非空、非 dev- 的值。
	Secret bool
	// SecretOptional（secret:"optional"）允许未启用功能时留空；非空值仍执行 secret 校验。
	// 功能启用后的必填条件由跨键 Validator 检查。
	SecretOptional bool
	// Closed 只用于段：段下没有声明的键报错（map 的元素总是 closed）。
	Closed bool
	Help   string
}

// Schema 是一组键的声明。由 Of 从结构体得到的 Schema 还记得结构体类型，Check 会解码一份新值，
// 从而也执行结构体的 ValidateConfig；从数据构造的 Schema（生成器的快照、Merge 的结果）只按键检查。
type Schema struct {
	Keys []Key
	typ  reflect.Type
}

// Validator 由需要跨键规则的配置结构体实现（例如 retry_min ≤ retry_max、生产环境 ops 端点不绑公网）。
// 在声明检查全部通过之后调用；production 表示 env / app.env / environment 是 prod / production。
type Validator interface {
	ValidateConfig(production bool) error
}

var (
	durationType = reflect.TypeOf(time.Duration(0))
	stringsType  = reflect.TypeOf([]string(nil))
	schemaCache  sync.Map // reflect.Type → Schema
)

// MustOf 与 Of 相同，声明写错时 panic：声明是代码常量，写错是编程错误，在第一次调用（测试或启动）时暴露。
func MustOf(config any) Schema {
	schema, err := Of(config)
	if err != nil {
		panic(err)
	}
	return schema
}

// Of 从配置结构体（值或指针）的 tag 推出声明。
func Of(config any) (Schema, error) {
	typ := reflect.TypeOf(config)
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return Schema{}, fmt.Errorf("configschema: %T is not a struct", config)
	}
	if cached, ok := schemaCache.Load(typ); ok {
		return cached.(Schema), nil
	}
	var keys []Key
	if err := collectKeys(typ, "", &keys); err != nil {
		return Schema{}, fmt.Errorf("configschema: %s: %w", typ, err)
	}
	if err := checkKeyTree(keys); err != nil {
		return Schema{}, fmt.Errorf("configschema: %s: %w", typ, err)
	}
	schema := Schema{Keys: keys, typ: typ}
	schemaCache.Store(typ, schema)
	return schema, nil
}

// joinKey 拼出完整键名。嵌套结构体的 tag 以 `_` 结尾时直接拼接（`config:"result_"` 下的 ack_wait 是
// result_ack_wait），用来给一组同形的平铺键共用一个结构体。
func joinKey(prefix, name string) string {
	if prefix == "" || strings.HasSuffix(prefix, "_") {
		return prefix + name
	}
	return prefix + "." + name
}

// collectKeys 按字段顺序收集键。嵌套结构体按 tag 加前缀，匿名嵌入不加前缀，map[string]T 加 `*` 段。
func collectKeys(typ reflect.Type, prefix string, keys *[]Key) error {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, hasTag := field.Tag.Lookup("config")
		name, options, _ := strings.Cut(name, ",")
		if !hasTag {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				if err := collectKeys(field.Type, prefix, keys); err != nil {
					return err
				}
				continue
			}
			if field.IsExported() {
				return fmt.Errorf("field %s has no config tag", field.Name)
			}
			continue
		}
		if !field.IsExported() {
			return fmt.Errorf("field %s has a config tag but is not exported", field.Name)
		}
		if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, " *") {
			return fmt.Errorf("field %s: config key %q must be a non-empty lowercase dotted name", field.Name, name)
		}
		full := joinKey(prefix, name)
		if err := collectField(field.Type, full, field.Tag, options, keys); err != nil {
			return fmt.Errorf("field %s: %w", field.Name, err)
		}
	}
	return nil
}

func collectField(typ reflect.Type, full string, tag reflect.StructTag, options string, keys *[]Key) error {
	switch {
	case typ.Kind() == reflect.Struct && typ != durationType:
		if help := tag.Get("help"); help != "" || options == "closed" {
			*keys = append(*keys, Key{Name: full, Kind: KindSection, Help: help, Closed: options == "closed"})
		}
		return collectKeys(typ, full, keys)
	case typ.Kind() == reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return fmt.Errorf("map keys must be strings")
		}
		key := Key{Name: full, Kind: KindMap, Help: tag.Get("help")}
		key.Example, key.Starter = tag.Lookup("example")
		*keys = append(*keys, key)
		return collectElem(typ.Elem(), full+".*", keys)
	}
	kind, err := kindOf(typ)
	if err != nil {
		return err
	}
	key := Key{
		Name: full, Kind: kind, Default: tag.Get("default"),
		Min: tag.Get("min"), Max: tag.Get("max"),
		Required: tag.Get("required") == "true", Secret: tag.Get("secret") == "true" || tag.Get("secret") == "optional",
		SecretOptional: tag.Get("secret") == "optional",
		Help:           tag.Get("help"),
	}
	key.Example, key.Starter = tag.Lookup("example")
	if enum := tag.Get("enum"); enum != "" {
		if kind != KindString {
			return fmt.Errorf("enum is only for strings")
		}
		for _, item := range strings.Split(enum, "|") {
			key.Enum = append(key.Enum, strings.ToLower(strings.TrimSpace(item)))
		}
	}
	if err := key.checkDeclaration(typ); err != nil {
		return err
	}
	*keys = append(*keys, key)
	return nil
}

// collectElem 声明 map 的元素：结构体元素展开字段，map 元素再加一层 `*`，标量元素就是键本身。
func collectElem(typ reflect.Type, full string, keys *[]Key) error {
	switch {
	case typ.Kind() == reflect.Struct && typ != durationType:
		*keys = append(*keys, Key{Name: full, Kind: KindSection, Closed: true})
		return collectKeys(typ, full, keys)
	case typ.Kind() == reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return fmt.Errorf("map keys must be strings")
		}
		return collectElem(typ.Elem(), full+".*", keys)
	}
	kind, err := kindOf(typ)
	if err != nil {
		return err
	}
	*keys = append(*keys, Key{Name: full, Kind: kind})
	return nil
}

func kindOf(typ reflect.Type) (Kind, error) {
	switch {
	case typ == durationType:
		return KindDuration, nil
	case typ == stringsType:
		return KindStrings, nil
	}
	switch typ.Kind() {
	case reflect.Bool:
		return KindBool, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return KindInt, nil
	case reflect.Float32, reflect.Float64:
		return KindFloat, nil
	case reflect.String:
		return KindString, nil
	}
	return "", fmt.Errorf("unsupported config field type %s", typ)
}

// checkDeclaration 让写错的 default / example / min / max 在 Of 时就报错，而不是等到某次配置缺这个键。
func (k Key) checkDeclaration(typ reflect.Type) error {
	for _, item := range []struct{ name, value string }{{"default", k.Default}, {"min", k.Min}, {"max", k.Max}} {
		if item.value == "" {
			continue
		}
		value, err := parseValue(k, item.value)
		if err != nil {
			return fmt.Errorf("%s %q: %w", item.name, item.value, err)
		}
		if item.name == "default" {
			if err := k.checkValue(value); err != nil {
				return fmt.Errorf("default %q: %w", item.value, err)
			}
			if err := fits(k.Name, typ, value); err != nil {
				return fmt.Errorf("default %q: %w", item.value, err)
			}
		}
	}
	if k.Starter && k.Example != "" && !strings.Contains(k.Example, "{") {
		value, err := parseValue(k, k.Example)
		if err != nil {
			return fmt.Errorf("example %q: %w", k.Example, err)
		}
		if err := k.checkValue(value); err != nil {
			return fmt.Errorf("example %q: %w", k.Example, err)
		}
	}
	return nil
}

// checkKeyTree 拒绝同名键与“既是键又是段”的声明。
func checkKeyTree(keys []Key) error {
	seen := map[string]Kind{}
	for _, key := range keys {
		if previous, ok := seen[key.Name]; ok && !(previous == KindSection && key.Kind == KindSection) {
			return fmt.Errorf("config key %s is declared twice", key.Name)
		}
		seen[key.Name] = key.Kind
	}
	for _, key := range keys {
		for prefix := parentKey(key.Name); prefix != ""; prefix = parentKey(prefix) {
			if kind, ok := seen[prefix]; ok && kind != KindSection && kind != KindMap {
				return fmt.Errorf("config key %s is both a value and the section of %s", prefix, key.Name)
			}
		}
	}
	return nil
}

func parentKey(name string) string {
	if index := strings.LastIndexByte(name, '.'); index >= 0 {
		return name[:index]
	}
	return ""
}

// Merge 合并几份声明。同一个键在两份里出现时，声明必须完全相同（同一个结构体被两个 Mod 共用就是这样），
// 否则报错并点名键：两个 Mod 对同一个键的类型或缺省值意见不一致时，谁先 Init 谁说了算是隐藏的配置缺陷。
func Merge(schemas ...Schema) (Schema, error) {
	var out Schema
	index := map[string]int{}
	var conflicts []string
	for _, schema := range schemas {
		for _, key := range schema.Keys {
			if at, ok := index[key.Name]; ok {
				if !reflect.DeepEqual(out.Keys[at], key) {
					conflicts = append(conflicts, key.Name)
				}
				continue
			}
			index[key.Name] = len(out.Keys)
			out.Keys = append(out.Keys, key)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return out, fmt.Errorf("config: keys declared differently by two mods: %s", strings.Join(conflicts, ", "))
	}
	return out, nil
}

// Lookup 返回名为 name 的键声明。
func (s Schema) Lookup(name string) (Key, bool) {
	for _, key := range s.Keys {
		if key.Name == name {
			return key, true
		}
	}
	return Key{}, false
}

// Sections 返回声明用到的顶层段名（键名的第一段），排序。
func (s Schema) Sections() []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range s.Keys {
		first, _, _ := strings.Cut(key.Name, ".")
		if !seen[first] {
			seen[first] = true
			out = append(out, first)
		}
	}
	sort.Strings(out)
	return out
}
