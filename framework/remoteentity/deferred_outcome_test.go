// 延迟的提交后工作：Remote 结论未知时由 finalizer 拿到持久结论后执行一次，快池拒绝时不离池执行。

package remoteentity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 本地已提交、Remote 结果未知或被拒绝时，Nest 把提交后工作（Sync Confirm、AfterCommit）交给批次。批次只在拿到持久结论后
// 调用一次：Applied/Committed → true（gate 已释放之后，经本地执行入口），Rejected → false（回滚、隔离、释放、仅内存卸载之后），
// 停机排空拿不到结论时不调用。
// 历史：RR-20260926-37、RR-20260926-61。

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

// deferredOutcomeNotRun 读取“快池拒绝投递、提交后工作未执行”的计数。
func deferredOutcomeNotRun(outcome string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "remote_entity.deferred_outcome_not_run_total" && metric.Labels["outcome"] == outcome {
			total += metric.Value
		}
	}
	return total
}

// Nest 已停机 / 已 fence（RunLocal 拒绝投递、fn 不执行）时拿到持久结论：提交后工作不离开快池执行——不在 finalizer goroutine 上
// 就地运行，计数 +1 并记日志；gate 等资源照常交还，只调用一次入口。
func TestOutcomeIsNotRunWhenShortPoolRefuses(t *testing.T) {
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

// deferredOutcomeNestRig 是跨层夹具：真实 Nest + 正式 Remote Manager（MongoCommitter 权威、ManagerAccess loader），
// handler 修改 Remote 实体并注册 AfterCommit，记录回调执行的位置与它看到的请求上下文。
type deferredOutcomeNestRig struct {
	f      reloadFixture
	live   *reloadableFullDocEntity
	engine *nest.NestMgr
	name   nest.HandlerName
	steps  chan string

	mu                sync.Mutex
	afterCommit       int
	afterCommitOnFast int
	seenKV            any
	seenTrace         string
	seenHandler       string
	seenBaseErr       error
}

type deferredOutcomeRequestKey struct{}

func newDeferredOutcomeNestRig(t *testing.T, rawID int64, name string) *deferredOutcomeNestRig {
	t.Helper()
	f, live := newReloadFixture(t, rawID)
	rig := &deferredOutcomeNestRig{f: f, live: live, steps: make(chan string, 16)}
	f.mgr.remote.finalizeTrace = func(_ entity.RemoteTransactionID, step string) { rig.steps <- step }
	syncMgr, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(context.Context, entitysync.SessionID, []byte) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syncMgr.Close(context.Background()) })
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte("v")), nil
	}
	live.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: live.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
	if err = syncMgr.Register(live.Sync()); err != nil {
		t.Fatal(err)
	}
	rig.engine = nest.NewEngine(nest.NestOptionWithGetter(f.access), nest.NestOptionWithRemoteEntityManager(f.mgr), nest.NestOptionWithEntitySync(syncMgr), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	rig.name = nest.NewHandlerName(name)
	rig.engine.MustRegisterHandlerWithMeta(rig.name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		live.set("rejected", "")
		live.MarkSyncDirty(1)
		fctx.CurrentContext().Set(deferredOutcomeRequestKey{}, "request-"+name)
		nest.AfterCommit(func() {
			current := fctx.CurrentContext()
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.afterCommit++
			if fctx.InShortWorker() {
				rig.afterCommitOnFast++
			}
			if current != nil {
				rig.seenKV, _ = current.Get(deferredOutcomeRequestKey{})
				rig.seenTrace = current.Trace.TraceID
				rig.seenHandler = current.Meta.Handler
				if current.Base != nil {
					rig.seenBaseErr = current.Base.Err()
				}
			}
		})
		return "ok", nil
	}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
	if err = rig.engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rig.engine.Shutdown(context.Background()) })
	return rig
}

// request 以带 trace 的调用方上下文发起请求，期望得到“结果未知”（不带已提交哨兵），随后取消请求 ctx。
func (rig *deferredOutcomeNestRig) request(t *testing.T, traceID string) {
	t.Helper()
	_, release := fctx.NewContext(fctx.WithTrace(fctx.TraceMeta{TraceID: traceID, Enabled: true}))
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	reply, err := rig.engine.Request(ctx, rig.name, rig.live.ID(), nil)
	cancel()
	if !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || errors.Is(err, nest.ErrAfterCommitFailed) {
		t.Fatalf("reply=%v err=%v, want an unknown outcome without the committed sentinel", reply, err)
	}
}

// holdFinalizer 让 finalizer 的回源读取停在返回之前，直到返回的 release 被调用。
func (rig *deferredOutcomeNestRig) holdFinalizer() (awaitHeld func(*testing.T), release func()) {
	hold := make(chan struct{})
	rig.f.storage.statusHold.Store(&hold)
	return func(t *testing.T) {
			t.Helper()
			select {
			case <-rig.f.storage.statusHeld:
			case <-time.After(5 * time.Second):
				t.Fatal("finalizer never asked the authority for the outcome")
			}
		}, func() {
			rig.f.storage.statusHold.Store(nil)
			close(hold)
		}
}

func (rig *deferredOutcomeNestRig) awaitReleased(t *testing.T) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case step := <-rig.steps:
			if step == "released" {
				return
			}
		case <-deadline:
			t.Fatal("finalizer did not conclude the transaction")
		}
	}
}

