package entity

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/internal/rangecontract"
)

// C7（维护者 2026-10-05）：遍历回调契约——回调里可以读写同一个容器，返回 false 立即停止。
// 实体管理器的“读写”是 Get / Add / Destroy；键是实体 ID，值由 ID 决定（KeysOnly）。交出的实体
// ID 为 0（已被 doClear 清零）会被辅助判为“交出不存在的键”——这正是 NC-180 复审补修的承诺。
// 没有 Clear，清空用例跳过。

const contractGroupID = 7001

func entityManagerOps(grouped bool, rangeOf func(mgr *EntityManager, access *ManagerAccess, fn func(IThreadSafeEntity) bool)) func(testing.TB) rangecontract.Ops {
	return func(t testing.TB) rangecontract.Ops {
		mgr := NewEntityManagerWithBuckets(4)
		access := NewManagerAccess(mgr)
		return rangecontract.Ops{
			Set: func(id, _ int64) {
				if mgr.Exists(id) {
					return
				}
				e := newMgrTestEntity(id, testEntityCategoryPlayer)
				if grouped {
					e.Base().SetGroupLockIDForTest(contractGroupID)
				}
				mgr.Add(e)
			},
			Get: func(id int64) (int64, bool) {
				if e := mgr.Get(id); e != nil {
					return e.ID(), true
				}
				return 0, false
			},
			Delete: func(id int64) {
				if e := mgr.Get(id); e != nil {
					if err := mgr.Destroy(context.Background(), e, DestroyReasonMemoryUnload, false); err != nil {
						t.Errorf("destroy %d: %v", id, err)
					}
				}
			},
			Range: func(f func(int64, int64) bool) {
				rangeOf(mgr, access, func(e IThreadSafeEntity) bool { return f(e.ID(), e.ID()) })
			},
		}
	}
}

func TestEntityRangeContract(t *testing.T) {
	subjects := map[string]rangecontract.Subject{
		"EntityManager.Range": {KeysOnly: true, New: entityManagerOps(false, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.Range(fn)
		})},
		"EntityManager.RangeByCategory": {KeysOnly: true, New: entityManagerOps(false, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.RangeByCategory(testEntityCategoryPlayer, fn)
		})},
		"EntityManager.RangeGroupEntities": {KeysOnly: true, New: entityManagerOps(true, func(mgr *EntityManager, _ *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			mgr.RangeGroupEntities(contractGroupID, fn)
		})},
		"ManagerAccess.Range": {KeysOnly: true, New: entityManagerOps(false, func(_ *EntityManager, access *ManagerAccess, fn func(IThreadSafeEntity) bool) {
			access.Range(fn)
		})},
	}
	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) { rangecontract.Check(t, subject) })
	}
}
