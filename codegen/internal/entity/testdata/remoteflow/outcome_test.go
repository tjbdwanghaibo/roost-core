package remoteflow

// REMAINING-2026-09-28 B27 第 1 批：reject_test.go 的正式装配（ManagerAccess 作 Remote loader、entitysync、真实 Mongo / Redis / NATS）
// 上补的另外几条端到端：strict 确认截止后的延迟回调（RR-37 / RR-20260927-24）、结果未知后停机 / fence（RR-63 / 61）、
// 带 Remote 批次的消息里的嵌套 / 收尾阶段独立事务（RR-75 / 84）、快 worker 上删除 Remote 实体（RR-27）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// RR-20260926-37 / RR-20260927-24：strict 等 Remote 确认到截止（投影器被拖住）后，提交后工作交给 finalizer；投影器随后照常
// 提交，AfterCommit 在快池恰好执行一次、Sync 门放行。截止到结论之间 AfterCommit 不执行、订阅者什么都没收到。
func TestGeneratedRemoteStrictConfirmDeadlineDefersPostCommit(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, nil)
	id := rig.seed(t, 9300)
	if err := rig.request(ctx, id); err != nil {
		t.Fatal(err)
	}
	rig.assertNoPending(t)
	rig.flushUntil(t, id, "the first committed write")
	eventually(t, "the first AfterCommit", func() bool { return rig.afterCommit.Load() == 1 })
	gate := &rig.projector.gate
	gate.arm(id, faultHold)
	t.Cleanup(gate.open)
	requestCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	err := rig.request(requestCtx, id)
	cancel()
	// Request 的调用方与 Remote 确认等待用同一个 ctx：截止时调用方可能先拿到 ErrNestCanceled，也可能拿到批次的回复
	// （ErrRemoteCommitTimeout + ErrRemotePersistenceIndeterminate + DeadlineExceeded，RR-24）。两种都带 DeadlineExceeded，都不带已提交哨兵。
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nest.ErrAfterCommitFailed) {
		t.Fatalf("strict write whose Remote confirmation hit the deadline replied %v, want a deadline without the committed sentinel", err)
	}
	if errors.Is(err, entity.ErrRemoteCommitTimeout) && !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("RR-20260927-24: strict confirmation deadline replied %v without ErrRemotePersistenceIndeterminate", err)
	}
	select {
	case <-gate.held:
	case <-ctx.Done():
		t.Fatal("the strict record never reached the projector")
	}
	if ran := rig.afterCommit.Load(); ran != 1 {
		t.Fatalf("AfterCommit ran %d time(s) before the durable conclusion, want only the first write's", ran)
	}
	rig.flushNone(t, id, "after the deadline, before the durable conclusion")
	gate.open()
	eventually(t, "the deferred AfterCommit runs once the projector commits", func() bool { return rig.afterCommit.Load() == 2 })
	if onFast := rig.afterCommitOnFast.Load(); onFast != 2 {
		t.Fatalf("AfterCommit on a fast worker %d/2 times", onFast)
	}
	if got := rig.flushUntil(t, id, "the Sync release after the durable commit"); got[len(got)-1].balance != 2 {
		t.Fatalf("frames after the durable commit=%v", got)
	}
	rig.assertNoPending(t)
	if version, balance, items, _ := rig.stored(t, id); version != 2 || balance != 2 || items != 2 {
		t.Fatalf("Mongo version=%d balance=%d items=%d, want the deferred commit 2/2/2", version, balance, items)
	}
	// 至多一次：下一笔提交之后恰好是 3，延迟的那次没有重复执行。
	if err := rig.request(ctx, id); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the next AfterCommit", func() bool { return rig.afterCommit.Load() >= 3 })
	rig.assertNoPending(t)
	if ran := rig.afterCommit.Load(); ran != 3 {
		t.Fatalf("AfterCommit ran %d times after three commits", ran)
	}
	t.Logf("deadline reply: %v", err)
}

func deferredOutcomeNotRun(outcome string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "remote_entity.deferred_outcome_not_run_total" && metric.Labels["outcome"] == outcome {
			total += metric.Value
		}
	}
	return total
}

