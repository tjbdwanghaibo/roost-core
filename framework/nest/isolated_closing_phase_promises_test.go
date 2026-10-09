package nest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-84：消息自己的事务结束之后（提交、回滚或失败），在这条消息的收尾阶段（Guard post-release 回调、解锁后回调，
// Remote 批次收尾之前）调用 RunIsolatedTransaction，不认领消息、不碰 Remote 批次与 Msg 上的提交事实：
//   - 带 Remote 批次的消息：返回 ErrNestedTransactionInRemoteMessage，函数体不执行，批次只随消息自己的事务 Commit / Abort；
//   - 纯本地消息：按不认领消息的嵌套独立事务处理，RR-65 的 nestedTxCommitted 与 RR-76 的 fence 同样适用。
//
// 旧行为（REPRO-2026-09-26-08 §1 探针 A / A2）：消息自己的事务返回后 txInFlight 已复位，独立事务被当成“消息自己的事务”
// 认领消息（tx.dispatch = msg）：Remote 消息外层失败时批次 FinalizeLocked 收到独立事务、Commit=1、回复误带 ErrAfterCommitFailed；
// 纯本地消息外层已回滚也误报 ErrAfterCommitFailed；回调内独立事务结果未知时 tx.dispatch 非 nil，引擎不 fence。

// releaseNotifyingCommitter 在 TransactionReleased（带 Remote 批次的消息里经解锁后回调执行）里调用 onReleased。
type releaseNotifyingCommitter struct {
	recordsCommitter
	onReleased func()
}

func (c *releaseNotifyingCommitter) TransactionReleased(TransactionID) {
	if c.onReleased != nil {
		c.onReleased()
	}
}

func TestIsolatedInClosingPhaseOfRemoteMessageIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name        string
		meta        HandlerMeta
		outerFails  bool
		afterUnlock bool // true：在解锁后回调（TransactionReleased）里调用；否则在 Guard post-release 回调里
	}{
		{"memory_outer_fails", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, true, false},
		{"state_strict_outer_fails", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, true, false},
		{"memory_outer_succeeds", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, false, false},
		{"state_strict_outer_succeeds_after_unlock", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 48400+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 48401+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &recordingRemoteBatch{}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			outer, iso := &releaseNotifyingCommitter{}, &recordsCommitter{}
			var isoErr error
			var isoCalled, isoRuns atomic.Int64
			isolated := func() {
				isoCalled.Add(1)
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr84_iso_in_closing", func() (any, error) {
					isoRuns.Add(1)
					old := e.dao.Value
					RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
					e.dao.Value = 55
					return nil, MarkPersist(e.dao, 1)
				})
			}
			if tc.afterUnlock {
				outer.onReleased = isolated
			}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr84_remote_closing_%d", i))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if !tc.afterUnlock {
					scope := entity.CurrentGuardScope()
					if scope == nil || scope.Guard() == nil {
						return nil, errors.New("test premise: no guard scope")
					}
					scope.Guard().AppendPostRelease(isolated)
				}
				if tc.outerFails {
					return nil, errors.New("outer business failed")
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			replyErr, _ := reply.(error)
			finalized := batch.finalizedHandlers()
			if isoCalled.Load() != 1 {
				t.Fatalf("test premise: the closing-phase callback ran %d times, want 1 (reply=%v)", isoCalled.Load(), reply)
			}
			if slices.Contains(finalized, "rr84_iso_in_closing") || iso.count() != 0 || isoRuns.Load() != 0 {
				t.Fatalf("isolated transaction in the closing phase went through the message's Remote batch: FinalizeLocked=%v isolatedRuns=%d isolatedCommits=%d isoErr=%v batch Commit=%d Abort=%d reply=%v",
					finalized, isoRuns.Load(), iso.count(), isoErr, batch.commits.Load(), batch.aborts.Load(), reply)
			}
			if !errors.Is(isoErr, ErrNestedTransactionInRemoteMessage) {
				t.Fatalf("RunIsolatedTransaction in the closing phase of a Remote message returned %v, want ErrNestedTransactionInRemoteMessage", isoErr)
			}
			if tc.outerFails {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 0 || a != 1 {
					t.Fatalf("outer failed: batch Commit=%d Abort=%d, want 0/1 (reply=%v)", c, a, reply)
				}
				if replyErr == nil || errors.Is(replyErr, ErrAfterCommitFailed) || errors.Is(replyErr, ErrNestedTransactionCommitted) {
					t.Fatalf("outer failed: reply=%v must not claim a commit", reply)
				}
			} else {
				if c, a := batch.commits.Load(), batch.aborts.Load(); c != 1 || a != 0 || !slices.Equal(finalized, []string{name.String()}) {
					t.Fatalf("outer succeeded: batch Commit=%d Abort=%d FinalizeLocked=%v, want 1/0/[%s] (reply=%v)", c, a, finalized, name, reply)
				}
				if reply != "ok" {
					t.Fatalf("outer succeeded: reply=%v, want ok", reply)
				}
			}
			if e.dao.Value != 10 {
				t.Fatalf("value=%d, want 10: the refused isolated call must not run", e.dao.Value)
			}
		})
	}
}

