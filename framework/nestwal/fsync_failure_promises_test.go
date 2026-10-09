package nestwal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// RR-20260926-33：段文件 fsync 失败是粘滞错误。Linux ≥4.13 同一 fd 的写回错误只报告一次，
// 下一次 fsync 返回 0，但失败那批页可能从未落盘。
//
// 承诺：fsync 一旦失败，WAL 进入 terminal；此后 Ack 拒绝且不写 checkpoint，ticker / Sync 不再
// fsync、不清 unsynced、不推进 DurableLSN；排空关闭最后一次 sync 失败时在途 Ack 返回错误；
// 这些错误（含 Ack 自身首次 fsync 失败传给票据的错误）都 errors.Is(ErrCommitIndeterminate)。
// 干净关闭后在途 Ack（active==nil 且无未刷写入）仍返回 nil；RR-16 的“先刷段再存 checkpoint”不变。
//
// 旧行为（v1.17.0）：Ack 不查 terminal，只看本次 fsync 返回值，第二次“成功”即把 checkpoint
// 推过已判不确定的记录，模拟丢写回后重开报 “acknowledgement is beyond recovered WAL end”；
// 关闭时 sync 失败后 active 置 nil，在途 Ack 得到 nil 并存 checkpoint；Ack 首次 fsync 失败
// 的 terminal 未包 ErrCommitIndeterminate；terminal 后 ticker 仍 fsync 并推进 DurableLSN。

// fsyncScript 通过 Options.syncFile 测试缝按调用序号注入段文件 fsync 结果：
// fail 返回真时回 EIO，否则做真实 fsync。onCall 在返回前调用，供测试观察时序。
type fsyncScript struct {
	calls  atomic.Int32
	fail   func(call int32) bool
	onCall func(call int32, failed bool)
}

func (s *fsyncScript) sync(file *os.File) error {
	call := s.calls.Add(1)
	failed := s.fail != nil && s.fail(call)
	var err error
	if failed {
		err = &os.PathError{Op: "sync", Path: file.Name(), Err: syscall.EIO}
	} else {
		err = file.Sync()
	}
	if s.onCall != nil {
		s.onCall(call, failed)
	}
	return err
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// 写线程 fsync 失败一次、之后“成功”：Ack 必须因 terminal 拒绝，checkpoint 不前进，也不再 fsync；
// 模拟失败页从未落盘后，重开不能因超前 checkpoint 拒绝启动。
func TestAckRefusesTerminalWALAndKeepsCheckpoint(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.GroupCommitInterval = time.Hour
	script := &fsyncScript{fail: func(call int32) bool { return call == 1 }}
	opts.syncFile = script.sync
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityAsync))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(context.Background(), testRecord(2, corenest.DurabilityStrict)); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("premise: strict append after injected EIO err=%v, want ErrCommitIndeterminate", err)
	}
	w.stateMu.RLock()
	end := corenest.CommitFence{Segment: w.segment, Offset: w.offset}
	w.stateMu.RUnlock()
	ackErr := w.Ack(context.Background(), end)
	stored, _ := loadCheckpoint(dir)
	calls := script.calls.Load()
	_ = w.Close(context.Background())
	if !errors.Is(ackErr, corenest.ErrCommitIndeterminate) || stored.fence != (corenest.CommitFence{}) || calls != 1 {
		t.Errorf("Ack(fence=%d/%d) on terminal WAL: err=%v; stored checkpoint=%d/%d; fsync calls=%d; want ErrCommitIndeterminate, no checkpoint, 1 fsync",
			end.Segment, end.Offset, ackErr, stored.fence.Segment, stored.fence.Offset, calls)
	}

	// 失败那次 fsync 覆盖的页没有落盘：段回到长度 0。
	if err := os.Truncate(filepath.Join(dir, segmentName(first.Segment)), 0); err != nil {
		t.Fatal(err)
	}
	opts.syncFile = nil
	reopened, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen after lost writeback: err=%v", err)
	}
	_ = reopened.Close(context.Background())
}

