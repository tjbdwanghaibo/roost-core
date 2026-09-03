package cache

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

const (
	defaultRefHMapPrefix   = "roost:redisdao"
	defaultRefHMapMaxDepth = 8
	refHMapRegistryField   = "__keys"
)

var (
	ErrRefHMapCycle       = errors.New("cache: redis ref hmap cycle")
	ErrRefHMapMaxDepth    = errors.New("cache: redis ref hmap max depth exceeded")
	ErrRefHMapUnsupported = errors.New("cache: redis ref hmap unsupported field")
)

var (
	textMarshalerType   = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

const refHMapWriteScript = `
local ttl_ms = tonumber(ARGV[1])
local write_count = tonumber(ARGV[2])
local arg = 3
for i = 1, #KEYS do
	redis.call("DEL", KEYS[i])
end
for i = 1, write_count do
	local key_index = tonumber(ARGV[arg])
	arg = arg + 1
	local pair_count = tonumber(ARGV[arg])
	arg = arg + 1
	local values = {}
	for j = 1, pair_count * 2 do
		values[j] = ARGV[arg]
		arg = arg + 1
	end
	if #values > 0 then
		redis.call("HSET", KEYS[key_index], unpack(values))
		if ttl_ms > 0 then
			redis.call("PEXPIRE", KEYS[key_index], ttl_ms)
		end
	end
end
return 1
`

type RefHMapKeyStringFunc[K comparable] func(K) string

type RefHMapPatcher[K comparable] interface {
	Patch(ctx context.Context, key K, path string, value any) error
}

type RefHMapConfig[K comparable, V any] struct {
	Prefix      string
	Name        string
	TTL         time.Duration
	MaxDepth    int
	KeyString   RefHMapKeyStringFunc[K]
	StoreConfig StoreConfig[K, V]
}

type RedisRefHMapStore[K comparable, V any] struct {
	layoutOnce   sync.Once
	layoutRoot   *refHMapNode
	layoutPrefix string
	layoutErr    error

	redis fredis.IRedis
	cfg   RefHMapConfig[K, V]
}

func NewRedisRefHMapStore[K comparable, V any](redis fredis.IRedis, cfg RefHMapConfig[K, V]) *RedisRefHMapStore[K, V] {
	return &RedisRefHMapStore[K, V]{
		redis: redis,
		cfg:   cfg,
	}
}

func (s *RedisRefHMapStore[K, V]) Get(ctx context.Context, key K) (V, bool, error) {
	var zero V
	if s == nil || s.redis == nil || !s.cfg.StoreConfig.validKey(key) {
		return zero, false, nil
	}
	plan, err := s.plan(key)
	if err != nil {
		return zero, false, err
	}
	hashes, err := s.loadHashes(ctx, plan)
	if err != nil {
		return zero, false, err
	}
	rootHash := hashes[plan.key(plan.root)]
	if len(rootHash) == 0 {
		return zero, false, nil
	}
	value := reflect.New(plan.root.typ).Elem()
	if err := decodeRefHMapNode(value, plan.root, hashes, plan.base); err != nil {
		return zero, false, err
	}
	return value.Interface().(V), true, nil
}

func (s *RedisRefHMapStore[K, V]) Set(ctx context.Context, value V) error {
	if s == nil || s.redis == nil {
		return nil
	}
	if s.cfg.StoreConfig.ValidateValue != nil {
		if err := s.cfg.StoreConfig.ValidateValue(value); err != nil {
			return err
		}
	}
	key, err := s.cfg.StoreConfig.keyOf(value)
	if err != nil {
		return err
	}
	plan, err := s.plan(key)
	if err != nil {
		return err
	}
	if s.cfg.StoreConfig.Stale != nil {
		old, ok, err := s.Get(ctx, key)
		if err != nil {
			return err
		}
		if ok && s.cfg.StoreConfig.Stale(old, value) {
			return nil
		}
	}
	writes, err := encodeRefHMapNode(reflect.ValueOf(value), plan.root, plan.base)
	if err != nil {
		return err
	}
	return s.writeHashes(ctx, plan, writes)
}

func (s *RedisRefHMapStore[K, V]) Delete(ctx context.Context, key K) error {
	if s == nil || s.redis == nil || !s.cfg.StoreConfig.validKey(key) {
		return nil
	}
	plan, err := s.plan(key)
	if err != nil {
		return err
	}
	keys, err := s.registeredKeys(ctx, plan)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	if pipe := s.redis.Pipeline(); pipe != nil {
		pipe.Del(ctx, keys...)
		return pipe.Exec(ctx)
	}
	_, err = s.redis.Del(ctx, keys...)
	return err
}

// layout builds the reflective type tree and the key prefix template once per
// store. Both are decided by V and the store config, neither of which can
// change, so doing this per operation was pure waste — it dominated a Get's
// allocations while the network round trip it accompanies dwarfed everything.
func (s *RedisRefHMapStore[K, V]) layout() (*refHMapNode, string, error) {
	s.layoutOnce.Do(func() {
		var zero V
		rootType := reflect.TypeOf(zero)
		if rootType == nil {
			s.layoutErr = fmt.Errorf("%w: nil root type", ErrRefHMapUnsupported)
			return
		}
		if rootType.Kind() == reflect.Pointer {
			rootType = rootType.Elem()
		}
		if rootType.Kind() != reflect.Struct {
			s.layoutErr = fmt.Errorf("%w: root type %s is not struct", ErrRefHMapUnsupported, rootType)
			return
		}
		maxDepth := s.cfg.MaxDepth
		if maxDepth <= 0 {
			maxDepth = defaultRefHMapMaxDepth
		}
		name := strings.TrimSpace(s.cfg.Name)
		if name == "" {
			name = refHMapSnake(rootType.Name())
		}
		if name == "" {
			name = "value"
		}
		prefix := strings.TrimRight(strings.TrimSpace(s.cfg.Prefix), ":")
		if prefix == "" {
			prefix = defaultRefHMapPrefix
		}
		root, err := buildRefHMapNode(rootType, nil, maxDepth, 0, make(map[reflect.Type]bool))
		if err != nil {
			s.layoutErr = err
			return
		}
		s.layoutRoot = root
		s.layoutPrefix = prefix + ":{" + name + ":"
	})
	return s.layoutRoot, s.layoutPrefix, s.layoutErr
}

func (s *RedisRefHMapStore[K, V]) plan(key K) (*refHMapPlan, error) {
	root, prefix, err := s.layout()
	if err != nil {
		return nil, err
	}
	keyString := fmt.Sprint(key)
	if s.cfg.KeyString != nil {
		keyString = s.cfg.KeyString(key)
	}
	return &refHMapPlan{root: root, base: prefix + keyString + "}"}, nil
}

func (s *RedisRefHMapStore[K, V]) Patch(ctx context.Context, key K, path string, value any) error {
	if s == nil || s.redis == nil || !s.cfg.StoreConfig.validKey(key) {
		return nil
	}
	plan, err := s.plan(key)
	if err != nil {
		return err
	}
	target, err := plan.patchTarget(path)
	if err != nil {
		return err
	}
	raw, err := encodeRefHMapScalarForType(value, target.field.scalar)
	if err != nil {
		return err
	}
	if pipe := s.redis.Pipeline(); pipe != nil {
		pipe.HSet(ctx, target.key, target.field.name, raw)
		if s.cfg.TTL > 0 {
			pipe.Expire(ctx, target.key, s.cfg.TTL)
		}
		return pipe.Exec(ctx)
	}
	if err := s.redis.HSet(ctx, target.key, target.field.name, raw); err != nil {
		return err
	}
	if s.cfg.TTL > 0 {
		_, err = s.redis.Expire(ctx, target.key, s.cfg.TTL)
	}
	return err
}

func (s *RedisRefHMapStore[K, V]) loadHashes(ctx context.Context, plan *refHMapPlan) (map[string]map[string]string, error) {
	keys := plan.keys()
	out := make(map[string]map[string]string, len(keys))
	if pipe := s.redis.Pipeline(); pipe != nil {
		futures := make(map[string]*fredis.FutureStringMap, len(keys))
		for _, key := range keys {
			futures[key] = pipe.HGetAll(ctx, key)
		}
		if err := pipe.Exec(ctx); err != nil {
			return nil, err
		}
		for key, future := range futures {
			value, err := future.Result()
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	}
	for _, key := range keys {
		value, err := s.redis.HGetAll(ctx, key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

func (s *RedisRefHMapStore[K, V]) writeHashes(ctx context.Context, plan *refHMapPlan, writes []refHMapWrite) error {
	writes = plan.withRegistry(writes)
	deleteKeys, err := s.registeredKeys(ctx, plan)
	if err != nil {
		return err
	}
	keys := uniqueRefHMapKeys(append(deleteKeys, plan.keys()...))
	return s.evalWriteHashes(ctx, keys, writes)
}

func (s *RedisRefHMapStore[K, V]) registeredKeys(ctx context.Context, plan *refHMapPlan) ([]string, error) {
	fallback := plan.keys()
	raw, err := s.redis.HGet(ctx, plan.key(plan.root), refHMapRegistryField)
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			return fallback, nil
		}
		return nil, err
	}
	keys := parseRefHMapRegistry(string(raw))
	if len(keys) == 0 {
		return fallback, nil
	}
	return uniqueRefHMapKeys(append(keys, plan.key(plan.root))), nil
}

func (s *RedisRefHMapStore[K, V]) evalWriteHashes(ctx context.Context, keys []string, writes []refHMapWrite) error {
	keyIndex := make(map[string]int, len(keys))
	for i, key := range keys {
		keyIndex[key] = i + 1
	}
	args := make([]any, 0, 2+len(writes)*4)
	args = append(args, strconv.FormatInt(s.cfg.TTL.Milliseconds(), 10), strconv.Itoa(len(writes)))
	for _, write := range writes {
		idx, ok := keyIndex[write.key]
		if !ok {
			keys = append(keys, write.key)
			idx = len(keys)
			keyIndex[write.key] = idx
		}
		args = append(args, strconv.Itoa(idx), strconv.Itoa(len(write.values)/2))
		args = append(args, write.values...)
	}
	if _, err := s.redis.Eval(ctx, refHMapWriteScript, keys, args...); err == nil {
		return nil
	} else {
		// The fallbacks below are NOT atomic: a concurrent reader can observe
		// the delete before the re-write lands. Degrading silently would hide
		// exactly the window operators need to know about, so it is logged
		// and counted before availability is chosen over atomicity.
		slog.Warn("cache: redis ref hmap Lua write failed, degrading to non-atomic fallback", "keys", len(keys), "err", err)
		metrics.IncCounter("cache.refhmap.write_degraded_total", nil, 1)
	}
	if pipe := s.redis.Pipeline(); pipe != nil {
		pipe.Del(ctx, keys...)
		for _, write := range writes {
			if len(write.values) == 0 {
				continue
			}
			pipe.HSet(ctx, write.key, write.values...)
			if s.cfg.TTL > 0 {
				pipe.Expire(ctx, write.key, s.cfg.TTL)
			}
		}
		return pipe.Exec(ctx)
	}
	if _, err := s.redis.Del(ctx, keys...); err != nil {
		return err
	}
	for _, write := range writes {
		if len(write.values) == 0 {
			continue
		}
		if err := s.redis.HSet(ctx, write.key, write.values...); err != nil {
			return err
		}
		if s.cfg.TTL > 0 {
			if _, err := s.redis.Expire(ctx, write.key, s.cfg.TTL); err != nil {
				return err
			}
		}
	}
	return nil
}

type refHMapPlan struct {
	root *refHMapNode
	base string
}

// key resolves one node's Redis key for this plan's entity.
func (p *refHMapPlan) key(node *refHMapNode) string {
	if p == nil || node == nil {
		return ""
	}
	return p.base + node.suffix
}

func (p *refHMapPlan) keys() []string {
	if p == nil || p.root == nil {
		return nil
	}
	var keys []string
	p.root.collectKeys(p.base, &keys)
	return keys
}

func (p *refHMapPlan) withRegistry(writes []refHMapWrite) []refHMapWrite {
	if p == nil || p.root == nil {
		return writes
	}
	registry := strings.Join(p.keys(), "\n")
	out := make([]refHMapWrite, len(writes))
	copy(out, writes)
	for i := range out {
		rootKey := p.key(p.root)
		if out[i].key == rootKey {
			out[i].values = append(out[i].values, refHMapRegistryField, registry)
			return out
		}
	}
	out = append(out, refHMapWrite{key: p.key(p.root), values: []any{refHMapRegistryField, registry}})
	return out
}

func (p *refHMapPlan) patchTarget(path string) (refHMapPatchTarget, error) {
	if p == nil || p.root == nil {
		return refHMapPatchTarget{}, fmt.Errorf("%w: empty plan", ErrRefHMapUnsupported)
	}
	parts := strings.Split(strings.TrimSpace(path), ".")
	node := p.root
	for i, part := range parts {
		if part == "" {
			return refHMapPatchTarget{}, fmt.Errorf("%w: empty patch path %q", ErrRefHMapUnsupported, path)
		}
		field, ok := node.findField(part)
		if !ok {
			return refHMapPatchTarget{}, fmt.Errorf("%w: unknown patch path %q", ErrRefHMapUnsupported, path)
		}
		if i == len(parts)-1 {
			if field.kind != refHMapScalarField {
				return refHMapPatchTarget{}, fmt.Errorf("%w: patch path %q is not scalar", ErrRefHMapUnsupported, path)
			}
			return refHMapPatchTarget{node: node, field: field, key: p.key(node)}, nil
		}
		if field.kind != refHMapStructField || field.child == nil {
			return refHMapPatchTarget{}, fmt.Errorf("%w: patch path %q crosses non-struct field", ErrRefHMapUnsupported, path)
		}
		node = field.child
	}
	return refHMapPatchTarget{}, fmt.Errorf("%w: empty patch path", ErrRefHMapUnsupported)
}

type refHMapPatchTarget struct {
	node  *refHMapNode
	field refHMapField
	key   string
}

// refHMapNode describes one Redis hash in the flattened struct. It holds a
// key SUFFIX, not a key: the suffix is decided entirely by the value's type,
// while the prefix carries the entity key. Keeping them apart is what lets the
// whole tree be built once per store instead of once per operation — the tree
// used to be rebuilt by reflection on every Get/Set/Delete/Patch, which cost
// more than half of a Get's time and allocations even though V is a type
// parameter and the structure never changes.
type refHMapNode struct {
	typ    reflect.Type
	suffix string
	fields []refHMapField
}

func (n *refHMapNode) collectKeys(base string, keys *[]string) {
	if n == nil {
		return
	}
	*keys = append(*keys, base+n.suffix)
	for _, field := range n.fields {
		if field.child != nil {
			field.child.collectKeys(base, keys)
		}
	}
}

func (n *refHMapNode) findField(name string) (refHMapField, bool) {
	for _, field := range n.fields {
		if field.name == name || field.goName == name {
			return field, true
		}
	}
	return refHMapField{}, false
}

type refHMapFieldKind uint8

const (
	refHMapScalarField refHMapFieldKind = iota + 1
	refHMapStructField
)

type refHMapField struct {
	index    []int
	goName   string
	name     string
	kind     refHMapFieldKind
	ptr      bool
	child    *refHMapNode
	scalar   reflect.Type
	fieldTyp reflect.Type
}

type refHMapWrite struct {
	key    string
	values []any
}

func buildRefHMapNode(typ reflect.Type, path []string, maxDepth int, depth int, stack map[reflect.Type]bool) (*refHMapNode, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: %s", ErrRefHMapMaxDepth, typ)
	}
	if stack[typ] {
		return nil, fmt.Errorf("%w: %s", ErrRefHMapCycle, typ)
	}
	stack[typ] = true
	defer delete(stack, typ)

	suffix := ":root"
	if len(path) > 0 {
		suffix = ":" + strings.Join(path, ":")
	}
	node := &refHMapNode{
		typ:    typ,
		suffix: suffix,
	}
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		name, ok := refHMapFieldName(sf)
		if !ok {
			continue
		}
		fieldType := sf.Type
		if isRefHMapScalar(fieldType) {
			node.fields = append(node.fields, refHMapField{
				index:    sf.Index,
				goName:   sf.Name,
				name:     name,
				kind:     refHMapScalarField,
				ptr:      fieldType.Kind() == reflect.Pointer,
				scalar:   refHMapDeref(fieldType),
				fieldTyp: fieldType,
			})
			continue
		}
		childType := fieldType
		ptr := false
		if childType.Kind() == reflect.Pointer {
			ptr = true
			childType = childType.Elem()
		}
		if childType.Kind() == reflect.Struct {
			child, err := buildRefHMapNode(childType, append(path, name), maxDepth, depth+1, stack)
			if err != nil {
				return nil, err
			}
			node.fields = append(node.fields, refHMapField{
				index:    sf.Index,
				goName:   sf.Name,
				name:     name,
				kind:     refHMapStructField,
				ptr:      ptr,
				child:    child,
				fieldTyp: fieldType,
			})
			continue
		}
		return nil, fmt.Errorf("%w: %s.%s %s", ErrRefHMapUnsupported, typ.Name(), sf.Name, fieldType)
	}
	return node, nil
}

func encodeRefHMapNode(value reflect.Value, node *refHMapNode, base string) ([]refHMapWrite, error) {
	value = refHMapIndirectValue(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: value is not struct", ErrRefHMapUnsupported)
	}
	values := make([]any, 0, len(node.fields)*2)
	var writes []refHMapWrite
	for _, field := range node.fields {
		fv := value.FieldByIndex(field.index)
		switch field.kind {
		case refHMapScalarField:
			raw, ok, err := encodeRefHMapScalar(fv)
			if err != nil {
				return nil, err
			}
			if ok {
				values = append(values, field.name, raw)
			}
		case refHMapStructField:
			if field.ptr && fv.IsNil() {
				continue
			}
			values = append(values, field.name, base+field.child.suffix)
			childWrites, err := encodeRefHMapNode(fv, field.child, base)
			if err != nil {
				return nil, err
			}
			writes = append(writes, childWrites...)
		}
	}
	writes = append(writes, refHMapWrite{key: base + node.suffix, values: values})
	return writes, nil
}

func decodeRefHMapNode(value reflect.Value, node *refHMapNode, hashes map[string]map[string]string, base string) error {
	value = refHMapIndirectValue(value)
	hash := hashes[base+node.suffix]
	for _, field := range node.fields {
		raw, ok := hash[field.name]
		fv := value.FieldByIndex(field.index)
		switch field.kind {
		case refHMapScalarField:
			if !ok {
				continue
			}
			if err := decodeRefHMapScalar(fv, raw); err != nil {
				return err
			}
		case refHMapStructField:
			if !ok || raw == "" {
				continue
			}
			if field.ptr {
				if fv.IsNil() {
					fv.Set(reflect.New(field.child.typ))
				}
				if err := decodeRefHMapNode(fv, field.child, hashes, base); err != nil {
					return err
				}
			} else if err := decodeRefHMapNode(fv, field.child, hashes, base); err != nil {
				return err
			}
		}
	}
	return nil
}

func encodeRefHMapScalar(value reflect.Value) (string, bool, error) {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", false, nil
		}
		value = value.Elem()
	}
	if raw, ok, err := encodeRefHMapTextScalar(value); ok || err != nil {
		return raw, ok, err
	}
	switch value.Kind() {
	case reflect.String:
		return value.String(), true, nil
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), true, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), true, nil
	case reflect.Float32:
		return strconv.FormatFloat(value.Float(), 'g', -1, 32), true, nil
	case reflect.Float64:
		return strconv.FormatFloat(value.Float(), 'g', -1, 64), true, nil
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return string(value.Bytes()), true, nil
		}
	}
	return "", false, fmt.Errorf("%w: scalar %s", ErrRefHMapUnsupported, value.Type())
}

