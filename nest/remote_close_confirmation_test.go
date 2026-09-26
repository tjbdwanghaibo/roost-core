package nest

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

type closeFailBatch struct{ stagedRemoteBatch }

func (b *closeFailBatch) Close(context.Context) error {
	b.closed.Add(1)
	return entity.ErrRemoteReleaseIncomplete
}

func TestRemoteCloseFailureStillConfirmsEntitySync(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			localID, e := newAsyncPilotEntity(t, 9950, 10)
			m, frames, _ := syncTestManager(t, e, mode)
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, 9951, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			fail := true
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				if fail {
					return &closeFailBatch{}, nil
				}
				return &stagedRemoteBatch{}, nil
			}}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithEntitySync(m), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName("remote_close_confirmation")
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				e.dao.Value++
				e.MarkSyncDirty(1)
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			send := func() any {
				msg, ch := GenSyncMsg(MsgTypeMulti)
				msg.Name = name.String()
				msg.Tids = []int64{remoteID, localID}
				msg.Cost = true
				msg.HasRemote = true
				if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
					t.Fatal(err)
				}
				return stagedWait(t, ch)
			}
			got := send()
			t.Logf("first reply (commit ok, close failed): %v", got)
			if !e.Sync().SyncCommitReady() {
				t.Fatal("committed transaction left Sync held after Close failure")
			}
			fail = false
			got = send()
			t.Logf("second reply (all ok): %v", got)
			_ = m.Flush(context.Background())
			t.Logf("after successful second tx: SyncCommitReady=%v value=%d", e.Sync().SyncCommitReady(), e.dao.Value)
			select {
			case raw := <-frames:
				t.Logf("frame delivered value=%d", syncValue(t, raw))
			case <-time.After(300 * time.Millisecond):
				t.Errorf("no frame delivered after a later successful transaction: entity sync frozen")
			}
		})
	}
}
