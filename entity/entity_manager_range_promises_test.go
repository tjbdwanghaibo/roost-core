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
