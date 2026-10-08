package nest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260928-08（第七轮审计疑点，OPEN-ITEMS B19 并存形态）：带 Remote 批次的消息，PrepareRemoteWriteBatch 执行期间（批次还没挂上）
// 调用的独立事务已提交，随后消息自己的本地事务也提交，而 Remote Commit 被明确拒绝或结果未知。回复同时带 ErrNestedTransactionCommitted 与
// ErrRemotePartRejected（或 entity.ErrRemotePersistenceIndeterminate），之前外层文本是 ErrNestedTransactionCommitted 的
// “…committed before the message failed”，与内层“local transaction committed …”矛盾。承诺：两个哨兵都照常 errors.Is，
// 外层表述不说“消息失败”；消息自己的本地事务确实已提交（批次收到 Commit、没有 Abort）。

type remotePartOutcomeBatch struct {
	stagedRemoteBatch
	commitErr error
}

func (b *remotePartOutcomeBatch) Commit(ctx context.Context) ([]entity.RemoteCommitReceipt, error) {
	_, _ = b.stagedRemoteBatch.Commit(ctx)
	return nil, b.commitErr
}

func TestNestedCommitInRemotePrepareAlongsideCommittedLocalPartKeepsReplyText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unique    int64
		commitErr error
		sentinel  error
	}{
		{name: "remote_part_rejected", unique: 48620, commitErr: entity.ErrRemoteRejected, sentinel: ErrRemotePartRejected},
		{name: "remote_outcome_unknown", unique: 48622, commitErr: errors.Join(entity.ErrRemotePersistenceIndeterminate, errors.New("remote reply lost")),
			sentinel: entity.ErrRemotePersistenceIndeterminate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, local := newAsyncPilotEntity(t, tc.unique, 1)
			remoteID := mustBuildCastID(t, tc.unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter := newMockGetter()
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(local)
			iso := &recordsCommitter{}
			var isoErr error
			batch := &remotePartOutcomeBatch{commitErr: tc.commitErr}
			var commits atomic.Int32
			batch.commit = func() { commits.Add(1) }
			remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				// 批次还没挂上：独立事务照常执行并提交，不认领消息（RR-20260926-84 复核残留，B19）。
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr08_iso_in_prepare", func() (any, error) {
					return nil, CurrentRollbackTx().AddMutation(EntityMutation{Key: dataengine.DocumentKey{ID: localID, Database: "test", Resource: "rr08_prepare"}, Kind: dataengine.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: []byte(`{}`)})
				})
				return batch, nil
			}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(remote), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName("rr08_nested_alongside_" + tc.name)
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
			err, _ := reply.(error)
			t.Logf("reply=%v", reply)

			if isoErr != nil || iso.count() != 1 {
				t.Fatalf("premise: isolated transaction in PrepareRemoteWriteBatch err=%v commits=%d, want it committed once", isoErr, iso.count())
			}
			if n := commits.Load(); n != 1 || batch.aborted.Load() {
				t.Fatalf("premise: batch commits=%d aborted=%v, want the message's own local transaction committed and the batch committed once", n, batch.aborted.Load())
			}
			if !errors.Is(err, ErrNestedTransactionCommitted) || !errors.Is(err, tc.sentinel) || !errors.Is(err, tc.commitErr) {
				t.Fatalf("reply=%v: want errors.Is ErrNestedTransactionCommitted, %v and the Remote cause", reply, tc.sentinel)
			}
			if errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("reply=%v: the Remote part was not confirmed, must not claim ErrAfterCommitFailed", reply)
			}
			if text := err.Error(); strings.Contains(text, "before the message failed") {
				t.Fatalf("reply text %q says the message failed although its own local transaction committed", text)
			}
		})
	}
}
