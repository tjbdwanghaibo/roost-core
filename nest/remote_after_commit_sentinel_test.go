package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// strictConfirmTimeoutBatch 模拟 remoteentity 在 Durability 2 下确认等待超时：本地事务已持久，
// Remote 结果未知（remoteWriteBatch.Commit 标 indeterminate 后返回错误，Close 交给 finalizer）。
type strictConfirmTimeoutBatch struct{ closeFailBatch }

func (b *strictConfirmTimeoutBatch) Commit(context.Context) ([]entity.RemoteCommitReceipt, error) {
	return nil, fmt.Errorf("%w: %w", entity.ErrRemotePersistenceIndeterminate, context.DeadlineExceeded)
}

// RR-20260926-46：提交之后发生的释放 / 回调错误必须带 ErrAfterCommitFailed（保留原因的 errors.Is 链），
// 未提交（Abort）或结果未知的回复不得带该哨兵。REPRO-2026-09-26-04 §11 探针改写。
func TestRemoteReplyDistinguishesCommittedFromUncommitted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unique    int64
		batch     func() entity.RemoteWriteBatch
		fail      bool
		committed bool
		cause     error
	}{
		{name: "committed then close failed", unique: 9420, batch: func() entity.RemoteWriteBatch { return &closeFailBatch{} }, committed: true, cause: entity.ErrRemoteReleaseIncomplete},
		{name: "handler failed then close failed", unique: 9430, batch: func() entity.RemoteWriteBatch { return &closeFailBatch{} }, fail: true, cause: entity.ErrRemoteReleaseIncomplete},
		{name: "remote confirmation unknown", unique: 9440, batch: func() entity.RemoteWriteBatch { return &strictConfirmTimeoutBatch{} }, cause: entity.ErrRemotePersistenceIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, tc.unique, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, tc.unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := tc.batch()
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName(fmt.Sprintf("remote_sentinel_%d", tc.unique))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if tc.fail {
					return nil, errors.New("business refused")
				}
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			reply := stagedWait(t, ch)
			err, _ := reply.(error)
			if err == nil {
				t.Fatalf("reply=%v, want an error", reply)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("reply=%v lost its cause %v", err, tc.cause)
			}
			if got := errors.Is(err, ErrAfterCommitFailed); got != tc.committed {
				t.Fatalf("reply=%v: errors.Is(ErrAfterCommitFailed)=%v, want %v (committed=%v)", err, got, tc.committed, tc.committed)
			}
		})
	}
}

// 提交之后 release hook panic（RR-32 场景）也是提交后错误：回复必须带哨兵，并保留 hook 原因。
func TestRemoteReleaseHookPanicAfterCommitCarriesSentinel(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 9450, 10)
	owner := entity.NewEntityManager()
	if err := owner.TryAdd(e); err != nil {
		t.Fatal(err)
	}
	hookErr := errors.New("release hook failed")
	armed := false
	defer owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
		if armed {
			armed = false
			panic(hookErr)
		}
	})()
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 9451, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	batch := &stagedRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("remote_sentinel_release_hook")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		old := e.dao.Value
		RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
		e.dao.Value++
		armed = true
		return "ok", MarkPersist(e.dao, 1)
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name, msg.Tids, msg.Cost, msg.HasRemote = name.String(), []int64{remoteID, localID}, true, true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	reply := stagedWait(t, ch)
	err, _ := reply.(error)
	if len(committer.record.Mutations) == 0 || batch.aborted.Load() {
		t.Fatalf("setup: transaction not durably committed or aborted: reply=%v", reply)
	}
	if err == nil || !errors.Is(err, hookErr) {
		t.Fatalf("reply=%v, want the release hook cause", reply)
	}
	if !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("reply=%v: committed transaction with a post-commit release failure lacks ErrAfterCommitFailed", err)
	}
}
