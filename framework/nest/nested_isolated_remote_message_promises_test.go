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

// RR-20260926-75：带 Remote 批次的消息里，handler 内嵌套的独立事务（RunIsolatedTransaction）直接返回
// ErrNestedTransactionInRemoteMessage：不执行、不 finalize、不改 remoteFinalized / remoteCommitted，Remote 批次只随消息自己的事务
// Commit 或 Abort。旧行为（REPRO-2026-09-26-07 §1 探针 3b）：批次 FinalizeLocked 收到嵌套事务的 outcome，外层自己的 finalize 被跳过，
// 嵌套提交置 remoteCommitted；外层失败时批次仍 Commit=1、Abort=0，回复误带 ErrAfterCommitFailed，外层自己的提交数为 0。
func TestNestedIsolatedInRemoteMessageIsRefused(t *testing.T) {
	for i, tc := range []struct {
		name       string
		meta       HandlerMeta
		outerFails bool
	}{
		{"memory_outer_fails", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, true},
		{"memory_outer_succeeds", HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}, false},
		{"state_strict_outer_fails", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 44000+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 44001+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &recordingRemoteBatch{}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			outer, iso := &recordsCommitter{}, &recordsCommitter{}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(outer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr75_outer_%d", i))
			var isoErr error
			var isoRuns atomic.Int64
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				_, isoErr = RunIsolatedTransaction(context.Background(), iso, "rr75_iso_in_remote", func() (any, error) {
					isoRuns.Add(1)
					old := e.dao.Value
					RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
					e.dao.Value = 77
					return nil, MarkPersist(e.dao, 1)
				})
				if tc.outerFails {
					return nil, errors.New("outer business failed after the isolated call")
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
			if slices.Contains(finalized, "rr75_iso_in_remote") || iso.count() != 0 || isoRuns.Load() != 0 {
				t.Fatalf("nested isolated transaction went through the message's Remote batch: FinalizeLocked handlers=%v isolatedRuns=%d isolatedCommits=%d isoErr=%v reply=%v",
					finalized, isoRuns.Load(), iso.count(), isoErr, reply)
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
			if !errors.Is(isoErr, ErrNestedTransactionInRemoteMessage) {
				t.Fatalf("RunIsolatedTransaction in a Remote message returned %v, want ErrNestedTransactionInRemoteMessage", isoErr)
			}
			if e.dao.Value != 10 {
				t.Fatalf("value=%d, want 10: the refused isolated call must not run", e.dao.Value)
			}
		})
	}
}
