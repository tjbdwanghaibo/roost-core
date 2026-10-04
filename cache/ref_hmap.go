package cache

import (
	"context"
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

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
	// ErrRefHMapRegistryChanged means the key registry changed after it was
	// read. This attempt wrote nothing; read the current record before retrying.
	// It is a schema/key-set conflict, not a value/version compare-and-set.
	ErrRefHMapRegistryChanged = errors.New("cache: redis ref hmap registry changed")
)

// errRefHMapPartialRecord：根引用了一个必然非空的子 hash，它却已不存在。
// RR-20261004-03：子 hash 单独过期（修复前 Patch 只续期路径）或被外部删除，
// 读到的只是半条记录；Get 把它当整条 miss，让上层按缺失重载，而不是返回
// ok=true 的部分值。它不是持久损坏：下一次 Set 会整条重写。
var errRefHMapPartialRecord = errors.New("cache: redis ref hmap referenced hash missing")

var (
	textMarshalerType   = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

const refHMapWriteScript = `
local ttl_ms = tonumber(ARGV[1])
local write_count = tonumber(ARGV[2])
-- RR-20261004-NC-30: refuse an obsolete cleanup snapshot before any DEL.
if (redis.call("HGET", KEYS[1], "__keys") or "") ~= ARGV[3] then
	return 0
end
local arg = 4
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

// RR-20261004-NC-30：清理清单与根 registry 在同槽 Lua 内一起裁决。
// 不在脚本里发现未声明键，不自动重试或补偿未知结果。
const refHMapDeleteScript = `
if (redis.call("HGET", KEYS[1], "__keys") or "") ~= ARGV[1] then
	return 0
end
for i = 1, #KEYS do redis.call("DEL", KEYS[i]) end
return 1
`

// RR-20261004-NC-18：同槽内先检查类型，再补齐祖先引用和叶字段。
// 不创建缺失的整条记录；错误/未知结果不降级为非原子重写。
//
// KEYS[1..ARGV[4]] 是根到叶的路径，其后是布局里其余的 hash；全部同属一个
// {name:key} hash tag，脚本只访问声明过的键。ARGV：1 TTL 毫秒，2 叶字段，
// 3 叶值，4 路径长度，5.. 路径上每一层的引用字段名。
//
// RR-20261004-03：续期覆盖整条记录的全部布局 hash，而不只是路径。原先兄弟
// hash 保留上一次 Set 的 TTL，过期后 Get 读出 ok=true 的部分记录。注册表并入
// 全部布局键（与 Set 写的注册表一致），没有 __keys 的旧数据被 Patch 后，
// Delete 仍能找到兄弟 hash。
const refHMapPatchScript = `
local path_count = tonumber(ARGV[4])
for i = 1, path_count do
	local kind = redis.call("TYPE", KEYS[i]).ok
	if kind ~= "none" and kind ~= "hash" then
		return redis.error_reply("WRONGTYPE ref hmap patch requires hashes")
	end
end
if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end
local registry = redis.call("HGET", KEYS[1], "__keys") or ""
for i = 1, #KEYS do
	if not string.find("\n" .. registry .. "\n", "\n" .. KEYS[i] .. "\n", 1, true) then
		if registry ~= "" then registry = registry .. "\n" end
		registry = registry .. KEYS[i]
	end
end
for i = 1, path_count - 1 do
	redis.call("HSET", KEYS[i], ARGV[4+i], KEYS[i+1])
end
redis.call("HSET", KEYS[path_count], ARGV[2], ARGV[3])
redis.call("HSET", KEYS[1], "__keys", registry)
local ttl_ms = tonumber(ARGV[1])
if ttl_ms > 0 then
	for i = 1, #KEYS do redis.call("PEXPIRE", KEYS[i], ttl_ms) end
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
		if errors.Is(err, errRefHMapPartialRecord) {
			return zero, false, nil
		}
		return zero, false, err
	}
	// RR-20261004-NC-16：layout保留struct内容，返回仍须匹配调用方的V形状。
	rootType := reflect.TypeOf((*V)(nil)).Elem()
	if rootType.Kind() == reflect.Pointer {
		return value.Addr().Convert(rootType).Interface().(V), true, nil
	}
	return value.Interface().(V), true, nil
}

