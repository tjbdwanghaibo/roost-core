package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// RR-20260926-50：投影器按退避重试 ReplayPass，每轮从 checkpoint 重投。被跳过的原生步骤在 ack 失败后
// 每轮都会再次读到（持久 skipped 标记），但它的驱逐只能按事务身份登记一次：重投不再排队、不重复计数，
// 驱逐成功后多余的队列项不能再去卸载刚从 Mongo 重载的新对象（REPRO-2026-09-26-05 §3）。
func TestSkippedFencedStepIsQueuedForEvictionOnce(t *testing.T) {
	projector, recorder, _, _ := fencedProjector(t, time.Now().UTC().Add(-time.Second))
	recorder.fail.Store(true)
	// 可恢复的 checkpoint 失败（不是 WAL terminal）：投影器照常重试并重投同一条记录。
	projector.OverrideAck(func(context.Context, corenest.CommitFence) error {
		return errors.New("injected checkpoint store failure")
	})
	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 5; pass++ {
		if _, err := projector.ReplayPass(context.Background()); err == nil {
			t.Fatalf("pass %d: ack unexpectedly succeeded", pass)
		}
		if pass == 0 {
			// 第一轮登记的那一项要先被驱逐 worker 取走（它随后在失败重试里退避），下面对队列长度的断言才只数
			// 重投多排的项。之前不等：worker goroutine 还没被调度时队列里正是那唯一一项，负载高或整包重复运行时
			// 偶发误报“多排了 1 次”（RR-20260926-82）。
			awaitChan(t, recorder.failed, "the eviction worker to take the first registration")
		}
	}
	projector.evictMu.Lock()
	queued := len(projector.evictQueue)
	projector.evictMu.Unlock()
	if queued != 0 {
		t.Fatalf("after 5 failed passes the same skipped step waits in the eviction queue %d more time(s), want it registered once", queued)
	}
	recorder.fail.Store(false)
	if err := projector.WaitEntityProjection(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if err := projector.waitEvictions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := projector.Stats().StaleEvictions; got != 1 {
		t.Fatalf("StaleEvictions=%d after the replays, want exactly one eviction of the skipped step (extra ones unload the reloaded object)", got)
	}
	calls := recorder.calls.Load()
	// 驱逐完成后的重投：记录已不在本进程待投影表里，不再驱逐。
	projector.OverrideAck(projector.wal.Ack)
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := projector.waitEvictions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := recorder.calls.Load(); got != calls {
		t.Fatalf("a replay after the eviction finished evicted again: calls %d -> %d", calls, got)
	}
}

// RR-20260926-50：驱逐还在重试（例如 Nest 已 fence、RunLocal 恒失败）时 WAL 进入 terminal，本进程不会再
// 确认任何记录。terminal 之前就已进入 WaitEntityProjection 的调用方必须立即拿到可判别的 WAL 错误
// （errors.Is ErrCommitIndeterminate），不能等到自己的截止时间或关闭；之后的准入在 WAL 前拒绝。
func TestTerminalAckWakesWaitersOfAPendingEviction(t *testing.T) {
	projector, recorder, _, _ := fencedProjector(t, time.Now().UTC().Add(-time.Second))
	recorder.fail.Store(true)
	projector.OverrideAck(func(context.Context, corenest.CommitFence) error {
		return errors.New("injected checkpoint store failure")
	})
	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	if _, err := projector.ReplayPass(context.Background()); err == nil {
		t.Fatal("premise: first pass acked")
	}
	awaitChan(t, recorder.failed, "the eviction to start failing")
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waiter := waitEntityInSelect(t, projector, waitCtx, 7)

	projector.OverrideAck(func(context.Context, corenest.CommitFence) error {
		return errors.Join(corenest.ErrCommitIndeterminate, errors.New("injected terminal WAL"))
	})
	if _, err := projector.ReplayPass(context.Background()); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("pass on a terminal WAL=%v, want ErrCommitIndeterminate", err)
	}
	select {
	case err := <-waiter:
		if !errors.Is(err, corenest.ErrCommitIndeterminate) {
			t.Fatalf("waiter woken by WAL terminal err=%v, want errors.Is ErrCommitIndeterminate", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter that entered WaitEntityProjection before the WAL went terminal is still blocked on the pending eviction; it gets no WAL error until its own deadline or Close")
	}
	if err := projector.WaitEntityProjection(context.Background(), 7); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("wait after WAL terminal=%v, want ErrCommitIndeterminate", err)
	}
	if err := commitReleased(projector, heroWrite(10, 4)); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("admission after WAL terminal=%v, want the terminal error before the WAL", err)
	}
	if got := projector.Stats().FencedEntities; got != 0 {
		t.Fatalf("WAL terminal left %d fenced entities", got)
	}
}
