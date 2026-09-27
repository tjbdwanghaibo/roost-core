package entity

import "testing"

// RR-20260926-83：kind / category / builder 注册表是进程级的，而 `go test -count=N` 在同一进程里把整包跑 N 轮，
// init() 与各用例的 sync.Once 只在第一轮注册。修前 remote_view_test 用 `t.Cleanup(ResetEntityRegistryForTest)`
// 清空每个槽，category 用例用 resetEntityCategoriesForTest 把所有 kind 的锁档清零：第一轮之后 init 注册的
// kind 3 / 252 / 253 与 sync.Once 注册的 211～214 都没了，第二轮 9 个用例失败；同一次清空还掩盖了用例之间
// 的撞号（kind 231）和“假定 kind 从未注册”的用例在第二轮的失败。
//
// isolateEntityRegistry 是这些用例共用的夹具：进入时记下整张注册表（每个 kind 的不可变记录指针、派生锁档、
// 声明的 category 集合），t.Cleanup 时原样放回。用例里新注册的 kind 随之撤销，别人注册的 kind 不受影响，
// 所以用例既能每轮从“未注册”开始，也不会删掉 init / sync.Once 留给其他用例的登记。
//
// 只在测试里用，不是导出 API；只能用于非并行用例（包内没有 t.Parallel），恢复期间持有与注册相同的锁，
// 顺序同 RegisterEntityCategories：categoryMu → registryMu。
func isolateEntityRegistry(t *testing.T) {
	t.Helper()
	type saved struct {
		entries  [len(kindEntries)]*entityKindEntry
		ranks    [len(lockRankByKind)]uint32
		declared *categoryTaxonomy
	}
	var s saved
	categoryMu.Lock()
	registryMu.Lock()
	for i := range kindEntries {
		s.entries[i] = kindEntries[i].Load()
		s.ranks[i] = lockRankByKind[i].Load()
	}
	s.declared = taxonomy.Load()
	registryMu.Unlock()
	categoryMu.Unlock()

	t.Cleanup(func() {
		categoryMu.Lock()
		defer categoryMu.Unlock()
		registryMu.Lock()
		defer registryMu.Unlock()
		for i := range kindEntries {
			kindEntries[i].Store(s.entries[i])
			lockRankByKind[i].Store(s.ranks[i])
		}
		taxonomy.Store(s.declared)
	})
}

// forgetRemoteSnapshotDeltaOnCleanup 在用例结束时撤销 RegisterRemoteSnapshotDelta 对 schema 的登记。
// delta 注册表没有撤销入口，重复注册同一 schema 被拒绝；与 isolateEntityRegistry 一样只在测试里用
// （RR-20260926-83）。
func forgetRemoteSnapshotDeltaOnCleanup(t *testing.T, schema uint32) {
	t.Helper()
	t.Cleanup(func() {
		remoteSnapshotDecoders.Lock()
		defer remoteSnapshotDecoders.Unlock()
		delete(remoteSnapshotDecoders.deltas, schema)
	})
}
