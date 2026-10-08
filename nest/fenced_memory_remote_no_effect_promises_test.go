package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260930-12（REMAINING §3 N21，维护者 2026-09-30 拍板“收紧”）：引擎 fence 之后，带 Remote 批次、没有 effect 的 memory handler
// 的 Durability 0 Remote 直写同样被拒绝。承诺：这样的消息在 fence 之后回复满足 errors.Is(err, ErrNestFenced) 与 ErrCommitRejected
// （判别表第 12 行），Remote 批次没有 Commit、至少一次 Abort，committer 不被调用；RollbackNone 的内存修改不撤销（与 RR-20260927-32 同）。
// 旧行为：durableCommit 对“memory 且无 effect”直接返回 nil、跳过 refuseCommitAfterFence（RR-20260927-06 只拦交给 committer 的记录），
// remoteCommitted 置位后 finishRemoteWriteBatch 照常 batch.Commit——别的消息 fence 引擎之后，它仍把权威写了（RR-20260927-32 §未验证项）。
func TestFencedMemoryRemoteWithoutEffectIsRefused(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 49700, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 49701, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	recording := &recordingRemoteBatch{}
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return recording, nil }}
	committer := &recordsCommitter{}
	var mgr *NestMgr
	mgr = NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
	name := NewHandlerName("rr20260930_12_memory_remote_no_effect")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		e.dao.Value = 77
		_ = MarkPersist(e.dao, 1)
		// 没有 Emit：记录不交给 committer。别的原因 fence 引擎（模拟另一条消息结果未知）。
		mgr.Fence(errors.New("rr12: fenced by another message"))
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
	reply := stagedWait(t, ch)
	err, _ := reply.(error)
	t.Logf("reply=%v committer=%d remote commits=%d aborts=%d value=%d", reply, committer.count(), recording.commits.Load(), recording.aborts.Load(), e.dao.Value)
	if c := recording.commits.Load(); c != 0 {
		t.Fatalf("Remote batch was committed %d time(s) after the engine was fenced: a Durability 0 write reached the authority behind the fence", c)
	}
	if !errors.Is(err, ErrNestFenced) || !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("reply=%v, want errors.Is ErrNestFenced and ErrCommitRejected (decision table row 12)", reply)
	}
	for _, committed := range []error{ErrCommitIndeterminate, ErrAfterCommitFailed, ErrNestedTransactionCommitted, ErrRemotePartRejected} {
		if errors.Is(err, committed) {
			t.Fatalf("reply %v must not claim a (possible) commit via %v", err, committed)
		}
	}
	if a := recording.aborts.Load(); a == 0 {
		t.Fatal("Remote batch was not aborted")
	}
	if n := committer.count(); n != 0 {
		t.Fatalf("committer received %d record(s)", n)
	}
	if e.dao.Value != 77 {
		t.Fatalf("declared entity value=%d, want 77: RollbackNone does not undo in-memory changes", e.dao.Value)
	}
}
