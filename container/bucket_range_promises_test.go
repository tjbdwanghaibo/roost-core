package container

import (
	"testing"
	"time"
)

// RR-20261005-NC-180：BucketHolder 的遍历回调可以改同一个容器。
//
// 承诺：RangeAll / RangeWithCursor* / RangeByCursor 的回调里删除、添加、查询
// 同一个 BucketHolder 不会卡住。旧行为：Bucket.Range 持桶读锁调用回调，回调里
// Del / Add 要同一桶的写锁、未命中的 Get 也拿写锁，当场自锁；EntityManager.Range
// 回调里 Destroy 一个实体，会在持有实体锁与 addMu 的情况下永久卡住。
func TestBucketRangeCallbackMayChangeTheHolder(t *testing.T) {
	cases := []struct {
		name string
		body func(h *BucketHolder[int64, int64], k int64)
	}{
		{"delete the visited key", func(h *BucketHolder[int64, int64], k int64) { h.Del(k) }},
		{"add a key", func(h *BucketHolder[int64, int64], k int64) { h.Add(k+1000, k) }},
		{"look up a missing key", func(h *BucketHolder[int64, int64], _ int64) { h.Get(99999) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One bucket: every key shares the lock the callback would need.
			h := NewBucketHolder[int64, int64](1, nil, false)
			for i := int64(1); i <= 4; i++ {
				h.Add(i, i)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.RangeAll(func(k, _ int64) bool {
					tc.body(h, k)
					return true
				})
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("RangeAll did not return: the callback is waiting for the bucket lock its own Range holds")
			}
		})
	}
}

// RR-20261005-NC-181：RangeAll 在回调第一次返回 false 时停止，跨桶也一样。
//
// 旧行为：false 只结束当前桶，RangeAll 接着遍历下一个桶，
// EntityManager.Range / RangeByCategory 写明的“Return false from fn to stop
// early”不成立：找到目标后回调还会被调用 bucketCnt-1 次。
func TestRangeAllStopsAtTheFirstFalse(t *testing.T) {
	h := NewBucketHolder[int64, int64](8, nil, false)
	for i := int64(1); i <= 64; i++ {
		h.Add(i, i)
	}
	calls := 0
	h.RangeAll(func(int64, int64) bool {
		calls++
		return false
	})
	if calls != 1 {
		t.Fatalf("callback ran %d times after returning false on the first entry, want 1", calls)
	}
}