func (rig *deferredOutcomeNestRig) counts() (ran, onFast int) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return rig.afterCommit, rig.afterCommitOnFast
}

// 跨层：Durability 0 的 Remote 写结果未知时，请求带“结果未知”错误回复；提交后工作经 Nest → remoteWriteBatch → finalizer 交接，
// 拿到持久结论后执行一次：回复丢失但已写入 → AfterCommit 在快池执行一次、Sync 放行；从未到达 → 持久拒绝、卸载、AfterCommit
// 不执行。延迟执行的 AfterCommit 带原请求的上下文快照（trace、handler 元数据、请求内写入的 KV），且不继承请求 ctx 的取消。
func TestNestDeferredPostCommitFollowsFinalizerOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rawID     int64
		committed bool
	}{
		{name: "lost_reply_commits", rawID: 1971, committed: true},
		{name: "never_reached_is_rejected", rawID: 1972},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newDeferredOutcomeNestRig(t, tc.rawID, "deferred_outcome_"+tc.name)
			if tc.committed {
				rig.f.storage.loseReply.Store(true)
			} else {
				rig.f.storage.unreachable.Store(true)
			}
			awaitHeld, release := rig.holdFinalizer()
			rig.request(t, "trace-"+tc.name)
			awaitHeld(t)
			release()
			rig.awaitReleased(t)
			ran, onFast := rig.counts()
			if tc.committed {
				if ran != 1 || onFast != 1 {
					t.Fatalf("AfterCommit after the durable commit ran=%d onFast=%d, want exactly once on a fast worker", ran, onFast)
				}
				rig.mu.Lock()
				kv, trace, handler, baseErr := rig.seenKV, rig.seenTrace, rig.seenHandler, rig.seenBaseErr
				rig.mu.Unlock()
				if kv != "request-deferred_outcome_"+tc.name || trace != "trace-"+tc.name || handler != rig.name.String() {
					t.Fatalf("deferred AfterCommit ran without the request context: kv=%v trace=%q handler=%q, want kv=%q trace=%q handler=%q",
						kv, trace, handler, "request-deferred_outcome_"+tc.name, "trace-"+tc.name, rig.name.String())
				}
				if baseErr != nil {
					t.Fatalf("deferred AfterCommit inherited the finished request's cancellation: %v", baseErr)
				}
				if !rig.live.Sync().SyncCommitReady() {
					t.Fatal("Sync still frozen after the durable commit")
				}
				if doc := rig.f.storedDocument(t); doc != "a=rejected;b=" {
					t.Fatalf("Mongo document=%q, want the committed write", doc)
				}
				return
			}
			if ran != 0 {
				t.Fatalf("AfterCommit ran %d time(s) for a rejected transaction", ran)
			}
			if !rig.live.IsRemoved() {
				t.Fatal("rejected instance was not unloaded")
			}
			if doc := rig.f.storedDocument(t); doc != "" {
				t.Fatalf("rejected transaction reached Mongo: %q", doc)
			}
		})
	}
}

// 持久结论到达时 Nest 已停机或已 fence（NestMgr.RunLocal 拒绝投递）：提交后工作不在 finalizer goroutine 上就地执行——
// AfterCommit 不执行、Sync 门保持冻结，计数 remote_entity.deferred_outcome_not_run_total{outcome=committed} +1；持久结果本身
// 不受影响，资源照常交还。
// 历史：RR-20260926-61。
func TestNestDeferredPostCommitIsNotRunOffThePool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rawID int64
		stop  func(*nest.NestMgr) error
	}{
		{name: "nest_stopped", rawID: 1973, stop: func(engine *nest.NestMgr) error { return engine.Shutdown(context.Background()) }},
		{name: "nest_fenced", rawID: 1974, stop: func(engine *nest.NestMgr) error {
			engine.Fence(errors.New("test: commit outcome unknown, process fenced"))
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newDeferredOutcomeNestRig(t, tc.rawID, "deferred_outcome_"+tc.name)
			rig.f.storage.loseReply.Store(true)
			awaitHeld, release := rig.holdFinalizer()
			rig.request(t, "trace-"+tc.name)
			awaitHeld(t)
			if err := tc.stop(rig.engine); err != nil {
				t.Fatal(err)
			}
			before := deferredOutcomeNotRun("committed")
			release()
			rig.awaitReleased(t)
			if ran, _ := rig.counts(); ran != 0 {
				t.Fatalf("AfterCommit ran %d time(s) after the Nest refused the fast-pool handoff (want 0: never run business callbacks on the finalizer goroutine)", ran)
			}
			if rig.live.Sync().SyncCommitReady() {
				t.Fatal("Sync gate released although the post-commit work never ran (the framework Confirm must not run off the pool either)")
			}
			if got := deferredOutcomeNotRun("committed") - before; got != 1 {
				t.Fatalf("deferred_outcome_not_run_total{outcome=committed} grew by %d, want 1", got)
			}
			if doc := rig.f.storedDocument(t); doc != "a=rejected;b=" {
				t.Fatalf("Mongo document=%q, want the committed write", doc)
			}
			if slots := len(rig.f.mgr.remote.writeSlots); slots != 0 {
				t.Fatalf("write slots=%d after the committed outcome", slots)
			}
		})
	}
}
