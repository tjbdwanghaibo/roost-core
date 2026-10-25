package goroutine

import "sync"

// localMapShards 是 LocalMap 的分片数。键是 goroutine ID；常驻的 worker goroutine 数量少、ID 各不相同，
// 64 片下几乎不会两个 worker 落在同一片。
const localMapShards = 64

// LocalMap 是按 goroutine ID 登记“当前 goroutine 的某个值”的表（Guard 作用域、当前派发的 Nest 消息等），
// 每个 goroutine 只读写自己 ID 下的条目。
//
// 按 ID 分 64 片，每片一把读写锁和一个 map；零值可用。与 sync.Map 相比：Nest worker 的 goroutine ID 固定、
// 每条消息先存后删，sync.Map（HashTrieMap）的 Store / Delete 在同一内部节点上互相争用，争用时阻塞的 goroutine
// 让空闲 P 去偷活空转（D20 方案 D 实验，见 docs/performance/NEST-BATCH-HEARTBEAT-20261021.md §7）；分片后不同
// worker 落在不同片上，互不阻塞。做法与 fctx 的请求上下文表相同。
type LocalMap[T any] struct {
	shards [localMapShards]localMapShard[T]
}

type localMapShard[T any] struct {
	mu   sync.RWMutex
	data map[int64]T
	// 补齐到 64 字节，相邻两片的锁不在同一缓存行上。
	_ [64 - 24 - 8]byte
}

func (m *LocalMap[T]) shard(gid int64) *localMapShard[T] {
	return &m.shards[uint64(gid)%localMapShards]
}

// Load 返回 gid 下的值；没有登记时返回零值与 false。
func (m *LocalMap[T]) Load(gid int64) (T, bool) {
	s := m.shard(gid)
	s.mu.RLock()
	v, ok := s.data[gid]
	s.mu.RUnlock()
	return v, ok
}

// Store 把 gid 下的值设为 v。
func (m *LocalMap[T]) Store(gid int64, v T) {
	s := m.shard(gid)
	s.mu.Lock()
	if s.data == nil {
		s.data = make(map[int64]T)
	}
	s.data[gid] = v
	s.mu.Unlock()
}

// Delete 删除 gid 下的登记；只删这一个键，同片其他 goroutine 的登记不受影响。
func (m *LocalMap[T]) Delete(gid int64) {
	s := m.shard(gid)
	s.mu.Lock()
	delete(s.data, gid)
	s.mu.Unlock()
}
