package index

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
)

type Index[K comparable, V comparable] struct {
	mu      sync.RWMutex
	primary map[K]V
	reverse map[V]map[K]struct{}
}

func NewIndex[K comparable, V comparable]() *Index[K, V] {
	return &Index[K, V]{
		primary: make(map[K]V),
		reverse: make(map[V]map[K]struct{}),
	}
}

func (i *Index[K, V]) Upsert(key K, value V) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ensureLocked()
	if old, ok := i.primary[key]; ok {
		if old == value {
			return
		}
		delete(i.reverse[old], key)
		if len(i.reverse[old]) == 0 {
			delete(i.reverse, old)
		}
	}
	i.primary[key] = value
	if value != value {
		// 不等于自身的值（NaN，或装着 NaN 的接口 / 结构体）作 map 键取不回来：反查桶建了也查不到、删不掉。
		// 旧实现先建桶再按同一个值取回写入，第一次插入就 panic（RR-20261005-NC-144）。这种值只进主表，
		// Get / Delete / Len 照常；Query 按 Go map 语义本来就命中不了它。
		return
	}
	keys := i.reverse[value]
	if keys == nil {
		keys = make(map[K]struct{})
		i.reverse[value] = keys
	}
	keys[key] = struct{}{}
}

func (i *Index[K, V]) Delete(key K) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.primary == nil {
		return
	}
	value, ok := i.primary[key]
	if !ok {
		return
	}
	delete(i.primary, key)
	delete(i.reverse[value], key)
	if len(i.reverse[value]) == 0 {
		delete(i.reverse, value)
	}
}

func (i *Index[K, V]) Get(key K) (V, bool) {
	var zero V
	if i == nil {
		return zero, false
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	value, ok := i.primary[key]
	return value, ok
}

func (i *Index[K, V]) Query(value V) []K {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	keys := i.reverse[value]
	if len(keys) == 0 {
		return nil
	}
	out := make([]K, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	return out
}

func (i *Index[K, V]) Len() int {
	if i == nil {
		return 0
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.primary)
}

func (i *Index[K, V]) Clear() {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.primary = make(map[K]V)
	i.reverse = make(map[V]map[K]struct{})
}

func (i *Index[K, V]) ensureLocked() {
	if i.primary == nil {
		i.primary = make(map[K]V)
	}
	if i.reverse == nil {
		i.reverse = make(map[V]map[K]struct{})
	}
}

// OrderedIndex 是 Query 结果按键排序的 Index。与 Index 一样，零值可直接使用（默认排序），
// nil 指针是空索引。
//
// 数据按值内嵌：旧实现存 *Index、只有 NewOrderedIndex 会填，零值的写入转发给 nil *Index 被静默丢掉，
// nil *OrderedIndex 的写入则解引用 panic（RR-20261005-NC-146）。
type OrderedIndex[K comparable, V comparable] struct {
	index Index[K, V]
	less  func(K, K) bool
}

func NewOrderedIndex[K comparable, V comparable](less func(K, K) bool) *OrderedIndex[K, V] {
	return &OrderedIndex[K, V]{less: less}
}

// inner 是内嵌的 Index；接收者为 nil 时返回 nil，Index 的方法对 nil 是空操作。
func (i *OrderedIndex[K, V]) inner() *Index[K, V] {
	if i == nil {
		return nil
	}
	return &i.index
}

func (i *OrderedIndex[K, V]) Upsert(key K, value V) { i.inner().Upsert(key, value) }
func (i *OrderedIndex[K, V]) Delete(key K)          { i.inner().Delete(key) }
func (i *OrderedIndex[K, V]) Get(key K) (V, bool)   { return i.inner().Get(key) }
func (i *OrderedIndex[K, V]) Len() int              { return i.inner().Len() }
func (i *OrderedIndex[K, V]) Clear()                { i.inner().Clear() }

func (i *OrderedIndex[K, V]) Query(value V) []K {
	if i == nil {
		return nil
	}
	out := i.index.Query(value)
	less := i.less
	if less == nil {
		less = defaultLess[K]
	}
	sort.Slice(out, func(a, b int) bool { return less(out[a], out[b]) })
	return out
}

// defaultLess orders numeric and string keys by their natural order; only
// other comparable kinds fall back to the formatted representation (the old
// blanket fmt.Sprint comparison sorted integers lexically: [9, 10] -> [10, 9]).
//
// K 为接口类型（如 any）时两边的动态类型可以不同、也可以是 nil：先按动态类型排（nil 最前，其余按类型名，
// 同名再按格式化串），同类型才比较值。旧实现只看左边的 Kind 就对右边调用 Int()/Uint()/Float()，
// 右边是 string、uint 或 nil 时 panic（RR-20261005-NC-145）。
func defaultLess[K comparable](a, b K) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() {
		return !va.IsValid() && vb.IsValid()
	}
	if ta, tb := va.Type(), vb.Type(); ta != tb {
		if na, nb := ta.String(), tb.String(); na != nb {
			return na < nb
		}
		return fmt.Sprint(a) < fmt.Sprint(b)
	}
	switch va.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return va.Int() < vb.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return va.Uint() < vb.Uint()
	case reflect.Float32, reflect.Float64:
		return va.Float() < vb.Float()
	case reflect.String:
		return va.String() < vb.String()
	default:
		return fmt.Sprint(a) < fmt.Sprint(b)
	}
}
