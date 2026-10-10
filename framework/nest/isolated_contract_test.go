package nest

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type prepareHookGetter struct {
	*mockGetter
	hook func()
}

func (g *prepareHookGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.hook()
	return g.mockGetter.Get(ctx, id, category)
}

func (g *prepareHookGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	g.hook()
	return g.mockGetter.GetMany(ctx, ids, categories)
}

// isolatedInPrepare 记录慢阶段准备里那一次 RunIsolatedTransaction 的观察结果（只在第一次读取时调用）。
type isolatedInPrepare struct {
	once              atomic.Bool
	inMessage, inFast bool
	callRan           bool
	err               error
}

func (o *isolatedInPrepare) run(iso TransactionCommitter, target int64) {
	if !o.once.CompareAndSwap(false, true) {
		return
	}
	o.inMessage, o.inFast = currentNestDispatchMsg() != nil, fctx.InFastWorker()
	_, o.err = RunIsolatedTransaction(context.Background(), iso, "b19_iso_in_prepare", func() (any, error) {
		o.callRan = true
		return nil, CurrentRollbackTx().AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: target, Database: "test", Resource: "b19_prepare"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte(`{}`)})
	})
}

func TestIsolatedInSlowPrepareOfLocalMessageDoesNotClaimIt(t *testing.T) {
	id := mustBuildCastID(t, 48600, entity.EntityCategory(1), nestLocalKind)
	obs := &isolatedInPrepare{}
	iso := &recordsCommitter{}
	getter := &prepareHookGetter{mockGetter: newMockGetter(), hook: func() { obs.run(iso, id) }}
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	outer := &recordsCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}))
	name := NewHandlerName("b19_local_after_prepare")
	var attempts atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		attempts.Add(1)
		return nil, fmt.Errorf("%w: outer failed after the isolated commit in prepare", ErrLockTimeout)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := mgr.Request(ctx, name, id, nil, SendOptionSlow())

	if !obs.inMessage || obs.inFast {
		t.Fatalf("premise: the isolated call must run in the message's slow stage (in message=%v, fast worker=%v)", obs.inMessage, obs.inFast)
	}
	if obs.err != nil || !obs.callRan || iso.count() != 1 {
		t.Fatalf("local message: isolated err=%v ran=%v commits=%d, want it to run and commit once", obs.err, obs.callRan, iso.count())
	}
	if errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("reply=%v: the isolated transaction in prepare claimed the message (reply reports the message's own commit)", err)
	}
	if !errors.Is(err, ErrNestedTransactionCommitted) || !errors.Is(err, ErrLockTimeout) || attempts.Load() != 1 {
		t.Fatalf("reply=%v attempts=%d: want ErrNestedTransactionCommitted around the outer error and no requeue (RR-65)", err, attempts.Load())
	}
	if outer.count() != 0 {
		t.Fatalf("outer committer got %d record(s) although the handler failed", outer.count())
	}
}

func TestIsolatedInSlowPrepareOfRemoteMessageIsRefused(t *testing.T) {
	localID, local := newAsyncPilotEntity(t, 48610, 1)
	remoteID := mustBuildCastID(t, 48611, entity.EntityCategoryRemote, nestRemoteManagedKind)
	obs := &isolatedInPrepare{}
	iso := &recordsCommitter{}
	getter := &prepareHookGetter{mockGetter: newMockGetter(), hook: func() { obs.run(iso, localID) }}
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(local)
	var batchCommits atomic.Int32
	batch := &stagedRemoteBatch{commit: func() { batchCommits.Add(1) }}
	remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("b19_remote_after_prepare")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	var reply any
	select {
	case reply = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("Remote message did not reply within 10s")
	}

	if !obs.inMessage || obs.inFast {
		t.Fatalf("premise: the isolated call must run in the message's slow stage (in message=%v, fast worker=%v)", obs.inMessage, obs.inFast)
	}
	if !errors.Is(obs.err, ErrNestedTransactionInRemoteMessage) || obs.callRan || iso.count() != 0 {
		t.Fatalf("Remote message: isolated err=%v ran=%v commits=%d, want ErrNestedTransactionInRemoteMessage with the body not executed", obs.err, obs.callRan, iso.count())
	}
	if reply != "ok" || batchCommits.Load() != 1 || batch.aborted.Load() {
		t.Fatalf("reply=%v batch commits=%d aborted=%v: the refused isolated call must not touch the message or its batch", reply, batchCommits.Load(), batch.aborted.Load())
	}
}

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
