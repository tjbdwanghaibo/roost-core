// 提交之后的回复：已提交的消息不重排并带 ErrAfterCommitFailed，未提交 / 回滚的消息不带该哨兵且保留业务错误。

package nest

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 事务越过提交点后，回复错误链里即使带锁超时类错误也不重新准入（否则已提交的业务会再执行一次）；
// 提交回调的 panic 以 %w 保留原因链。
// 历史：RR-20260926-49。

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
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	return mgr, remoteID, localID
}

// Remote 已确认提交、批次 Close 返回包裹 ErrLockTimeout 的错误：handler 只执行一次。
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

// 纯本地 strict 事务持久提交后 release hook 抛出包裹 ErrLockTimeout 的错误：Msg 的显式已提交状态阻止重新准入，
// 回复带 ErrAfterCommitFailed 与 hook 原因。
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
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	if !errors.Is(err, ErrLockTimeout) || !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("reply=%v, want ErrAfterCommitFailed with the release hook error", err)
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

// 纯本地事务提交成功后发生的释放 / 回调错误带 ErrAfterCommitFailed（保留原因链）；未提交路径不带。
// 历史：RR-20260926-53。

type localHookFixture struct {
	id      int64
	e       *rollbackTestEntity
	owner   *entity.EntityManager
	hookErr error
	armed   bool
}

func newLocalHookFixture(t *testing.T, unique int64) *localHookFixture {
	t.Helper()
	id, e := newAsyncPilotEntity(t, unique, 10)
	f := &localHookFixture{id: id, e: e, owner: entity.NewEntityManager(), hookErr: errors.New("release hook failed")}
	if err := f.owner.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
		if f.armed {
			f.armed = false
			panic(f.hookErr)
		}
	}))
	return f
}

// memory 快路径 handler 成功即已提交（内存）；随后的 release hook 失败同样带哨兵。
func TestMemoryHandlerReleaseHookPanicCarriesSentinel(t *testing.T) {
	f := newLocalHookFixture(t, 9983)
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr53_memory_release_hook")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		f.e.dao.Value++
		f.armed = true
		return "ok", nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	_, err := mgr.Request(context.Background(), name, f.id, nil)
	if !errors.Is(err, ErrAfterCommitFailed) || !errors.Is(err, f.hookErr) {
		t.Fatalf("reply=%v afterCommit=%v hook=%v", err, errors.Is(err, ErrAfterCommitFailed), errors.Is(err, f.hookErr))
	}
}

// 未提交路径：handler 报错、提交被明确拒绝后 release hook panic，回复不得带哨兵。
func TestUncommittedLocalReleaseHookPanicHasNoSentinel(t *testing.T) {
	for i, tc := range []struct {
		name       string
		handlerErr error
		commitErr  error
	}{
		{"handler_failed", errors.New("business failed"), nil},
		{"commit_rejected", nil, errors.New("wal rejected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalHookFixture(t, 9986+int64(i))
			committer := &recordingCommitter{err: tc.commitErr}
			mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("rr53_uncommitted_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				old := f.e.dao.Value
				RecordUndo(f.e.dao, 1, func() error { f.e.dao.Value = old; return nil })
				f.e.dao.Value++
				f.armed = true
				if tc.handlerErr != nil {
					return nil, tc.handlerErr
				}
				return "ok", MarkPersist(f.e.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			_, err := mgr.Request(context.Background(), name, f.id, nil)
			if err == nil || errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v: an uncommitted transaction must fail without ErrAfterCommitFailed", err)
			}
			if f.e.dao.Value != 10 {
				t.Fatalf("value=%d, want rollback to 10", f.e.dao.Value)
			}
		})
	}
}

// handler 返回业务错误（事务回滚）后释放实体时 OnEntityRelease 钩子 panic：回复仍以业务错误为主，hook 的 panic 以
// errors.Join 附加在错误链上，且不带 ErrAfterCommitFailed——什么都没提交。已提交路径的哨兵见本文件前面的用例。
// 历史：RR-20260930-20。

var errRolledBackBusiness = errors.New("business rejected")

func TestRolledBackReleaseHookPanicKeepsBusinessError(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"memory", HandlerMeta{}},
		{"strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}},
		{"pipelined", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalHookFixture(t, 10060+int64(i))
			var committer TransactionCommitter
			recording := &recordingCommitter{}
			switch tc.meta.Durability {
			case DurabilityPipelined:
				committer = newPipelinedTestCommitter(true)
			default:
				committer = recording
			}
			mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("rr20_rolled_back_release_hook_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if tc.meta.Rollback == RollbackUndo {
					old := f.e.dao.Value
					RecordUndo(f.e.dao, 1, func() error { f.e.dao.Value = old; return nil })
					f.e.dao.Value++
				}
				f.armed = true
				return nil, errRolledBackBusiness
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			_, err := mgr.Request(context.Background(), name, f.id, nil)
			if f.armed {
				t.Fatal("premise: the release hook did not run")
			}
			if tc.meta.Rollback == RollbackUndo && f.e.dao.Value != 10 {
				t.Fatalf("value=%d, want rollback to 10", f.e.dao.Value)
			}
			if len(recording.record.Mutations) != 0 {
				t.Fatalf("fixture: %d mutations committed for a rolled-back transaction", len(recording.record.Mutations))
			}
			if !errors.Is(err, errRolledBackBusiness) {
				t.Fatalf("%s reply=%v: the business error must be kept when the release hook panics after a rollback", tc.name, err)
			}
			if !errors.Is(err, f.hookErr) {
				t.Fatalf("%s reply=%v: the release hook panic must stay on the error chain", tc.name, err)
			}
			if errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("%s reply=%v: nothing was committed, ErrAfterCommitFailed must not be reported", tc.name, err)
			}
		})
	}
}
