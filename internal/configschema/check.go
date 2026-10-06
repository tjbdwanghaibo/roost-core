package configschema

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Source 是配置的来源：viper（app 的适配器）或解析后的 YAML（NewMapSource，生成器与 doctor 用）。
// 键一律是小写点分的完整键名。
type Source interface {
	// Get 返回键的值；set 为 false 表示没写（段名也算写了，值是映射）。
	Get(key string) (value any, set bool)
	// Keys 返回全部叶子键。
	Keys() []string
}

// Decode 按 dst（指向配置结构体的指针）的声明从 src 读出全部键：没写的取缺省值，写了的检查类型、范围、枚举、必填、
// 生产密钥；声明检查全部通过后，再依次调用嵌套结构体与 dst 自己的 ValidateConfig。全部错误一次报出（errors.Join）。
func Decode(src Source, dst any, production bool) error {
	value := reflect.ValueOf(dst)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("configschema: Decode needs a pointer to a config struct, got %T", dst)
	}
	if _, err := Of(dst); err != nil {
		return err
	}
	d := decoder{src: src, production: production}
	d.structValue(value.Elem(), "", false)
	if len(d.errs) == 0 {
		d.validate(value.Elem())
	}
	return errors.Join(d.errs...)
}

type decoder struct {
	src        Source
	production bool
	errs       []error
}

func (d *decoder) note(err error) {
	if err != nil {
		d.errs = append(d.errs, err)
	}
}

// structValue 读一个结构体的全部字段；closed 时还拒绝段下没有声明的键。
func (d *decoder) structValue(value reflect.Value, prefix string, closed bool) {
	typ := value.Type()
	known := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag, hasTag := field.Tag.Lookup("config")
		name, options, _ := strings.Cut(tag, ",")
		if !hasTag {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				d.structValue(value.Field(i), prefix, false)
				for _, child := range childNames(field.Type) {
					known[child] = true
				}
			}
			continue
		}
		first, _, _ := strings.Cut(name, ".")
		known[first] = true
		full := joinKey(prefix, name)
		target := value.Field(i)
		switch {
		case field.Type.Kind() == reflect.Struct && field.Type != durationType:
			d.structValue(target, full, options == "closed")
		case field.Type.Kind() == reflect.Map:
			d.mapValue(target, full)
		default:
			key, err := scalarKey(full, field.Type, field.Tag)
			if err != nil {
				d.note(err)
				continue
			}
			d.scalar(target, key, full)
		}
	}
	if closed {
		d.rejectUnknown(prefix, known)
	}
}

// childNames 是结构体字段键名的第一段（匿名嵌入展开），closed 段用它判断哪些子键是声明过的。
func childNames(typ reflect.Type) []string {
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		tag, hasTag := field.Tag.Lookup("config")
		if !hasTag {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				out = append(out, childNames(field.Type)...)
			}
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		first, _, _ := strings.Cut(name, ".")
		out = append(out, first)
	}
	return out
}

func (d *decoder) rejectUnknown(prefix string, known map[string]bool) {
	var unknown []string
	for _, key := range d.src.Keys() {
		rest, ok := strings.CutPrefix(key, prefix+".")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, ".")
		if !known[first] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	sort.Strings(unknown)
	for _, key := range unknown {
		d.note(fmt.Errorf("config: %s is not a known key; %s takes %s", key, prefix, strings.Join(names, ", ")))
	}
}

// mapValue 读 map[string]T 字段：元素名来自配置里实际出现的下一段键名。
func (d *decoder) mapValue(target reflect.Value, prefix string) {
	if raw, set := d.src.Get(prefix); set && raw != nil && !isMapValue(raw) {
		d.note(fmt.Errorf("config: %s must be a map, got %v", prefix, raw))
		return
	}
	names := childSegments(d.src.Keys(), prefix)
	result := reflect.MakeMapWithSize(target.Type(), len(names))
	elemType := target.Type().Elem()
	for _, name := range names {
		full := prefix + "." + name
		elem := reflect.New(elemType).Elem()
		switch {
		case elemType.Kind() == reflect.Struct && elemType != durationType:
			if raw, set := d.src.Get(full); set && raw != nil && !isMapValue(raw) {
				d.note(fmt.Errorf("config: %s must be a map, got %v", full, raw))
				continue
			}
			d.structValue(elem, full, true)
		case elemType.Kind() == reflect.Map:
			d.mapValue(elem, full)
		default:
			kind, err := kindOf(elemType)
			if err != nil {
				d.note(err)
				continue
			}
			d.scalar(elem, Key{Name: full, Kind: kind}, full)
		}
		result.SetMapIndex(reflect.ValueOf(name).Convert(target.Type().Key()), elem)
	}
	target.Set(result)
}