// holdAckAcrossClose 让一次 Ack 在进入 operations 后停住，发起 Close 并等到排空的最后一次段
// fsync 已返回，再放行 Ack。failFinalSync 决定那次 fsync 是否失败。
func holdAckAcrossClose(t *testing.T, failFinalSync bool) (ackErr, closeErr error, fence corenest.CommitFence, stored checkpointState) {
	t.Helper()
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.GroupCommitInterval = time.Hour
	var closing atomic.Bool
	finalSynced := make(chan struct{})
	var finalOnce sync.Once
	script := &fsyncScript{
		fail: func(int32) bool { return failFinalSync && closing.Load() },
		onCall: func(int32, bool) {
			if closing.Load() {
				finalOnce.Do(func() { close(finalSynced) })
			}
		},
	}
	opts.syncFile = script.sync
	admitted, resume := make(chan struct{}), make(chan struct{})
	var ackCalls atomic.Int32
	opts.afterAckAdmitted = func() {
		if ackCalls.Add(1) == 1 {
			close(admitted)
			<-resume
		}
	}
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	var resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	defer release()
	fence, err = w.Append(context.Background(), testRecord(1, corenest.DurabilityAsync))
	if err != nil {
		t.Fatal(err)
	}
	ackDone := make(chan error, 1)
	go func() { ackDone <- w.Ack(context.Background(), fence) }()
	waitSignal(t, admitted, "Ack admission")
	closing.Store(true)
	closeDone := make(chan error, 1)
	go func() { closeDone <- w.Close(context.Background()) }()
	waitSignal(t, finalSynced, "final drain fsync")
	release()
	ackErr, closeErr = <-ackDone, <-closeDone
	stored, _ = loadCheckpoint(dir)
	return ackErr, closeErr, fence, stored
}

// 排空关闭最后一次 sync 失败后 active 已置 nil：在途 Ack 不能把这当成“已全部落盘”。
func TestInflightAckFailsAfterFailedFinalSync(t *testing.T) {
	ackErr, closeErr, fence, stored := holdAckAcrossClose(t, true)
	if !errors.Is(closeErr, syscall.EIO) {
		t.Fatalf("premise: Close err=%v, want injected EIO", closeErr)
	}
	if !errors.Is(ackErr, corenest.ErrCommitIndeterminate) || stored.fence != (corenest.CommitFence{}) {
		t.Errorf("in-flight Ack after failed final sync: err=%v; stored checkpoint=%d/%d (fence %d/%d); want ErrCommitIndeterminate and no checkpoint",
			ackErr, stored.fence.Segment, stored.fence.Offset, fence.Segment, fence.Offset)
	}
}

// 干净关闭：最后一次 sync 成功、active==nil 且无未刷写入，在途 Ack 仍成功并写 checkpoint。
func TestInflightAckAfterCleanCloseStillSucceeds(t *testing.T) {
	ackErr, closeErr, fence, stored := holdAckAcrossClose(t, false)
	if closeErr != nil {
		t.Fatalf("clean Close err=%v", closeErr)
	}
	want := corenest.CommitFence{Segment: fence.Segment, Offset: fence.Offset}
	if ackErr != nil || stored.fence != want {
		t.Errorf("in-flight Ack after clean close: err=%v; stored checkpoint=%d/%d; want nil and %d/%d",
			ackErr, stored.fence.Segment, stored.fence.Offset, want.Segment, want.Offset)
	}
}

// Ack 自身的 fsync 首先失败：它设置的 terminal 必须是 ErrCommitIndeterminate，
// 在途 pipelined 票据得到的也是它（nest 只对该错误 Fence）。
func TestAckFirstFsyncFailureIsIndeterminateForTickets(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.GroupCommitInterval = time.Hour
	opts.syncFile = (&fsyncScript{fail: func(call int32) bool { return call == 1 }}).sync
	var batches atomic.Int32
	held, release := make(chan struct{}), make(chan struct{})
	opts.beforeProcessBatch = func() {
		if batches.Add(1) == 2 {
			close(held)
			<-release
		}
	}
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		_ = w.Close(context.Background())
	}()
	fence, err := w.Append(context.Background(), testRecord(1, corenest.DurabilityAsync))
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := w.Enqueue(context.Background(), testRecord(2, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, held, "pipelined batch held before write")
	ackErr := w.Ack(context.Background(), fence)
	waitSignal(t, ticket.Done(), "ticket resolution after terminal")
	stored, _ := loadCheckpoint(dir)
	if !errors.Is(ackErr, corenest.ErrCommitIndeterminate) || !errors.Is(ticket.Err(), corenest.ErrCommitIndeterminate) ||
		!errors.Is(w.terminal(), corenest.ErrCommitIndeterminate) || stored.fence != (corenest.CommitFence{}) {
		t.Errorf("Ack first fsync failure: Ack err=%v; ticket err=%v Is(ErrCommitIndeterminate)=%v; terminal Is(ErrCommitIndeterminate)=%v; stored checkpoint=%d/%d",
			ackErr, ticket.Err(), errors.Is(ticket.Err(), corenest.ErrCommitIndeterminate),
			errors.Is(w.terminal(), corenest.ErrCommitIndeterminate), stored.fence.Segment, stored.fence.Offset)
	}
}

