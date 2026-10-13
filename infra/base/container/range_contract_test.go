package container

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/internal/rangecontract"
)

// C7（维护者 2026-10-05）：遍历回调契约——回调里可以读写同一个容器，返回 false 立即停止。
// 共用辅助 internal/rangecontract 对每个遍历入口跑同一组用例（读 / 写 / 删 / 清空 / 停止）。
// BucketHolder、KeyMap 是快照遍历：回调期间删掉的未到达条目仍可能交出（Live=false）。

func bucketHolderOps(buckets int, rangeOf func(h *BucketHolder[int64, int64], f func(int64, int64) bool)) func(testing.TB) rangecontract.Ops {
	return func(testing.TB) rangecontract.Ops {
		h := NewBucketHolder[int64, int64](buckets, nil, false)
		return rangecontract.Ops{
			Set: h.Add,
			Get: func(k int64) (int64, bool) {
				v := h.Get(k)
				return v, v != 0
			},
			Delete: h.Del,
			Clear: func() {
				for _, b := range h.Buckets {
					b.Clear()
				}
			},
			Range: func(f func(int64, int64) bool) { rangeOf(h, f) },
		}
	}
}

func TestContainerRangeContract(t *testing.T) {
	rangeAll := func(h *BucketHolder[int64, int64], f func(int64, int64) bool) { h.RangeAll(f) }
	cursorSweep := func(h *BucketHolder[int64, int64], f func(int64, int64) bool) {
		h.RangeWithCursorCnt(uint(h.BucketCnt), f)
	}
	subjects := map[string]rangecontract.Subject{
		"BucketHolder.RangeAll one bucket":    {New: bucketHolderOps(1, rangeAll)},
		"BucketHolder.RangeAll eight buckets": {New: bucketHolderOps(8, rangeAll)},
		"BucketHolder.RangeWithCursorCnt":     {New: bucketHolderOps(8, cursorSweep)},
		"BucketHolder.RangeByCursor":          {New: bucketHolderOps(1, func(h *BucketHolder[int64, int64], f func(int64, int64) bool) { h.RangeByCursor(0, f) })},
		"BucketHolder.RangeWithCursor":        {New: bucketHolderOps(1, func(h *BucketHolder[int64, int64], f func(int64, int64) bool) { h.RangeWithCursor(f) })},
		"Bucket.Range": {New: func(testing.TB) rangecontract.Ops {
			b := NewBucket[int64, int64](nil, false)
			return rangecontract.Ops{
				Set: b.Add,
				Get: func(k int64) (int64, bool) {
					v := b.Get(k)
					return v, v != 0
				},
				Delete: b.Del,
				Clear:  b.Clear,
				Range:  b.Range,
			}
		}},
		"KeyMap.Range": {New: func(testing.TB) rangecontract.Ops {
			m := NewKeyMap[int64, int64](16)
			return rangecontract.Ops{Set: m.Set, Get: m.Get, Delete: m.Remove, Clear: m.Clear, Range: m.Range}
		}},
	}
	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) { rangecontract.Check(t, subject) })
	}
}
