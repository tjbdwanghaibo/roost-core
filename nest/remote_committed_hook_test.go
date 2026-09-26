package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestRemoteAfterAdmissionPanicDoesNotAbortDurableCommit(t *testing.T) {
	localID, e := newAsyncPilotEntity(t, 9960, 10)
	getter := newMockGetter()
	remoteID := mustBuildCastID(t, 9961, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
	getter.Add(e)
	batch := &stagedRemoteBatch{}
	var committed bool
	batch.commit = func() { committed = true }
	manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("remote_committed_hook_panic")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		old := e.dao.Value
		RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
		e.dao.Value++
		if err := MarkPersist(e.dao, 1); err != nil {
			return nil, err
		}
		CurrentRollbackTx().AfterAdmission(func() { panic("lifecycle finalizer panic") })
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	msg, ch := GenSyncMsg(MsgTypeMulti)
	msg.Name = name.String()
	msg.Tids = []int64{remoteID, localID}
	msg.Cost = true
	msg.HasRemote = true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	got := stagedWait(t, ch)
	t.Logf("reply=%v", got)
	t.Logf("WAL record committed: mutations=%d; local value=%d; remote batch Commit called=%v Abort called=%v", len(committer.record.Mutations), e.dao.Value, committed, batch.aborted.Load())
	if len(committer.record.Mutations) == 0 || batch.aborted.Load() || !committed {
		t.Errorf("remote batch aborted after the transaction was durably committed")
	}
}

func TestPostRemoteCommitRunsAllCallbacksAfterPanic(t *testing.T) {
	msg := &Msg{}
	called := 0
	msg.addPostRemoteCommit(func() { panic("after commit") }, func() { called++ })
	if err := msg.runPostRemoteCommit(); !errors.Is(err, ErrAfterCommitFailed) {
		t.Fatalf("callback error=%v", err)
	}
	if called != 1 {
		t.Fatal("panic skipped framework completion")
	}
	if err := msg.runPostRemoteCommit(); err != nil || called != 1 {
		t.Fatal("callbacks repeated")
	}
}