func decodeRefHMapScalar(value reflect.Value, raw string) error {
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		value = value.Elem()
	}
	if ok, err := decodeRefHMapTextScalar(value, raw); ok || err != nil {
		return err
	}
	switch value.Kind() {
	case reflect.String:
		value.SetString(raw)
		return nil
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		value.SetBool(parsed)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 10, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetInt(parsed)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		parsed, err := strconv.ParseUint(raw, 10, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetUint(parsed)
		return nil
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(raw, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetFloat(parsed)
		return nil
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			value.SetBytes([]byte(raw))
			return nil
		}
	}
	return fmt.Errorf("%w: scalar %s", ErrRefHMapUnsupported, value.Type())
}

func PatchStructPath(target any, path string, value any) error {
	rv := reflect.ValueOf(target)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("%w: patch target must be non-nil pointer", ErrRefHMapUnsupported)
	}
	current := rv.Elem()
	parts := strings.Split(strings.TrimSpace(path), ".")
	for i, part := range parts {
		if part == "" {
			return fmt.Errorf("%w: empty patch path %q", ErrRefHMapUnsupported, path)
		}
		if current.Kind() == reflect.Pointer {
			if current.IsNil() {
				current.Set(reflect.New(current.Type().Elem()))
			}
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return fmt.Errorf("%w: patch path %q crosses non-struct value", ErrRefHMapUnsupported, path)
		}
		field, ok := findRefHMapStructField(current.Type(), part)
		if !ok {
			return fmt.Errorf("%w: unknown patch path %q", ErrRefHMapUnsupported, path)
		}
		current = current.FieldByIndex(field.Index)
		if i == len(parts)-1 {
			return setRefHMapValue(current, value)
		}
	}
	return fmt.Errorf("%w: empty patch path", ErrRefHMapUnsupported)
}

