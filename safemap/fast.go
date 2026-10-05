package safemap

const (
	slotEmpty uint8 = iota
	slotFilled
	slotDeleted
)

// FastMap is a non-thread-safe open-addressing map for hot paths already
// protected by an outer lock or owned by a single worker.
type FastMap[K comparable, V any] struct {
	hash   HashFunc[K]
	keys   []K
	values []V
	states []uint8
	size   int
	used   int
}

func NewFastMap[K comparable, V any](capHint int, hash HashFunc[K]) *FastMap[K, V] {
	if hash == nil {
		panic("fmap: nil hash func")
	}
	capacity := fastMapCapacityForHint(capHint)
	return &FastMap[K, V]{
		hash:   hash,
		keys:   make([]K, capacity),
		values: make([]V, capacity),
		states: make([]uint8, capacity),
	}
}

func NewIntegerFastMap[K Integer, V any](capHint int) *FastMap[K, V] {
	return NewFastMap[K, V](capHint, HashInteger[K])
}

func NewInt64FastMap[V any](capHint int) *FastMap[int64, V] {
	return NewIntegerFastMap[int64, V](capHint)
}

func NewUint64FastMap[V any](capHint int) *FastMap[uint64, V] {
	return NewIntegerFastMap[uint64, V](capHint)
}

func NewStringFastMap[V any](capHint int) *FastMap[string, V] {
	return NewFastMap[string, V](capHint, HashString)
}

// Set 先查键：改已有键、复用墓碑都原地写，只有占用新的空槽且会超过装载率时
// 才扩容 / 重排。之前 Set 先 ensureWritable 再查键，装载率到阈值时改已有键
// 也会换掉整张表，Range 回调里的 `Set(k, v*10)` 因此让遍历读错数组
// （RR-20261005-NC-182）。used（已占用 + 墓碑）不超过装载率的不变量不变，
// 探测总能遇到空槽结束。
func (m *FastMap[K, V]) Set(key K, value V) {
	if len(m.states) > 0 {
		idx, found := m.findSlot(key)
		if found {
			m.values[idx] = value
			return
		}
		if m.hasRoomFor(idx) {
			m.insertAt(idx, key, value)
			return
		}
	}
	m.ensureWritable()
	idx, found := m.findSlot(key)
	if found {
		m.values[idx] = value
		return
	}
	m.insertAt(idx, key, value)
}

// hasRoomFor 报告往 idx 这个空闲槽插入后装载率是否仍在阈值内：复用墓碑不增加
// used，总在阈值内。
func (m *FastMap[K, V]) hasRoomFor(idx int) bool {
	if m.states[idx] != slotEmpty {
		return true
	}
	return (m.used+1)*100 <= len(m.states)*fastMapLoadPercent
}

func (m *FastMap[K, V]) insertAt(idx int, key K, value V) {
	if m.states[idx] == slotEmpty {
		m.used++
	}
	m.keys[idx] = key
	m.values[idx] = value
	m.states[idx] = slotFilled
	m.size++
}

func (m *FastMap[K, V]) Get(key K) (V, bool) {
	if len(m.states) == 0 {
		var zero V
		return zero, false
	}
	idx, found := m.findExisting(key)
	if !found {
		var zero V
		return zero, false
	}
	return m.values[idx], true
}

func (m *FastMap[K, V]) Delete(key K) bool {
	if len(m.states) == 0 {
		return false
	}
	idx, found := m.findExisting(key)
	if !found {
		return false
	}
	var zeroK K
	var zeroV V
	m.keys[idx] = zeroK
	m.values[idx] = zeroV
	m.states[idx] = slotDeleted
	m.size--
	if m.size == 0 {
		m.Clear()
	}
	return true
}

func (m *FastMap[K, V]) Len() int {
	return m.size
}

// Clear 丢弃整张表。旧数组不清零：正在进行的 Range 还持有它们，靠
// “表已被换掉”回到当前表查找（见 Range）；清零会让它读到零值键。
func (m *FastMap[K, V]) Clear() {
	m.keys = nil
	m.values = nil
	m.states = nil
	m.size = 0
	m.used = 0
}

