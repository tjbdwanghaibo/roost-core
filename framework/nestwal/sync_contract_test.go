package nestwal

import (
	"context"
	"errors"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"testing"
	"time"
)

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
