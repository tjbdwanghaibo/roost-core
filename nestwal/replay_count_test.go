package nestwal

import (
	"context"
	"sync"
	"testing"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// RR-20260926-07 复核残留：Committer 回放在批次最后一条的 consume 里返回截止哨兵，
// WAL 只对返回 nil 的 consume 计数，于是每批最后一条不计入 Stats.Replayed。
// 承诺：Replayed 与实际被回放应用的记录数一致；批次边界的 lookahead 记录不被应用两次。
func TestCommitterReplayCountsBatchBoundaryRecords(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	applied := map[corenest.TransactionID]int{}
	opts := DefaultCommitterOptions()
	opts.ReplayBatchRecords = 2
	opts.CloseWAL = true
	c, err := NewCommitter(w, MutationApplyFunc(func(_ context.Context, id corenest.TransactionID, _ corenest.EntityMutation) error {
		mu.Lock()
		applied[id]++
		mu.Unlock()
		return nil
	}), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	c.cancel()
	<-c.done           // 只由下面的外部 Flush 回放，计数不受后台循环影响。
	const records = 20 // testOptions 的 1KiB 段，20 条跨越多个段文件。
	for i := byte(1); i <= records; i++ {
		record := testRecord(i, corenest.DurabilityStrict)
		record.Effects = nil
		if _, err := w.Append(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().Replayed; got != records {
		t.Errorf("WAL Stats.Replayed=%d want %d (records applied by the committer)", got, records)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != records {
		t.Fatalf("applied transactions=%d want %d", len(applied), records)
	}
	for id, count := range applied {
		if count != 1 {
			t.Fatalf("transaction %s applied %d times", id.String(), count)
		}
	}
}
