package entitysync

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

type settlementTransport struct {
	*recordingTransport
	after func()
}

func (r *settlementTransport) Push(ctx context.Context, sid SessionID, data []byte) error {
	err := r.recordingTransport.Push(ctx, sid, data)
	if r.after != nil {
		f := r.after
		r.after = nil
		f()
	}
	return err
}

// 发送后的结算失败不得让健康实体重发旧基线增量。
func TestCloseDuringDeliveryKeepsOtherSubjectBaseline(t *testing.T) {
	tr := &settlementTransport{recordingTransport: newRecordingTransport()}
	m := newTestManager(t, tr, ManagerConfig{})
	packs := 0
	live := testSubject(t, 7101, &packs)
	closing := testSubject(t, 7102, &packs)
	for _, s := range []*entity.SubjectSyncState{live, closing} {
		if err := m.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	open(t, m, 1)
	mustSubscribe(t, m, 1, 7101, entity.SyncProfile{})
	mustSubscribe(t, m, 1, 7102, entity.SyncProfile{})
	mustFlush(t, m)
	tr.take(1)
	live.MarkDirty(1)
	closing.MarkDirty(1)
	tr.after = closing.Close
	err := m.Flush(context.Background())
	if !errors.Is(err, entity.ErrSubjectSyncStalePrepare) {
		t.Fatalf("want stale batch, got %v", err)
	}
	delivered := oneFrame(t, tr.recordingTransport, 1).updates[7101]
	live.MarkDirty(2)
	mustFlush(t, m)
	retry := oneFrame(t, tr.recordingTransport, 1).updates[7101]
	if !retry.Full && retry.BaseVersion != delivered.Version {
		t.Fatalf("healthy subject received invalid delta: client=%d retry base=%d version=%d full=%v", delivered.Version, retry.BaseVersion, retry.Version, retry.Full)
	}
}

// 恢复不依赖下一次 setter；其他会话、重绑和同 ID 新连接也必须正常推进。
func TestSettlementFailureRecoveryLifecycles(t *testing.T) {
	for _, mode := range []string{"unchanged", "rebind", "reopen", "remove"} {
		t.Run(mode, func(t *testing.T) {
			tr := &settlementTransport{recordingTransport: newRecordingTransport()}
			m := newTestManager(t, tr, ManagerConfig{})
			packs := 0
			live := testSubject(t, 7201, &packs)
			closing := testSubject(t, 7202, &packs)
			for _, state := range []*entity.SubjectSyncState{live, closing} {
				if err := m.Register(state); err != nil {
					t.Fatal(err)
				}
			}
			open(t, m, 1, 2)
			for _, sid := range []SessionID{1, 2} {
				mustSubscribe(t, m, sid, 7201, entity.SyncProfile{})
				mustSubscribe(t, m, sid, 7202, entity.SyncProfile{})
			}
			mustFlush(t, m)
			tr.take(1)
			tr.take(2)
			live.MarkDirty(1)
			closing.MarkDirty(1)
			tr.after = func() {
				closing.Close()
				switch mode {
				case "rebind":
					if err := m.Rebind(testSubject(t, 7202, &packs)); err != nil {
						t.Fatal(err)
					}
				case "reopen":
					m.CloseSession(1)
					open(t, m, 1)
					mustSubscribe(t, m, 1, 7201, entity.SyncProfile{})
				case "remove":
					if err := m.Unregister(7202); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := m.Flush(context.Background()); !errors.Is(err, entity.ErrSubjectSyncStalePrepare) {
				t.Fatalf("want stale batch: %v", err)
			}
			delivered := oneFrame(t, tr.recordingTransport, 2)
			tr.take(1)
			mustFlush(t, m)
			for _, sid := range []SessionID{1, 2} {
				recovered := oneFrame(t, tr.recordingTransport, sid)
				update, ok := recovered.updates[7201]
				if !ok || !update.Full || update.Version != live.Version() {
					t.Fatalf("session %d did not recover full: %+v", sid, update)
				}
				if sid == 2 && recovered.wire.BaseTick != delivered.wire.Tick {
					t.Fatal("healthy session frame clock rolled back")
				}
				if mode == "rebind" && !recovered.updates[7202].Full {
					t.Fatal("rebound subject must use full")
				}
			}
			if live.PendingDirty() {
				t.Fatal("successful recovery did not settle dirty state")
			}
			live.MarkDirty(2)
			mustFlush(t, m)
			for _, sid := range []SessionID{1, 2} {
				update := oneFrame(t, tr.recordingTransport, sid).updates[7201]
				if update.Full || update.Version != update.BaseVersion+1 {
					t.Fatalf("session %d did not resume delta: %+v", sid, update)
				}
			}
			if m.Stats().SessionsLost != 0 {
				t.Fatal("recovery lost a healthy session")
			}
		})
	}
}