func isRefHMapScalar(typ reflect.Type) bool {
	typ = refHMapDeref(typ)
	if isRefHMapTextScalar(typ) {
		return true
	}
	switch typ.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	case reflect.Slice:
		return typ.Elem().Kind() == reflect.Uint8
	default:
		return false
	}
}

func encodeRefHMapScalarForType(value any, typ reflect.Type) (string, error) {
	if typ == nil {
		return "", fmt.Errorf("%w: nil scalar type", ErrRefHMapUnsupported)
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "", fmt.Errorf("%w: nil patch value", ErrRefHMapUnsupported)
	}
	if rv.Type().AssignableTo(typ) {
		raw, _, err := encodeRefHMapScalar(rv)
		return raw, err
	}
	if rv.Type().ConvertibleTo(typ) {
		raw, _, err := encodeRefHMapScalar(rv.Convert(typ))
		return raw, err
	}
	return "", fmt.Errorf("%w: cannot assign %s to %s", ErrRefHMapUnsupported, rv.Type(), typ)
}

func isRefHMapTextScalar(typ reflect.Type) bool {
	typ = refHMapDeref(typ)
	return typ.Implements(textMarshalerType) ||
		reflect.PointerTo(typ).Implements(textMarshalerType) ||
		reflect.PointerTo(typ).Implements(textUnmarshalerType)
}

