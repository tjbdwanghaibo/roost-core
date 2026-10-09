package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260927-32：提交前（写任何持久记录之前）的明确拒绝统一带 ErrCommitRejected。之前只有 committer / Enqueue 的拒绝带它；
// Remote 批次 FinalizeLocked 拒绝（如 RR-20260927-09 的 sid 作用域）、fence 之后不交给 committer（RR-20260927-06）等原样返回，
// USER_GUIDE §4 判别表一行都不命中。承诺：这些回复满足 errors.Is(err, ErrCommitRejected)，原因仍可 errors.Is；committer 不被调用、
// Remote 批次 Abort。同时钉住 RollbackNone 下的实际行为（判别表第 11 行与 RR-06 的表述据此更正）：带 Remote 批次的 memory handler
// 被拒时内存修改不撤销；可回滚事务整条回滚。

var errFinalizeRefused = errors.New("test: remote batch refused at FinalizeLocked")

// finalizeRefusingBatch 在 FinalizeLocked 明确拒绝（与 remoteentity 的 sid 作用域拒绝同一位置：WAL 准入之前）。
type finalizeRefusingBatch struct {
	*recordingRemoteBatch
}

func (b finalizeRefusingBatch) FinalizeLocked(o entity.RemoteTransactionOutcome) error {
	_ = b.recordingRemoteBatch.FinalizeLocked(o)
	return fmt.Errorf("remote_entity: validate commit: %w", errFinalizeRefused)
}

func TestPreCommitRejectionsCarryErrCommitRejected(t *testing.T) {
	memoryRemote := HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory}
	stateStrict := HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}
	for i, tc := range []struct {
		name string
		meta HandlerMeta
		// fence：handler 内 fence 引擎（别的原因），提交点按 RR-06 拒绝；否则 Remote 批次在 FinalizeLocked 拒绝。
		fence bool
		cause error
		// wantValue：声明实体（初值 10，handler 改成 77）在回复之后的值。
		wantValue int
	}{
		{name: "finalize_refused/state_strict", meta: stateStrict, cause: errFinalizeRefused, wantValue: 10},
		{name: "finalize_refused/memory_remote", meta: memoryRemote, cause: errFinalizeRefused, wantValue: 77},
		{name: "fenced/state_strict", meta: stateStrict, fence: true, cause: ErrNestFenced, wantValue: 10},
		// memory handler 带 Remote 批次、emit 了 effect（升级为 strict，记录要交给 committer）：RR-06 的拒绝不撤销内存修改。
		{name: "fenced/memory_remote_with_effect", meta: memoryRemote, fence: true, cause: ErrNestFenced, wantValue: 77},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 49600+int64(i)*10, 10)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 49601+int64(i)*10, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			recording := &recordingRemoteBatch{}
			var batch entity.RemoteWriteBatch = recording
			if !tc.fence {
				batch = finalizeRefusingBatch{recording}
			}
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			committer := &recordsCommitter{}
			var mgr *NestMgr
			mgr = NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName(fmt.Sprintf("rr20260927_32_%d", i))
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				e.dao.Value = 77
				if err := MarkPersist(e.dao, 1); err != nil && tc.meta.Rollback != RollbackNone {
					return nil, err
				}
				if tc.fence {
					if tc.meta.Rollback == RollbackNone {
						if err := Emit(Effect{Topic: "rr32.effect", Payload: []byte("x")}); err != nil {
							return nil, err
						}
					}
					mgr.Fence(errors.New("rr32: fenced by another message"))
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
			err, _ := reply.(error)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("reply=%v, want errors.Is the rejection cause %v", reply, tc.cause)
			}
			if !errors.Is(err, ErrCommitRejected) {
				t.Fatalf("a rejection before any durable record must carry ErrCommitRejected (decision table row 12 since RR-20260928-03), reply=%v", err)
			}
			for _, committed := range []error{ErrCommitIndeterminate, ErrAfterCommitFailed, ErrNestedTransactionCommitted} {
				if errors.Is(err, committed) {
					t.Fatalf("reply %v must not claim a (possible) commit via %v", err, committed)
				}
			}
			if n := committer.count(); n != 0 {
				t.Fatalf("committer received %d record(s) for a rejected transaction", n)
			}
			// 锁内拒绝时先 Abort 一次，收尾（finishRemoteWriteBatch）再 Abort 一次；Abort 幂等，这里只要求没有 Commit、至少一次 Abort。
			if c, a := recording.commits.Load(), recording.aborts.Load(); c != 0 || a == 0 {
				t.Fatalf("Remote batch Commit=%d Abort=%d, want no Commit and an Abort", c, a)
			}
			if e.dao.Value != tc.wantValue {
				t.Fatalf("declared entity value=%d after the rejection, want %d (rollback=%s)", e.dao.Value, tc.wantValue, tc.meta.Rollback)
			}
		})
	}
}
