package container

// Key constraint for KeyMap keys.
type Key interface {
	int32 | int64 | uint64 | uint32
}

type kmEntry[K Key, V any] struct {
	key   K
	value V
}

// KeyMap is a high-performance array-based hash map for integer keys.
type KeyMap[K Key, V any] struct {
	capacity uint64
	buckets  [][]kmEntry[K, V]
	size     int
}

func NewKeyMap[K Key, V any](capacity uint64) *KeyMap[K, V] {
	if capacity == 0 {
		capacity = 1000
	}
	return &KeyMap[K, V]{
		capacity: capacity,
		buckets:  make([][]kmEntry[K, V], capacity),
		size:     0,
	}
}

func (m *KeyMap[K, V]) hashKey(key K) uint64 {
	x := uint64(key)
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	x = x ^ (x >> 31)
	return x
}

func (m *KeyMap[K, V]) getBucketIndex(key K) uint64 {
	return m.hashKey(key) % m.capacity
}

func (m *KeyMap[K, V]) Set(key K, value V) {
	index := m.getBucketIndex(key)
	for i, e := range m.buckets[index] {
		if e.key == key {
			m.buckets[index][i].value = value
			return
		}
	}
	m.buckets[index] = append(m.buckets[index], kmEntry[K, V]{key: key, value: value})
	m.size++
}

func (m *KeyMap[K, V]) Get(key K) (V, bool) {
	index := m.getBucketIndex(key)
	for _, e := range m.buckets[index] {
		if e.key == key {
			return e.value, true
		}
	}
	var zeroV V
	return zeroV, false
}

func (m *KeyMap[K, V]) Remove(key K) {
	index := m.getBucketIndex(key)
	bucket := m.buckets[index]
	n := len(bucket)
	for i := 0; i < n; i++ {
		if bucket[i].key == key {
			bucket[i] = bucket[n-1]
			var zero kmEntry[K, V]
			bucket[n-1] = zero
			m.buckets[index] = bucket[:n-1]
			m.size--
			return
		}
	}
}

func (m *KeyMap[K, V]) Len() int {
	return m.size
}

func (m *KeyMap[K, V]) Clear() {
	m.buckets = make([][]kmEntry[K, V], m.capacity)
	m.size = 0
}

// Range 逐桶遍历，每个桶先复制再调用 f；f 返回 false 即停止。
//
// 回调里可以 Set / Remove（RR-20261005-NC-185）：Remove 把桶末尾的元素换到
// 被删位置并清零末尾，之前直接遍历活切片时，换过来的键被跳过、清零的末尾
// 以零值键交给 f。复制之后回调看到的是该桶开始遍历时的内容，与
// SmallSafeMap / ShardedSafeMap 的快照语义一致。
func (m *KeyMap[K, V]) Range(f func(key K, value V) bool) {
	var entries []kmEntry[K, V]
	// 每个桶从 m.buckets 现读：回调里 Clear 换掉了整张表时，后面的桶已是空的。
	for i := 0; i < len(m.buckets); i++ {
		bucket := m.buckets[i]
		if len(bucket) == 0 {
			continue
		}
		entries = append(entries[:0], bucket...)
		for _, e := range entries {
			if !f(e.key, e.value) {
				return
			}
		}
	}
}
