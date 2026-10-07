package nest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-49：事务已越过提交点后，回复错误链里即使带锁超时类错误，也不能被 requeueTransientDispatch
// 重新准入——那会把已提交的业务再执行一次。提交回调的 panic 以 %w 保留原因链。

type lockTimeoutCloseBatch struct{ stagedRemoteBatch }

func (b *lockTimeoutCloseBatch) Close(context.Context) error {
	return fmt.Errorf("release step reported %w", ErrLockTimeout)
}

func sendRemoteTestMsg(t *testing.T, mgr *NestMgr, name HandlerName, remoteID, localID int64) error {
	t.Helper()
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case reply := <-ch:
		err, _ := reply.(error)
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("no reply after 15s")
		return nil
	}
}

func remoteCommitEngine(t *testing.T, unique int64, batch func() entity.RemoteWriteBatch) (*NestMgr, int64, int64) {
	t.Helper()
	localID, e := newAsyncPilotEntity(t, unique, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch(), nil }}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerNumAndMsgCap(1, 16))
	return mgr, remoteID, localID
}

// REPRO-2026-09-26-05 §2 替身：Remote 已确认提交，批次 Close 返回包裹 ErrLockTimeout 的错误。修前 handler 被执行 401 次。
func TestCommittedRemoteReplyIsNotRequeued(t *testing.T) {
	mgr, remoteID, localID := remoteCommitEngine(t, 9960, func() entity.RemoteWriteBatch { return &lockTimeoutCloseBatch{} })
	name := NewHandlerName("rr49_remote_close_lock_timeout")
	var runs atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		runs.Add(1)
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	err := sendRemoteTestMsg(t, mgr, name, remoteID, localID)
	if n := runs.Load(); n != 1 {
		t.Fatalf("committed remote transaction was executed %d times (reply=%v)", n, err)
	}
	if !errors.Is(err, ErrAfterCommitFailed) || !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("reply=%v, want ErrAfterCommitFailed with the release cause chain", err)
	}
}

// 纯本地 strict 事务持久提交后，release hook 抛出包裹 ErrLockTimeout 的错误：Msg 的显式已提交状态阻止重新准入。
// 修前事务被重新准入，业务再执行一次（持久记录与内存值都多加一次）。
func TestCommittedLocalReplyIsNotRequeued(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 9965, 10)
	owner := entity.NewEntityManager()
	if err := owner.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	defer owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
		if armed.CompareAndSwap(true, false) {
			panic(fmt.Errorf("release hook reported %w", ErrLockTimeout))
		}
	})()
	committer := &countingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("rr49_local_release_lock_timeout")
	var runs atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		runs.Add(1)
		old := e.dao.Value
		RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
		e.dao.Value++
		armed.Store(true)
		return "ok", MarkPersist(e.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	_, err := mgr.Request(context.Background(), name, localID, nil)
	if n := runs.Load(); n != 1 {
		t.Fatalf("committed local transaction was executed %d times (commits=%d value=%d reply=%v)", n, committer.calls.Load(), e.dao.Value, err)
	}
	if committer.calls.Load() != 1 || e.dao.Value != 11 {
		t.Fatalf("commits=%d value=%d, want one durable commit to 11", committer.calls.Load(), e.dao.Value)
	}
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("reply=%v, want the release hook error", err)
	}
}

// 提交回调 panic 的原因以 %w 保留：Remote 已提交后的 AfterCommit panic(errX) 回复满足 errors.Is(errX)，且不重新准入。
func TestAfterCommitPanicKeepsCauseChain(t *testing.T) {
	mgr, remoteID, localID := remoteCommitEngine(t, 9975, func() entity.RemoteWriteBatch { return &stagedRemoteBatch{} })
	cause := fmt.Errorf("after-commit hook reported %w", ErrLockTimeout)
	name := NewHandlerName("rr49_after_commit_panic_chain")
	var runs atomic.Int64
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		runs.Add(1)
		AfterCommit(func() { panic(cause) })
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	err := sendRemoteTestMsg(t, mgr, name, remoteID, localID)
	if !errors.Is(err, ErrAfterCommitFailed) || !errors.Is(err, cause) {
		t.Fatalf("reply=%v: errors.Is(ErrAfterCommitFailed)=%v errors.Is(cause)=%v, want both", err, errors.Is(err, ErrAfterCommitFailed), errors.Is(err, cause))
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("committed transaction was executed %d times", n)
	}
}

// countingCommitter 是可并发调用的 strict committer。
type countingCommitter struct {
	calls atomic.Int64
	err   error
}

func (c *countingCommitter) Commit(context.Context, CommitRecord) error {
	c.calls.Add(1)
	return c.err
}