func childSegments(keys []string, prefix string) []string {
	seen := map[string]bool{}
	var out []string
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key, prefix+".")
		if !ok {
			continue
		}
		first, _, _ := strings.Cut(rest, ".")
		if !seen[first] {
			seen[first] = true
			out = append(out, first)
		}
	}
	sort.Strings(out)
	return out
}

func isMapValue(raw any) bool {
	kind := reflect.ValueOf(raw).Kind()
	return kind == reflect.Map
}

// scalar 读一个键并写进字段。
func (d *decoder) scalar(target reflect.Value, key Key, name string) {
	key.Name = name
	value, err := d.read(key)
	if err != nil {
		d.note(err)
		return
	}
	if err := fits(name, target.Type(), value); err != nil {
		d.note(err)
		return
	}
	assign(target, value)
}

// read 读一个键并做全部声明检查，返回解析后的值（没写且没有缺省值时为 nil）。
func (d *decoder) read(key Key) (any, error) {
	raw, set := d.src.Get(key.Name)
	var value any
	if set && raw != nil {
		parsed, err := parseRaw(key, raw)
		if err != nil {
			return nil, err
		}
		value = parsed
	} else if key.Default != "" {
		parsed, err := parseValue(key, key.Default)
		if err != nil {
			return nil, fmt.Errorf("config: %s: declared default %q: %w", key.Name, key.Default, err)
		}
		value = parsed
	}
	if key.Required && isEmpty(value) {
		if key.Starter && key.Example != "" && !strings.ContainsAny(key.Example, "{}") {
			return nil, fmt.Errorf("config: %s is required (for example %s)", key.Name, key.Example)
		}
		return nil, fmt.Errorf("config: %s is required", key.Name)
	}
	if value != nil {
		if err := key.checkValue(value); err != nil {
			return nil, err
		}
	}
	if key.Secret && d.production {
		if text, _ := value.(string); strings.TrimSpace(text) == "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "dev-") {
			return nil, fmt.Errorf("config: production requires non-dev %s", key.Name)
		}
	}
	return value, nil
}

func assign(target reflect.Value, value any) {
	if value == nil {
		target.Set(reflect.Zero(target.Type()))
		return
	}
	switch v := value.(type) {
	case bool:
		target.SetBool(v)
	case int64:
		switch target.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			target.SetUint(uint64(v))
		default:
			target.SetInt(v)
		}
	case float64:
		target.SetFloat(v)
	case time.Duration:
		target.SetInt(int64(v))
	case string:
		target.SetString(v)
	case []string:
		target.Set(reflect.ValueOf(v))
	}
}

// validate 自内向外调用 ValidateConfig：嵌套结构体先，外层后（外层的跨键规则可以依赖内层已经成立）。
// 匿名嵌入的结构体不单独调用：它的 ValidateConfig 被提升为外层的方法，由外层那一次调用执行；外层自己定义了
// ValidateConfig 时要显式调用被嵌入的那个（Go 的方法遮蔽规则）。
func (d *decoder) validate(value reflect.Value) {
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() == reflect.Struct && field.Type != durationType && field.IsExported() && !field.Anonymous {
			d.validate(value.Field(i))
		}
	}
	if value.CanAddr() && value.Addr().CanInterface() {
		if validator, ok := value.Addr().Interface().(Validator); ok {
			d.note(validator.ValidateConfig(d.production))
		}
	}
}

func scalarKey(full string, typ reflect.Type, tag reflect.StructTag) (Key, error) {
	var keys []Key
	_, options, _ := strings.Cut(tag.Get("config"), ",")
	if err := collectField(typ, full, tag, options, &keys); err != nil {
		return Key{}, err
	}
	return keys[0], nil
}