func TestIsolatedInClosingPhaseOfLocalMessageDoesNotClaimIt(t *testing.T) {
	for i, tc := range []struct {
		name          string
		meta          HandlerMeta
		outerErr      error // 第一次执行时外层返回的错误；nil 表示成功
		indeterminate bool  // 回调里的独立事务提交结果未知
	}{
		{"undo_strict_outer_fails", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, errors.New("outer failed"), false},
		{"memory_outer_fails", HandlerMeta{}, errors.New("outer failed"), false},
		{"undo_strict_outer_lock_timeout", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, fmt.Errorf("%w: outer contended", ErrLockTimeout), false},
		{"undo_strict_outer_ok_hook_indeterminate", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, true},
		{"memory_outer_ok_hook_indeterminate", HandlerMeta{}, nil, true},
		{"undo_strict_outer_ok_hook_commits", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pID, p := newAsyncPilotEntity(t, 48500+int64(i)*10, 1)
			if err := manager.TryAdd(p); err != nil {
				t.Fatal(err)
			}
			access := entity.NewManagerAccess(manager)
			outer := &recordsCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			hookCommits := &recordsCommitter{}
			var hookCommitter TransactionCommitter = hookCommits
			if tc.indeterminate {
				hookCommitter = &indeterminateCommitter{}
			}
			var hookErr error
			var attempts atomic.Int64
			name := NewHandlerName("rr84_local_closing_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				n := attempts.Add(1)
				declared := es[0].(*rollbackTestEntity)
				entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
					_, hookErr = RunIsolatedTransaction(context.Background(), hookCommitter, "rr84_hook_iso", func() (any, error) {
						old := declared.dao.Value
						RecordUndo(declared.dao, 1, func() error { declared.dao.Value = old; return nil })
						declared.dao.Value = 40
						return nil, MarkPersist(declared.dao, 1)
					})
				})
				if tc.outerErr != nil && n == 1 {
					return nil, tc.outerErr
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			r := requestWithin(t, mgr, name, pID)
			if attempts.Load() != 1 {
				t.Fatalf("attempts=%d reply=%v/%v hookErr=%v: a message whose closing-phase isolated transaction committed must not be requeued", attempts.Load(), r.ret, r.err, hookErr)
			}
			if errors.Is(r.err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v claims the message's own transaction committed (outer commits=%d, hookErr=%v): the closing-phase isolated transaction must not claim the message", r.err, outer.count(), hookErr)
			}
			switch {
			case tc.indeterminate:
				fenced := mgr.FenceError()
				if !errors.Is(hookErr, ErrCommitIndeterminate) || !errors.Is(fenced, ErrNestFenced) || !errors.Is(fenced, ErrCommitIndeterminate) {
					t.Fatalf("closing-phase isolated transaction outcome unknown: hookErr=%v FenceError=%v reply=%v/%v, want the engine fenced like any nested isolated transaction (RR-76)", hookErr, fenced, r.ret, r.err)
				}
				if r.err != nil || r.ret != "ok" {
					t.Fatalf("reply=%v/%v, want ok: the message's own transaction committed before the closing phase", r.ret, r.err)
				}
				second := requestWithin(t, mgr, name, pID)
				if !errors.Is(second.err, ErrNestFenced) || attempts.Load() != 1 {
					t.Fatalf("second request reply=%v/%v attempts=%d: a fenced engine must refuse new work", second.ret, second.err, attempts.Load())
				}
			case tc.outerErr != nil:
				if hookErr != nil || hookCommits.count() != 1 || p.dao.Value != 40 {
					t.Fatalf("premise: hookErr=%v hookCommits=%d value=%d, want the closing-phase isolated transaction committed once", hookErr, hookCommits.count(), p.dao.Value)
				}
				if !errors.Is(r.err, ErrNestedTransactionCommitted) || !errors.Is(r.err, tc.outerErr) {
					t.Fatalf("reply=%v, want ErrNestedTransactionCommitted wrapping the outer error (RR-65)", r.err)
				}
				if outer.count() != 0 {
					t.Fatalf("outer commits=%d, want 0: the outer transaction failed", outer.count())
				}
			default:
				if hookErr != nil || hookCommits.count() != 1 || r.err != nil || r.ret != "ok" {
					t.Fatalf("hookErr=%v hookCommits=%d reply=%v/%v, want ok and one closing-phase commit", hookErr, hookCommits.count(), r.ret, r.err)
				}
				if err := mgr.FenceError(); err != nil {
					t.Fatalf("FenceError=%v, want nil", err)
				}
			}
		})
	}
}