func (s *RedisRefHMapStore[K, V]) Set(ctx context.Context, value V) error {
	if s == nil || s.redis == nil {
		return nil
	}
	// nil根不能交给业务KeyOf，也不能由反射偷偷分配并修改调用方。
	rv := reflect.ValueOf(value)
	if rv.IsValid() && rv.Kind() == reflect.Pointer && rv.IsNil() {
		return fmt.Errorf("%w: nil root value", ErrRefHMapUnsupported)
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
			return ErrStaleWrite
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
	keys, registry, err := s.registeredKeys(ctx, plan)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	result, err := s.redis.Eval(ctx, refHMapDeleteScript, keys, registry)
	return refHMapWriteResult(result, err)
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
		// RR-20261004-NC-19：在任何I/O之前拒绝物理hash键的别名。
		var keys []string
		root.collectKeys("", &keys)
		if len(uniqueRefHMapKeys(keys)) != len(keys) {
			s.layoutErr = fmt.Errorf("%w: duplicate hash paths", ErrRefHMapUnsupported)
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
	args := []any{strconv.FormatInt(s.cfg.TTL.Milliseconds(), 10), target.field.name, raw, strconv.Itoa(len(target.keys))}
	for _, name := range target.references {
		args = append(args, name)
	}
	// RR-20261004-03：路径之后声明布局里其余的 hash，脚本把整条记录一起续期。
	keys := uniqueRefHMapKeys(append(append([]string(nil), target.keys...), plan.keys()...))
	applied, err := s.redis.Eval(ctx, refHMapPatchScript, keys, args...)
	if err != nil {
		return err
	}
	if applied != int64(1) {
		return fmt.Errorf("%w: patch %q requires an existing root", ErrRefHMapUnsupported, path)
	}
	return nil
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
	deleteKeys, registry, err := s.registeredKeys(ctx, plan)
	if err != nil {
		return err
	}
	keys := uniqueRefHMapKeys(append(deleteKeys, plan.keys()...))
	return s.evalWriteHashes(ctx, keys, writes, registry)
}

func (s *RedisRefHMapStore[K, V]) registeredKeys(ctx context.Context, plan *refHMapPlan) ([]string, string, error) {
	fallback := plan.keys()
	raw, err := s.redis.HGet(ctx, plan.key(plan.root), refHMapRegistryField)
	if err != nil {
		if errors.Is(err, fredis.ErrNil) {
			return fallback, "", nil
		}
		return nil, "", err
	}
	keys := parseRefHMapRegistry(string(raw))
	if len(keys) == 0 {
		return fallback, string(raw), nil
	}
	// The Lua guard always checks KEYS[1], regardless of legacy registry order.
	return uniqueRefHMapKeys(append([]string{plan.key(plan.root)}, keys...)), string(raw), nil
}

func (s *RedisRefHMapStore[K, V]) evalWriteHashes(ctx context.Context, keys []string, writes []refHMapWrite, registry string) error {
	keyIndex := make(map[string]int, len(keys))
	for i, key := range keys {
		keyIndex[key] = i + 1
	}
	args := make([]any, 0, 2+len(writes)*4)
	args = append(args, strconv.FormatInt(s.cfg.TTL.Milliseconds(), 10), strconv.Itoa(len(writes)), registry)
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
	// RR-20261004-NC-21：错误不能证明 Lua 未执行；无身份重放会覆盖后续写。
	// 保留原始原因，让调用方区分失败与结果未知，不自动回滚或降级。
	result, err := s.redis.Eval(ctx, refHMapWriteScript, keys, args...)
	return refHMapWriteResult(result, err)
}

func refHMapWriteResult(result any, err error) error {
	if err != nil {
		return err
	}
	switch result {
	case int64(1):
		return nil
	case int64(0):
		return ErrRefHMapRegistryChanged
	default:
		return fmt.Errorf("%w: unexpected write reply %v", ErrRefHMapUnsupported, result)
	}
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
	keys := []string{p.key(node)}
	var references []string
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
			return refHMapPatchTarget{node: node, field: field, key: p.key(node), keys: keys, references: references}, nil
		}
		if field.kind != refHMapStructField || field.child == nil {
			return refHMapPatchTarget{}, fmt.Errorf("%w: patch path %q crosses non-struct field", ErrRefHMapUnsupported, path)
		}
		references = append(references, field.name)
		node = field.child
		keys = append(keys, p.key(node))
	}
	return refHMapPatchTarget{}, fmt.Errorf("%w: empty patch path", ErrRefHMapUnsupported)
}

type refHMapPatchTarget struct {
	node       *refHMapNode
	field      refHMapField
	key        string
	keys       []string
	references []string
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
	// alwaysWritten：这一层有非指针字段，Set 写它时 hash 必然非空。
	// RR-20261004-03：被引用却不存在的这种 hash 只能是过期或被删，Get 据此报 miss；
	// 字段全是指针的层全 nil 时只被引用、不写 hash，缺失是合法状态。
	alwaysWritten bool
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
	fieldNames := make(map[string]bool)
	for i := 0; i < typ.NumField(); i++ {
		sf := typ.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		name, ok := refHMapFieldName(sf)
		if !ok {
			continue
		}
		// RR-20261004-NC-19：同hash字段别名与路径分隔符不能静默覆盖数据。
		if fieldNames[name] || strings.ContainsAny(name, ":\n\r") || (len(path) == 0 && name == refHMapRegistryField) {
			return nil, fmt.Errorf("%w: reserved or duplicate field %s.%s (%q)", ErrRefHMapUnsupported, typ.Name(), sf.Name, name)
		}
		fieldNames[name] = true
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
	for _, field := range node.fields {
		if !field.ptr {
			node.alwaysWritten = true
			break
		}
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
			if field.child.alwaysWritten && len(hashes[base+field.child.suffix]) == 0 {
				return errRefHMapPartialRecord
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
	if reflect.PointerTo(value.Type()).Implements(textMarshalerType) {
		// RR-20261004-NC-17：值根字段不可寻址也要调用已识别的codec。
		// 使用独立值副本，避免指针receiver改写调用方的scalar本身。
		ptr := reflect.New(value.Type())
		ptr.Elem().Set(value)
		raw, err := ptr.Interface().(encoding.TextMarshaler).MarshalText()
		return string(raw), true, err
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