// Check 按声明检查 src，返回全部错误（不写任何值）。由 Of 得到的 Schema 解码一份新的结构体，因而也执行
// ValidateConfig；数据构造的 Schema 只按键检查（类型、范围、枚举、必填、生产密钥、closed 段与 map 下的未知键）。
func (s Schema) Check(src Source, production bool) []error {
	if s.typ != nil {
		return unjoin(Decode(src, reflect.New(s.typ).Interface(), production))
	}
	d := decoder{src: src, production: production}
	for _, key := range s.Keys {
		switch {
		case key.Kind == KindSection:
			if key.Closed && !strings.Contains(key.Name, "*") {
				known := map[string]bool{}
				for _, other := range s.Keys {
					if rest, ok := strings.CutPrefix(other.Name, key.Name+"."); ok {
						first, _, _ := strings.Cut(rest, ".")
						known[first] = true
					}
				}
				d.rejectUnknown(key.Name, known)
			}
		case key.Kind == KindMap:
			if raw, set := src.Get(key.Name); set && raw != nil && !isMapValue(raw) {
				d.note(fmt.Errorf("config: %s must be a map, got %v", key.Name, raw))
			}
		case strings.Contains(key.Name, "*"):
			for _, name := range src.Keys() {
				if matchPattern(key.Name, name) {
					item := key
					item.Name = name
					_, err := d.read(item)
					d.note(err)
				}
			}
		default:
			_, err := d.read(key)
			d.note(err)
		}
	}
	for _, name := range src.Keys() {
		if root, ok := s.mapRoot(name); ok && !s.declares(name) {
			d.note(fmt.Errorf("config: %s is not a known key under %s", name, root))
		}
	}
	return d.errs
}

// Unknown 返回 src 里落在声明用到的顶层段内、却没有任何声明的键（拼错的键名、没有读取方的键）。
// map 字段下的键按通配匹配；值为空的段名（YAML 里只写了 `section:`）不算。
func (s Schema) Unknown(src Source) []string {
	return s.Undeclared(src, s.Sections())
}

// Undeclared 与 Unknown 相同，只是检查的顶层段由调用方给出（doctor 用全部框架声明的段）。
func (s Schema) Undeclared(src Source, sectionNames []string) []string {
	sections := map[string]bool{}
	for _, section := range sectionNames {
		sections[section] = true
	}
	var out []string
	for _, name := range src.Keys() {
		first, _, _ := strings.Cut(name, ".")
		if !sections[first] || s.declares(name) {
			continue
		}
		if raw, _ := src.Get(name); raw == nil && s.isSection(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s Schema) declares(name string) bool {
	for _, key := range s.Keys {
		if key.Kind != KindSection && matchPattern(key.Name, name) {
			return true
		}
		// map 字段的值是列表或标量之外的东西时，叶子就是 map 本身之下的键。
		if key.Kind == KindMap && strings.HasPrefix(name, key.Name+".") && !s.hasDeclaredElements(key.Name) {
			return true
		}
	}
	return false
}

func (s Schema) hasDeclaredElements(mapName string) bool {
	for _, key := range s.Keys {
		if strings.HasPrefix(key.Name, mapName+".*") {
			return true
		}
	}
	return false
}

func (s Schema) isSection(name string) bool {
	for _, key := range s.Keys {
		if strings.HasPrefix(key.Name, name+".") {
			return true
		}
	}
	return false
}

func (s Schema) mapRoot(name string) (string, bool) {
	for _, key := range s.Keys {
		if key.Kind == KindMap && strings.HasPrefix(name, key.Name+".") {
			return key.Name, true
		}
	}
	return "", false
}

// matchPattern 按段匹配键名，`*` 匹配恰好一段。
func matchPattern(pattern, name string) bool {
	p := strings.Split(pattern, ".")
	n := strings.Split(name, ".")
	if len(p) != len(n) {
		return false
	}
	for i := range p {
		if p[i] != "*" && p[i] != n[i] {
			return false
		}
	}
	return true
}

func unjoin(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// NewMapSource 把解析后的 YAML（嵌套映射）变成 Source。键名转成小写（与 viper 一致）。
func NewMapSource(document map[string]any) Source {
	source := mapSource{values: map[string]any{}}
	source.flatten("", document)
	sort.Strings(source.keys)
	return source
}

type mapSource struct {
	values map[string]any
	keys   []string
}

func (m *mapSource) flatten(prefix string, value any) {
	switch nested := value.(type) {
	case map[string]any:
		if prefix != "" {
			m.values[prefix] = nested
		}
		for name, child := range nested {
			m.flatten(joinKey(prefix, strings.ToLower(name)), child)
		}
		return
	case map[any]any:
		converted := make(map[string]any, len(nested))
		for name, child := range nested {
			converted[fmt.Sprint(name)] = child
		}
		m.flatten(prefix, converted)
		return
	}
	m.values[prefix] = value
	m.keys = append(m.keys, prefix)
}

func (m mapSource) Get(key string) (any, bool) {
	value, ok := m.values[key]
	return value, ok
}

func (m mapSource) Keys() []string { return m.keys }
