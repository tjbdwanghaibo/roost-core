package nestwal

import (
	"context"
	"errors"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// U-0190 · C8 · RR-20260914-01:Close 发起(closed=true)不等于排空完成(doneCh 关闭)。
// U-0185 的屏障在 closed 分支直接返回 nil——把"拒绝新写"当成了"已全部落盘":writer 收完批、
// 尚未 processBatch 时被调度暂停,另一 goroutine 发起 Close,此刻 Sync 返回 nil 而 ticket 未完成、
// DurableLSN=0。承诺:关闭进行中的 Sync 必须按 ctx 等待 writer 排空(doneCh),再传播关闭 / 终止
// 错误;完全关闭后的 Sync 保持既有的幂等 nil。

func TestWALSyncPromiseWaitsForDrainWhileClosing(t *testing.T) {
	for _, closing := range []bool{false, true} {
		name := "open_control"
		if closing {
			name = "closing"
		}
		t.Run(name, func(t *testing.T) {
			entered, resume := make(chan struct{}), make(chan struct{})
			first := true
			opts := testOptions(t.TempDir())
			opts.BatchMaxRecords = 1
			opts.BatchDelay = time.Hour
			opts.beforeProcessBatch = func() {
				if first {
					first = false
					close(entered)
					<-resume
				}
			}
			w, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			closed := make(chan error, 1)
			defer func() {
				close(resume)
				if closing {
					<-closed
				} else {
					_ = w.Close(context.Background())
				}
			}()
			ticket, err := w.Enqueue(context.Background(), testRecord(91, corenest.DurabilityPipelined))
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("writer not captured")
			}
			if closing {
				go func() { closed <- w.Close(context.Background()) }()
				select {
				case <-w.closeCh:
				case <-time.After(time.Second):
					t.Fatal("Close not begun")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			err = w.Sync(ctx)
			select {
			case <-ticket.Done():
				t.Fatal("premise violated: ticket done while writer held")
			default:
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Sync while writer held: closing=%v err=%v admitted=%d appended=%d durable=%d ticket=%d",
					closing, err, w.Stats().Admitted, w.Stats().Appended, w.DurableLSN(), ticket.LSN())
			}
		})
	}
}

// 关闭完成后 Sync 仍是幂等的 nil,且 Close 已让 ticket 完成。
func TestWALSyncPromiseAfterCompletedClose(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := w.Enqueue(context.Background(), testRecord(92, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticket.Done():
		if err := ticket.Err(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("completed close left ticket pending")
	}
}