func encodeRefHMapTextScalar(value reflect.Value) (string, bool, error) {
	if value.Type().Implements(textMarshalerType) {
		raw, err := value.Interface().(encoding.TextMarshaler).MarshalText()
		return string(raw), true, err
	}
	if value.CanAddr() {
		ptr := value.Addr()
		if ptr.Type().Implements(textMarshalerType) {
			raw, err := ptr.Interface().(encoding.TextMarshaler).MarshalText()
			return string(raw), true, err
		}
	}
	return "", false, nil
}

func decodeRefHMapTextScalar(value reflect.Value, raw string) (bool, error) {
	if value.CanAddr() {
		ptr := value.Addr()
		if ptr.Type().Implements(textUnmarshalerType) {
			return true, ptr.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(raw))
		}
	}
	return false, nil
}

func refHMapDeref(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

func refHMapIndirectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			value.Set(reflect.New(value.Type().Elem()))
		}
		value = value.Elem()
	}
	return value
}

func refHMapFieldName(field reflect.StructField) (string, bool) {
	if tag := field.Tag.Get("redisdao"); tag != "" {
		if tag == "-" {
			return "", false
		}
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			return parts[0], true
		}
	}
	if tag := field.Tag.Get("json"); tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] == "-" {
			return "", false
		}
		if parts[0] != "" {
			return parts[0], true
		}
	}
	return refHMapSnake(field.Name), true
}

