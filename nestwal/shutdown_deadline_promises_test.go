package nestwal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// U-0186 · C1 · RR-20260912-01：Committer.Shutdown/Flush 等待 replay 所有权时必须遵守
// 调用者的截止时间。旧实现用 sync.Mutex（flushMu / replayMu）：后台 replayPass 正在
// ApplyMutations 时持有 replayMu，Flush 进入 replayPass 后阻塞在无 context 的 Lock 上；
// 后台 replay 用的是 committer 自身 context，只有其后的 Close 才会 cancel，于是
// Shutdown(ctx) 的截止时间对这段等锁完全无效——慢后端能让停机调用无界超预算。

func TestCommitterShutdownPromiseHonoursDeadlineWhileReplayBusy(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	applier := MutationApplyFunc(func(ctx context.Context, _ corenest.TransactionID, _ corenest.EntityMutation) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		// 投影器等待下游，但正确监听传进来的 context——这是后台 committer 的
		// context，Shutdown 的截止时间不会传到这里。
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error { return nil })
	opts := DefaultCommitterOptions()
	opts.RetryMin = time.Millisecond
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		committer.Close(context.Background())
	}()

	record := testRecord(21, corenest.DurabilityPipelined)
	ticket, err := committer.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTicket(t, ticket); err != nil {
		t.Fatal(err)
	}
	committer.TransactionReleased(record.ID)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background replay did not reach the applier")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- committer.Shutdown(ctx) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown after deadline: got %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Shutdown stayed blocked behind background replay after its context expired")
	}
}

// 同一根因的另一条路径：两个并发 Flush，第二个在 flushMu 上等待也必须可取消。
func TestCommitterFlushPromiseHonoursDeadlineBehindAnotherFlush(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	applier := MutationApplyFunc(func(ctx context.Context, _ corenest.TransactionID, _ corenest.EntityMutation) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	publisher := EffectPublishFunc(func(context.Context, corenest.TransactionID, corenest.Effect) error { return nil })
	opts := DefaultCommitterOptions()
	opts.RetryMin = time.Hour // 让后台 run 循环在第一次空跑后退避很久，Flush 自己进入 applier
	opts.RetryMax = time.Hour
	committer, err := NewCommitter(w, applier, publisher, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeRelease()
		committer.Close(context.Background())
	}()

	record := testRecord(22, corenest.DurabilityPipelined)
	ticket, err := committer.Enqueue(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitTicket(t, ticket); err != nil {
		t.Fatal(err)
	}
	committer.TransactionReleased(record.ID)

	first := make(chan error, 1)
	go func() { first <- committer.Flush(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no replay reached the applier")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := make(chan error, 1)
	go func() { second <- committer.Flush(ctx) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second Flush after deadline: got %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("second Flush stayed blocked behind the first after its context expired")
	}
	closeRelease()
	if err := <-first; err != nil {
		t.Fatalf("first Flush: %v", err)
	}
}
