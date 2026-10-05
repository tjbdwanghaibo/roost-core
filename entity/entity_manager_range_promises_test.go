package entity

import (
	"context"
	"testing"
	"time"
)

// RR-20261005-NC-180 / NC-181 在 EntityManager 上的组合：Range 的公开承诺
// 由 container.BucketHolder 兑现，这里按调用方实际的用法验证。
//
// NC-180：Range 回调里 Destroy 实体（例如“遍历并卸载空闲玩家”）。旧行为是
// Destroy 在持有实体锁与 addMu 后去拿 Range 正持读锁的桶写锁，永久卡住，
// 之后全进程的 Add / Destroy 都卡在 addMu 上。
//
// NC-181：Range / RangeByCategory 写明返回 false 提前停止。旧行为是 false
// 只结束当前桶，回调还会在其余每个桶各被调用一次。
func TestManagerRangeCallbackMayDestroyAndStopsAtFalse(t *testing.T) {
	t.Run("destroy inside Range", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(4)
		for i := int64(1); i <= 32; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			mgr.Range(func(e IThreadSafeEntity) bool {
				if err := mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false); err != nil {
					t.Errorf("destroy %d: %v", e.ID(), err)
				}
				return true
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Range did not return: Destroy inside the callback waits for the bucket lock Range holds")
		}
		if n := mgr.Len(); n != 0 {
			t.Fatalf("Len = %d after destroying every visited entity, want 0", n)
		}
	})

	t.Run("false stops Range and RangeByCategory", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(4)
		for i := int64(1); i <= 32; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		calls := 0
		mgr.Range(func(IThreadSafeEntity) bool { calls++; return false })
		if calls != 1 {
			t.Fatalf("Range called fn %d times after it returned false, want 1", calls)
		}
		calls = 0
		mgr.RangeByCategory(testEntityCategoryPlayer, func(IThreadSafeEntity) bool { calls++; return false })
		if calls != 1 {
			t.Fatalf("RangeByCategory called fn %d times after it returned false, want 1", calls)
		}
	})
}

// NC-180 的快照语义在 EntityManager 上的后果：Range 先复制桶、锁外回调之后，
// 回调期间 / 之前被 Destroy 的实体照样交给 fn，而 Destroy 收尾的 doClear 已把
// ID / 分类 / 种类写回零值（use-after-destroy；kit/statslog 因此按分类 0 计数，
// RangeByCategory / CountByCategory 的分类读取与 doClear 数据竞争）。修复前
// 桶读锁挡住了 Del，回调期间实体不可能被清理。
//
// 承诺：Range / RangeByCategory 交给 fn 的实体在 fn 返回前不会被清理（ID 与
// 分类可读）；遍历到达前已被摘除的实体不交出。与 nest 分发同一协议：Touch 失败
// 即跳过，持有引用期间 Destroy 的清理推迟到引用归还。
func TestManagerRangeNeverHandsOutAClearedEntity(t *testing.T) {
	const n = 8
	t.Run("entities destroyed before they are reached", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(1) // one bucket: one snapshot holds every entity
		for i := int64(1); i <= n; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		var first int64
		visited := 0
		mgr.Range(func(e IThreadSafeEntity) bool {
			visited++
			if e.ID() == 0 || e.GetEntityCategory() != testEntityCategoryPlayer {
				t.Fatalf("Range handed out an entity already destroyed and cleared: id=%d category=%v", e.ID(), e.GetEntityCategory())
			}
			if first == 0 {
				first = e.ID()
				for i := int64(1); i <= n; i++ {
					if i != first {
						if err := mgr.Destroy(context.Background(), mgr.Get(i), DestroyReasonMemoryUnload, false); err != nil {
							t.Fatalf("destroy %d: %v", i, err)
						}
					}
				}
			}
			return true
		})
		if visited != 1 {
			t.Fatalf("Range visited %d entities, want 1: the other %d were destroyed before being reached", visited, n-1)
		}
	})

	t.Run("an entity destroyed concurrently while fn holds it", func(t *testing.T) {
		mgr := NewEntityManagerWithBuckets(1)
		for i := int64(1); i <= n; i++ {
			mgr.Add(newMgrTestEntity(i, testEntityCategoryPlayer))
		}
		done := false
		mgr.RangeByCategory(testEntityCategoryPlayer, func(e IThreadSafeEntity) bool {
			if done {
				return true
			}
			done = true
			id := e.ID()
			destroyed := make(chan error, 1)
			go func() { destroyed <- mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false) }()
			select {
			case err := <-destroyed:
				if err != nil {
					t.Fatalf("destroy %d: %v", id, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Destroy of an entity held by a Range callback did not return")
			}
			if mgr.Exists(id) {
				t.Fatalf("entity %d still managed after Destroy returned", id)
			}
			if e.ID() != id || e.GetEntityCategory() != testEntityCategoryPlayer || e.IsClear() {
				t.Fatalf("entity cleared while fn still holds it: id=%d (was %d) category=%v cleared=%v", e.ID(), id, e.GetEntityCategory(), e.IsClear())
			}
			return true
		})
		if got := mgr.Len(); got != n-1 {
			t.Fatalf("Len = %d, want %d", got, n-1)
		}
	})
}
