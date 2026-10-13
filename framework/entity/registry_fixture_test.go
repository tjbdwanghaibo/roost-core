package entity

import "testing"

// kind / category / builder 注册表是进程级的，而 `go test -count=N` 在同一进程里把整包跑 N 轮，init() 与各用例的 sync.Once 只在
// 第一轮注册，所以用例不能清空整张注册表。
//
// isolateEntityRegistry 是这些用例共用的夹具：进入时记下整张注册表（每个 kind 的不可变记录指针、派生锁档、声明的 category 集合），
// t.Cleanup 时原样放回。用例里新注册的 kind 随之撤销，别人注册的 kind 不受影响，所以用例既能每轮从“未注册”开始，也不会删掉
// init / sync.Once 留给其他用例的登记。
//
// 只在测试里用，不是导出 API；只能用于非并行用例（包内没有 t.Parallel），恢复期间持有与注册相同的锁，
// 顺序同 RegisterEntityCategories：categoryMu → registryMu。
// 历史：RR-20260926-83。
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
// delta 注册表没有撤销入口，重复注册同一 schema 被拒绝；与 isolateEntityRegistry 一样只在测试里用。
func forgetRemoteSnapshotDeltaOnCleanup(t *testing.T, schema uint32) {
	t.Helper()
	t.Cleanup(func() {
		remoteSnapshotDecoders.Lock()
		defer remoteSnapshotDecoders.Unlock()
		delete(remoteSnapshotDecoders.deltas, schema)
	})
}