func findRefHMapStructField(typ reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		redisName, ok := refHMapFieldName(field)
		if !ok {
			continue
		}
		if field.Name == name || redisName == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func setRefHMapValue(target reflect.Value, value any) error {
	if target.Kind() == reflect.Pointer {
		if value == nil {
			target.Set(reflect.Zero(target.Type()))
			return nil
		}
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}
	next := reflect.ValueOf(value)
	if !next.IsValid() {
		return fmt.Errorf("%w: nil patch value", ErrRefHMapUnsupported)
	}
	if next.Type().AssignableTo(target.Type()) {
		target.Set(next)
		return nil
	}
	if next.Type().ConvertibleTo(target.Type()) {
		target.Set(next.Convert(target.Type()))
		return nil
	}
	return fmt.Errorf("%w: cannot assign %s to %s", ErrRefHMapUnsupported, next.Type(), target.Type())
}

func parseRefHMapRegistry(raw string) []string {
	var keys []string
	for _, key := range strings.Split(raw, "\n") {
		key = strings.TrimSpace(key)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

func uniqueRefHMapKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

func refHMapSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

var _ Store[int64, refHMapCompileAssert] = (*RedisRefHMapStore[int64, refHMapCompileAssert])(nil)

type refHMapCompileAssert struct {
	ID int64
}
