package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// RR-20260926-37：本地已提交、Remote 结果未知或被拒绝时，Nest 把提交后工作（Sync Confirm、AfterCommit）交给批次。
// 批次只在拿到持久结论后调用一次：Applied/Committed → true（gate 已释放之后，经本地执行入口），Rejected → false
// （回滚、隔离、释放、仅内存卸载之后），停机排空拿不到结论时不调用。

type outcomeRecorder struct {
	mu        sync.Mutex
	calls     []bool
	slots     []int
	removed   []bool
	viaRun    []bool
	inRun     atomic.Bool
	delivered chan struct{}
}

func newOutcomeRecorder() *outcomeRecorder {
	return &outcomeRecorder{delivered: make(chan struct{}, 8)}
}

func (r *outcomeRecorder) callback(mgr *Manager, stale entity.IThreadSafeRemoteEntity) func(bool) {
	return func(committed bool) {
		r.mu.Lock()
		r.calls = append(r.calls, committed)
		r.slots = append(r.slots, len(mgr.remote.writeSlots))
		r.removed = append(r.removed, stale.IsRemoved())
		r.viaRun = append(r.viaRun, r.inRun.Load())
		r.mu.Unlock()
		r.delivered <- struct{}{}
	}
}

// bind 模拟 Nest 注入的本地执行入口：记录后台收尾是否经它执行提交后回调。
func (r *outcomeRecorder) bind(mgr *Manager) {
	mgr.BindLocalExecutor(func(fn func()) error {
		r.inRun.Store(true)
		defer r.inRun.Store(false)
		fn()
		return nil
	})
}

func (r *outcomeRecorder) awaitOne(t *testing.T) (committed bool, slots int, removed, viaRun bool) {
	t.Helper()
	select {
	case <-r.delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("durable outcome was never delivered to the deferred post-commit work")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := len(r.calls) - 1
	return r.calls[i], r.slots[i], r.removed[i], r.viaRun[i]
}

func (r *outcomeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func deferOutcome(t *testing.T, batch entity.RemoteWriteBatch, fn func(bool)) {
	t.Helper()
	deferrer, ok := batch.(interface{ DeferUntilDurableOutcome(func(bool)) bool })
	if !ok {
		t.Fatal("remote write batch does not accept post-commit work for its durable outcome")
	}
	if !deferrer.DeferUntilDurableOutcome(fn) {
		t.Fatal("batch refused post-commit work although its outcome is still pending")
	}
}

func expiredContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// strict 确认超时 → 投影器提交并发布 → finalizer 释放后调用一次 true。
func TestStrictTimeoutOutcomeCommittedRunsPostCommitOnce(t *testing.T) {
	f, live := newReloadFixture(t, 1961)
	steps := make(chan string, 16)
	f.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	recorder := newOutcomeRecorder()
	recorder.bind(f.mgr)
	tx := remoteTestTxID(0xD1)
	batch := f.prepareRejected(t, live, tx, 2)
	commits := batch.Commits()
	if _, err := batch.Commit(expiredContext()); err == nil {
		t.Fatal("strict commit with an expired wait returned no error")
	}
	deferOutcome(t, batch, recorder.callback(f.mgr, live))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-steps; got != "await_projection" {
		t.Fatalf("finalizer step=%s", got)
	}
	if recorder.count() != 0 {
		t.Fatal("post-commit work ran before any durable conclusion")
	}
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	committed, slots, _, viaRun := recorder.awaitOne(t)
	if !committed || slots != 0 || !viaRun {
		t.Fatalf("outcome committed=%v slots-at-callback=%d via-local-runtime=%v, want true after release through the local runtime", committed, slots, viaRun)
	}
	if err := f.mgr.StopFinalizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := recorder.count(); n != 1 {
		t.Fatalf("post-commit work ran %d times, want exactly once", n)
	}
}

// strict 确认超时 → 投影器持久拒绝 → finalizer 回滚、隔离、释放、卸载后调用一次 false。
func TestStrictTimeoutOutcomeRejectedDiscardsAfterUnload(t *testing.T) {
	f, live := newReloadFixture(t, 1962)
	recorder := newOutcomeRecorder()
	recorder.bind(f.mgr)
	tx := remoteTestTxID(0xD2)
	batch := f.prepareRejected(t, live, tx, 2)
	if _, err := batch.Commit(expiredContext()); err == nil {
		t.Fatal("strict commit with an expired wait returned no error")
	}
	deferOutcome(t, batch, recorder.callback(f.mgr, live))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	committed, slots, removed, viaRun := recorder.awaitOne(t)
	if committed || slots != 0 || !removed || !viaRun {
		t.Fatalf("outcome committed=%v slots=%d staleRemoved=%v via-local-runtime=%v, want false after release and unload", committed, slots, removed, viaRun)
	}
	if err := f.mgr.StopFinalizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := recorder.count(); n != 1 {
		t.Fatalf("outcome delivered %d times, want exactly once", n)
	}
}

// Durability 0 被权威明确拒绝：Commit 同步回滚，Close 释放并卸载后调用一次 false。
func TestDefinitelyRejectedMemoryOutcomeDiscardsAfterUnload(t *testing.T) {
	f, live := newReloadFixture(t, 1963)
	recorder := newOutcomeRecorder()
	f.storage.reject.Store(true)
	batch := f.prepareRejected(t, live, remoteTestTxID(0xD3), 0)
	if _, err := batch.Commit(context.Background()); !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("commit err=%v", err)
	}
	deferOutcome(t, batch, recorder.callback(f.mgr, live))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed, slots, removed, _ := recorder.awaitOne(t)
	if committed || slots != 0 || !removed {
		t.Fatalf("outcome committed=%v slots=%d staleRemoved=%v, want false after release and unload", committed, slots, removed)
	}
	if n := recorder.count(); n != 1 {
		t.Fatalf("outcome delivered %d times, want exactly once", n)
	}
}

// 停机排空时仍无持久结论：不调用（门保持冻结），资源照常交还。
func TestShutdownWithoutOutcomeKeepsPostCommitPending(t *testing.T) {
	f, live := newReloadFixture(t, 1964)
	steps := make(chan string, 16)
	f.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	recorder := newOutcomeRecorder()
	batch := f.prepareRejected(t, live, remoteTestTxID(0xD4), 2)
	if _, err := batch.Commit(expiredContext()); err == nil {
		t.Fatal("strict commit with an expired wait returned no error")
	}
	deferOutcome(t, batch, recorder.callback(f.mgr, live))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-steps; got != "await_projection" {
		t.Fatalf("finalizer step=%s", got)
	}
	if err := f.mgr.StopFinalizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-steps; got != "abandoned" {
		t.Fatalf("finalizer step on shutdown=%s, want abandoned", got)
	}
	if n := recorder.count(); n != 0 {
		t.Fatalf("post-commit work ran %d time(s) without a durable conclusion", n)
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots=%d after shutdown drain", slots)
	}
}

// deferredOutcomeNotRun 读取“快池拒绝投递、提交后工作未执行”的计数（RR-20260926-61）。
func deferredOutcomeNotRun(outcome string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "remote_entity.deferred_outcome_not_run_total" && metric.Labels["outcome"] == outcome {
			total += metric.Value
		}
	}
	return total
}