// terminal 后 ticker 调用的同步路径不得再 fsync、清 unsynced 或推进 DurableLSN。
// 先直接调用 ticker 所用的 syncActive（确定性），再给真实 ticker 若干个周期（负向断言的有界窗口）。
func TestSyncAfterTerminalNeverFsyncsOrAdvancesDurableLSN(t *testing.T) {
	opts := testOptions(t.TempDir())
	opts.GroupCommitInterval = 2 * time.Millisecond
	script := &fsyncScript{fail: func(call int32) bool { return call == 1 }}
	opts.syncFile = script.sync
	w, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ticket, err := w.Enqueue(context.Background(), testRecord(1, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ticket.Done(), "ticket resolution")
	if !errors.Is(ticket.Err(), corenest.ErrCommitIndeterminate) {
		t.Fatalf("premise: ticket err=%v, want ErrCommitIndeterminate", ticket.Err())
	}

	syncErr := w.syncActive()
	w.stateMu.RLock()
	unsynced := w.unsynced
	w.stateMu.RUnlock()
	if !errors.Is(syncErr, corenest.ErrCommitIndeterminate) || script.calls.Load() != 1 || !unsynced || w.DurableLSN() != 0 {
		t.Fatalf("syncActive after terminal: err=%v fsync calls=%d unsynced=%v DurableLSN=%d; want terminal error, 1 fsync, unsynced, DurableLSN 0",
			syncErr, script.calls.Load(), unsynced, w.DurableLSN())
	}

	for start := time.Now(); time.Since(start) < 50*opts.GroupCommitInterval; {
		if script.calls.Load() != 1 || w.DurableLSN() != 0 {
			break
		}
		time.Sleep(opts.GroupCommitInterval / 2)
	}
	if script.calls.Load() != 1 || w.DurableLSN() != 0 {
		t.Fatalf("ticker after terminal: fsync calls=%d DurableLSN=%d; want 1 and 0", script.calls.Load(), w.DurableLSN())
	}
}

// terminal 清扫时，晚于覆盖它的那次 fsync 才登记的票据已被 DurableLSN 覆盖，按已落盘解析；
// 未被覆盖的票据仍得到 ErrCommitIndeterminate。terminal 后 syncActive 的兜底清扫同样处理。
func TestTerminalSweepResolvesTicketsAlreadyCoveredByDurableLSN(t *testing.T) {
	w, err := Open(testOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close(context.Background())
	ticket, err := w.Enqueue(context.Background(), testRecord(1, corenest.DurabilityPipelined))
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ticket.Done(), "ticket resolution")
	if ticket.Err() != nil || w.DurableLSN() != ticket.LSN() {
		t.Fatalf("premise: ticket err=%v DurableLSN=%d LSN=%d", ticket.Err(), w.DurableLSN(), ticket.LSN())
	}
	covered := &walTicket{lsn: ticket.LSN(), done: make(chan struct{})}
	uncovered := &walTicket{lsn: ticket.LSN() + 1, done: make(chan struct{})}
	w.ticketMu.Lock()
	w.tickets = append(w.tickets, covered, uncovered)
	w.ticketMu.Unlock()
	w.setTerminal(indeterminate(errors.New("physical write outcome unknown")))
	waitSignal(t, covered.Done(), "covered ticket")
	waitSignal(t, uncovered.Done(), "uncovered ticket")
	if covered.Err() != nil || !errors.Is(uncovered.Err(), corenest.ErrCommitIndeterminate) {
		t.Fatalf("terminal sweep: covered err=%v uncovered err=%v", covered.Err(), uncovered.Err())
	}

	late := &walTicket{lsn: ticket.LSN() + 2, done: make(chan struct{})}
	w.ticketMu.Lock()
	w.tickets = append(w.tickets, late)
	w.ticketMu.Unlock()
	if err := w.syncActive(); !errors.Is(err, corenest.ErrCommitIndeterminate) {
		t.Fatalf("syncActive after terminal err=%v", err)
	}
	waitSignal(t, late.Done(), "late ticket after terminal")
	if !errors.Is(late.Err(), corenest.ErrCommitIndeterminate) {
		t.Fatalf("late ticket err=%v", late.Err())
	}
}