// Range 按槽位顺序遍历，f 返回 false 即停止。回调里可以 Set / Delete / Clear
// 这个 map：每个未被删除的原有键恰好交出一次、带当前值，被删除的未到达键不
// 交出，回调里新增的键是否交出不承诺（与 Go map 相同）。
//
// 遍历固定开始时的三个数组。回调扩容 / 重排 / Clear 换掉了表之后，旧数组里
// 还没走到的键改到当前表里查，查不到（已删除）就跳过——之前 Range 拿旧 states
// 的下标去读新数组，交出零值键、漏掉原有键，Clear 后下标越界
// （RR-20261005-NC-182）。
func (m *FastMap[K, V]) Range(f func(key K, value V) bool) {
	if f == nil {
		return
	}
	keys, values, states := m.keys, m.values, m.states
	for i, state := range states {
		if state != slotFilled {
			continue
		}
		if m.replaced(states) {
			// 旧数组不再被写入，states[i] 仍是“开始时就在、还没走到”的键。
			value, ok := m.Get(keys[i])
			if !ok {
				continue
			}
			if !f(keys[i], value) {
				return
			}
			continue
		}
		if !f(keys[i], values[i]) {
			return
		}
	}
}

// replaced 报告 states 是否已不是当前表（被 rehash 或 Clear 换掉）。
func (m *FastMap[K, V]) replaced(states []uint8) bool {
	return len(m.states) != len(states) || &m.states[0] != &states[0]
}

func (m *FastMap[K, V]) Cap() int {
	return len(m.states)
}

func (m *FastMap[K, V]) ensureWritable() {
	if len(m.states) == 0 {
		m.rehash(minFastMapCapacity)
		return
	}
	if (m.used+1)*100 <= len(m.states)*fastMapLoadPercent {
		return
	}
	newCapacity := len(m.states) * fastMapRehashFactor
	if m.size*100 < len(m.states)*(fastMapLoadPercent/2) {
		newCapacity = len(m.states)
	}
	m.rehash(newCapacity)
}

func (m *FastMap[K, V]) rehash(capacity int) {
	if capacity < minFastMapCapacity {
		capacity = minFastMapCapacity
	}
	capacity = nextPowerOfTwo(capacity)
	oldKeys := m.keys
	oldValues := m.values
	oldStates := m.states

	m.keys = make([]K, capacity)
	m.values = make([]V, capacity)
	m.states = make([]uint8, capacity)
	m.size = 0
	m.used = 0

	for i, state := range oldStates {
		if state == slotFilled {
			m.setNoGrow(oldKeys[i], oldValues[i])
		}
	}
}

func (m *FastMap[K, V]) setNoGrow(key K, value V) {
	idx, found := m.findSlot(key)
	if found {
		m.values[idx] = value
		return
	}
	if m.states[idx] == slotEmpty {
		m.used++
	}
	m.keys[idx] = key
	m.values[idx] = value
	m.states[idx] = slotFilled
	m.size++
}

func (m *FastMap[K, V]) findExisting(key K) (int, bool) {
	mask := uint64(len(m.states) - 1)
	idx := m.hash(key) & mask
	for {
		switch m.states[idx] {
		case slotEmpty:
			return 0, false
		case slotFilled:
			if m.keys[idx] == key {
				return int(idx), true
			}
		}
		idx = (idx + 1) & mask
	}
}

func (m *FastMap[K, V]) findSlot(key K) (int, bool) {
	mask := uint64(len(m.states) - 1)
	idx := m.hash(key) & mask
	firstDeleted := -1
	for {
		switch m.states[idx] {
		case slotEmpty:
			if firstDeleted >= 0 {
				return firstDeleted, false
			}
			return int(idx), false
		case slotDeleted:
			if firstDeleted < 0 {
				firstDeleted = int(idx)
			}
		case slotFilled:
			if m.keys[idx] == key {
				return int(idx), true
			}
		}
		idx = (idx + 1) & mask
	}
}

var _ IMap[int64, int64] = (*FastMap[int64, int64])(nil)