// RR-20260926-63 / 61：Durability 0 写已落 Mongo、回复丢失（结果未知），持久结论到达时 Nest 已停机或已 fence：提交后工作不离开
// 快池执行——AfterCommit 不执行、Sync 门保持冻结，计数 remote_entity.deferred_outcome_not_run_total{outcome=committed} +1；
// 持久结果本身不受影响（Mongo 已是这一笔）。
func TestGeneratedRemoteUnknownOutcomeAfterNestStopIsNotRunOffThePool(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	for index, tc := range []struct {
		name string
		stop func(*nest.NestMgr) error
	}{
		{name: "nest_stopped", stop: func(scheduler *nest.NestMgr) error { return scheduler.Shutdown(context.Background()) }},
		{name: "nest_fenced", stop: func(scheduler *nest.NestMgr) error {
			scheduler.Fence(errors.New("remoteflow: commit outcome unknown, process fenced"))
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityMemory, nil, nil)
			id := rig.seed(t, int64(9400+index))
			if err := rig.request(ctx, id); err != nil {
				t.Fatal(err)
			}
			rig.flushUntil(t, id, "the first committed write")
			eventually(t, "the first AfterCommit", func() bool { return rig.afterCommit.Load() == 1 })
			rig.committer.gate.arm(id, faultLoseReply)
			t.Cleanup(rig.committer.gate.open)
			if err := rig.request(ctx, id); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) || errors.Is(err, nest.ErrAfterCommitFailed) {
				t.Fatalf("lost-reply write replied %v, want an unknown outcome without the committed sentinel", err)
			}
			if err := tc.stop(rig.scheduler); err != nil {
				t.Fatal(err)
			}
			before := deferredOutcomeNotRun("committed")
			rig.committer.gate.open()
			eventually(t, "the finalizer reaches the committed outcome and the Nest refuses the handoff", func() bool {
				return deferredOutcomeNotRun("committed")-before >= 1
			})
			if got := deferredOutcomeNotRun("committed") - before; got != 1 {
				t.Fatalf("deferred_outcome_not_run_total{outcome=committed} grew by %d, want 1", got)
			}
			if ran := rig.afterCommit.Load(); ran != 1 {
				t.Fatalf("AfterCommit ran %d time(s), want only the first write's (never on the finalizer goroutine)", ran)
			}
			vault := rig.vault(t, id)
			if vault == nil || vault.Base().Sync().SyncCommitReady() {
				t.Fatal("Sync gate released although the post-commit work never ran")
			}
			rig.flushNone(t, id, "after the refused handoff")
			if version, balance, items, _ := rig.stored(t, id); version != 2 || balance != 2 || items != 2 {
				t.Fatalf("Mongo version=%d balance=%d items=%d, want the committed lost-reply write 2/2/2", version, balance, items)
			}
			select {
			case err := <-rig.fatal:
				t.Fatalf("runtime fatal: %v", err)
			default:
			}
		})
	}
}

// RR-20260926-75 / 84：带 Remote 批次的消息里，handler 内与收尾阶段（实体 release hook，Remote 批次收尾之前）调用
// RunIsolatedTransaction 都返回 ErrNestedTransactionInRemoteMessage、函数体不执行、不进 WAL；外层照常按自己的结果提交或回滚，
// 回复不带 ErrAfterCommitFailed / ErrNestedTransactionCommitted。
func TestGeneratedRemoteNestedIsolatedTransactionIsRefused(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	type mode struct {
		inHandler, fail bool
	}
	isolated := nest.NewHandlerName("remote_reject_isolated")
	businessErr := errors.New("outer business failed after the isolated call")
	var (
		mu         sync.Mutex
		isoErrs    []error
		isoRan     atomic.Int32
		closingFor atomic.Int64
	)
	runIsolated := func(rig *rejectRig) {
		_, err := nest.RunIsolatedTransaction(context.Background(), rig.localCommitter, "remote_reject_isolated_inner", func() (any, error) {
			isoRan.Add(1)
			return nil, nil
		})
		mu.Lock()
		isoErrs = append(isoErrs, err)
		mu.Unlock()
	}
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, func(rig *rejectRig) {
		rig.scheduler.MustRegisterHandlerWithMeta(isolated, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			vault := es[0].(*SyncedVault)
			vault.balance.SetValue(vault.balance.GetValue() + 1)
			vault.items.SetValue(vault.items.GetValue() + 1)
			m := params[0].(mode)
			if m.inHandler {
				runIsolated(rig)
			}
			if m.fail {
				return nil, businessErr
			}
			return nil, nil
		}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityStrict})
		unhook, err := rig.access.RegisterOnEntityRelease(func(e entity.IThreadSafeEntity) {
			if id := e.ID(); id != 0 && closingFor.CompareAndSwap(id, 0) {
				runIsolated(rig)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unhook)
	})
	id := rig.seed(t, 9500)
	want := int64(0)
	for _, m := range []mode{{inHandler: true}, {inHandler: true, fail: true}, {}, {fail: true}} {
		name := fmt.Sprintf("in_handler=%v/outer_fails=%v", m.inHandler, m.fail)
		mu.Lock()
		isoErrs = nil
		mu.Unlock()
		if !m.inHandler {
			closingFor.Store(id)
		}
		admitted := rig.wal.Stats().Admitted
		_, err := rig.scheduler.Request(ctx, isolated, id, nest.Params{m})
		switch {
		case m.fail && (!errors.Is(err, businessErr) || errors.Is(err, nest.ErrAfterCommitFailed) || errors.Is(err, nest.ErrNestedTransactionCommitted)):
			t.Fatalf("%s: reply=%v, want the business error alone", name, err)
		case !m.fail && err != nil:
			t.Fatalf("%s: reply=%v", name, err)
		}
		if !m.fail {
			want++
		}
		mu.Lock()
		errs := append([]error(nil), isoErrs...)
		mu.Unlock()
		if len(errs) != 1 || !errors.Is(errs[0], nest.ErrNestedTransactionInRemoteMessage) {
			t.Fatalf("%s: RunIsolatedTransaction returned %v, want exactly one ErrNestedTransactionInRemoteMessage", name, errs)
		}
		if ran := isoRan.Load(); ran != 0 {
			t.Fatalf("%s: the refused isolated transaction ran %d time(s)", name, ran)
		}
		rig.assertNoPending(t)
		if delta := rig.wal.Stats().Admitted - admitted; (m.fail && delta != 0) || (!m.fail && delta != 1) {
			t.Fatalf("%s: WAL admitted %d record(s)", name, delta)
		}
		version, balance, items, found := rig.stored(t, id)
		if want == 0 && found || want > 0 && (version != uint64(want) || balance != want || items != want) {
			t.Fatalf("%s: Mongo version=%d balance=%d items=%d found=%v, want %d", name, version, balance, items, found, want)
		}
		if balance, items := rig.memory(t, id); balance != want || items != want {
			t.Fatalf("%s: memory balance=%d items=%d, want %d", name, balance, items, want)
		}
	}
}