// RR-20260926-61：Nest 已停机 / 已 fence（RunLocal 拒绝投递、fn 不执行）时拿到持久结论：提交后工作（Sync Confirm、
// AfterCommit）不离开快池执行——不在 finalizer goroutine 上就地运行，计数 +1 并记日志；gate 等资源照常交还，只调用一次入口。
func TestOutcomeIsNotRunWhenFastPoolRefuses(t *testing.T) {
	f, live := newReloadFixture(t, 1965)
	steps := make(chan string, 16)
	f.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { steps <- step }
	recorder := newOutcomeRecorder()
	var refused atomic.Int32
	f.mgr.BindLocalExecutor(func(func()) error { refused.Add(1); return errors.New("nest: stopped") })
	tx := remoteTestTxID(0xD5)
	batch := f.prepareRejected(t, live, tx, 2)
	commits := batch.Commits()
	if _, err := batch.Commit(expiredContext()); err == nil {
		t.Fatal("strict commit with an expired wait returned no error")
	}
	deferOutcome(t, batch, recorder.callback(f.mgr, live))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := <-steps; got != "await_projection" {
		t.Fatalf("finalizer step=%s", got)
	}
	before := deferredOutcomeNotRun("committed")
	if _, err := f.store.CommitRemoteBatch(context.Background(), commits); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mgr.ApplyRemoteCommits(context.Background(), tx, commits); err != nil {
		t.Fatal(err)
	}
	for step := <-steps; step != "released"; step = <-steps {
	}
	if n := recorder.count(); n != 0 {
		t.Fatalf("post-commit work ran %d time(s) outside the fast pool after the Nest refused it (want 0: never run business callbacks on the finalizer goroutine)", n)
	}
	if got := deferredOutcomeNotRun("committed") - before; got != 1 {
		t.Fatalf("deferred_outcome_not_run_total{outcome=committed} grew by %d, want 1", got)
	}
	if refused.Load() == 0 {
		t.Fatal("finalizer never offered the post-commit work to the local executor")
	}
	if slots := len(f.mgr.remote.writeSlots); slots != 0 {
		t.Fatalf("write slots=%d after the committed outcome", slots)
	}
	if err := f.mgr.StopFinalizer(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := recorder.count(); n != 0 {
		t.Fatalf("post-commit work ran %d time(s) after shutdown", n)
	}
}
