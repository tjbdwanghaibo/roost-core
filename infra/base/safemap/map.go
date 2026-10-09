// Package safemap provides the concurrent map implementations used by
// generated DAO collections and framework registries.
//
// 遍历回调契约（仓库级，维护者决定 C7）：Range 的回调里可以读写同一个 map（Get / Set /
// Delete / Clear / 嵌套 Range），不死锁、不交出不存在的键；回调返回 false 立即停止。遍历开始
// 时就在、期间没被删除的键恰好交出一次；回调里新增的键是否交出不承诺。SmallSafeMap /
// ShardedSafeMap 按快照遍历（期间删掉的未到达键仍可能交出），FastMap 活遍历（不交出）。
// 回归：range_contract_promises_test.go（共用辅助 internal/rangecontract）。
package safemap

type IMap[K comparable, V any] interface {
	Set(key K, value V)
	Get(key K) (V, bool)
	Delete(key K) bool
	Len() int
	Clear()
	// Range 遍历全部键值，回调里可以读写同一个 map，返回 false 立即停止（见包注释）。
	Range(func(key K, value V) bool)
}

type Entry[K comparable, V any] struct {
	Key   K
	Value V
}

type HashFunc[K comparable] func(K) uint64

type ComputeFunc[V any] func(old V, exists bool) (next V, keep bool)