// RR-20260926-27：快 worker 上直接删除一个没有声明进消息的 Remote 托管实体，删除准入在任何副作用之前确定拒绝：Runtime 不 fatal、
// Nest 不 fence，实体留在内存、Mongo 不变，之后照常可写。两种调用位置：
//   - local_memory_message：目标是本地 Clerk 的 memory handler（没有 RollbackTx），走 admitRemoteEntityDelete 的快池检查，
//     错误可 errors.Is(fctx.ErrBlockingInFastWorker)（RR-27 修复的那条路径）；
//   - remote_message：目标是另一个 Remote 实体的消息（Remote 消息总有 RollbackTx），删除随事务走 deferEntityDelete，
//     未声明的 Remote 删除目标以 nest.ErrDurableRemoteWriteUnsupported 拒绝。
func TestGeneratedRemoteFastWorkerDeleteIsDefiniteRejection(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	deleteOther := nest.NewHandlerName("remote_reject_delete_other")
	var (
		victim     atomic.Int64
		destroyErr atomic.Pointer[error]
	)
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityStrict, nil, func(rig *rejectRig) {
		rig.scheduler.MustRegisterHandlerWithMeta(deleteOther, func([]entity.IThreadSafeEntity, []any, ...nest.HandlerOption) (any, error) {
			err := rig.access.Destroy(context.Background(), rig.access.Manager().Get(victim.Load()), entity.DestroyReasonCommon, true)
			destroyErr.Store(&err)
			return nil, nil
		}, nest.HandlerMeta{})
	})
	remoteCaller := rig.seed(t, 9600)
	localCaller, err := entity.BuildEntityID(9602, EntityKindClerk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindClerk, Id: localCaller}); err != nil {
		t.Fatal(err)
	}
	target, err := entity.BuildEntityID(9601, EntityKindSyncedVault)
	if err != nil {
		t.Fatal(err)
	}
	created, err := rig.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindSyncedVault, Id: target})
	if err != nil {
		t.Fatal(err)
	}
	prepareRemoteOwnership(t, ctx, rig.manager, []int64{target})
	if err := rig.request(ctx, target); err != nil {
		t.Fatal(err)
	}
	rig.assertNoPending(t)
	victim.Store(target)
	want := int64(1)
	for _, tc := range []struct {
		name   string
		caller int64
		cause  error
	}{
		{name: "local_memory_message", caller: localCaller, cause: fctx.ErrBlockingInFastWorker},
		{name: "remote_message", caller: remoteCaller, cause: nest.ErrDurableRemoteWriteUnsupported},
	} {
		destroyErr.Store(nil)
		if _, err := rig.scheduler.Request(ctx, deleteOther, tc.caller, nil); err != nil {
			t.Fatalf("%s: delete_other reply: %v", tc.name, err)
		}
		var got error
		if stored := destroyErr.Load(); stored != nil {
			got = *stored
		}
		if !errors.Is(got, tc.cause) {
			t.Fatalf("%s: fast-worker Remote delete returned %v, want errors.Is(%v)", tc.name, got, tc.cause)
		}
		select {
		case err := <-rig.fatal:
			t.Fatalf("%s: a definite rejection before any side effect escalated to runtime fatal: %v", tc.name, err)
		default:
		}
		if rig.access.Manager().Get(target) != created {
			t.Fatalf("%s: the definitively rejected delete removed the entity from memory", tc.name)
		}
		if version, balance, _, found := rig.stored(t, target); !found || version != uint64(want) || balance != want {
			t.Fatalf("%s: Mongo after the refused delete version=%d balance=%d found=%v, want %d", tc.name, version, balance, found, want)
		}
		// Nest 没有被 fence：同一实体的下一笔写照常提交。
		if err := rig.request(ctx, target); err != nil {
			t.Fatalf("%s: write after the refused delete: %v", tc.name, err)
		}
		want++
		rig.assertNoPending(t)
		if version, balance, _, _ := rig.stored(t, target); version != uint64(want) || balance != want {
			t.Fatalf("%s: Mongo after the next write version=%d balance=%d, want %d", tc.name, version, balance, want)
		}
	}
}
