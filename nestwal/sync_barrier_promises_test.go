package nestwal

import (
	"context"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// U-0185 · C8 · RR-20260912-02：WAL.Sync 返回 nil 时，调用 Sync 之前 Enqueue 已经
// 成功返回的 ticket 必须已经落盘（DurableLSN 覆盖其 LSN）。旧实现只 fsync 当前 active
// 文件，不向 writer 排屏障：writer 还在 collectBatch 等 BatchDelay 时，记录仍在内存
// 队列里，Sync 成功返回而 ticket 未完成——依赖 Sync(nil) 推进自身状态的调用者会提前
// 确认。用 BatchDelay=time.Hour 把收集窗口拉大来暴露这个窗口。

func TestWALSyncPromiseCoversAdmittedTickets(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.BatchDelay = time.Hour
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	ticket, err := w.Enqueue(context.Background(), testRecord(1, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := w.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatalf("ticket failed: %v", err)
		}
	default:
		t.Fatalf("Sync returned nil before admitted ticket resolved: admitted=%d appended=%d durable_lsn=%d ticket_lsn=%d",
			w.Stats().Admitted, w.Stats().Appended, w.DurableLSN(), ticket.LSN())
	}
	if got := w.DurableLSN(); got < ticket.LSN() {
		t.Fatalf("durable watermark=%d < ticket lsn=%d after Sync", got, ticket.LSN())
	}
}

// 屏障不能让 Sync 在 WAL 已关闭后无限阻塞：Close 已经排空并 fsync 了全部队列，
// 所以关闭后的 Sync 是空满足——必须立刻返回 nil，而不是卡在没人接收的 appendCh 上。
func TestWALSyncPromiseDoesNotBlockAfterClose(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Sync(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync after clean Close: got %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sync blocked after Close")
	}
}
