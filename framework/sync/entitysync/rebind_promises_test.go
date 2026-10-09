package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR-20260926-30：DataEngine 在原生步骤投影被 lease fence 跳过后驱逐内存实体，实体的同步状态随之关闭；
// 订阅者已经收到了含被跳过效果的增量。实体从 Mongo 重新加载后是一份新的内容状态、新的版本链。
// 承诺：关闭期间不捕获、不重试、不计失败；重新绑定后每个订阅者收到整份全量（已持有对象的是
// ObjectUpdate + Full，对象引用不变），之后的增量从这份全量的版本继续，而不是接着旧状态续发。

func labelledSubject(id int64, label string) *entity.SubjectSyncState {
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("snapshot:"+label)), nil
			},
			Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("delta:"+label)), nil
			},
		},
	})
}

func TestReloadedSubjectIsRebound(t *testing.T) {
	for _, via := range []string{"Rebind", "Register"} {
		t.Run(via, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			evicted := labelledSubject(3001, "phantom")
			if err := manager.Register(evicted); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 3001, entity.SyncProfile{})
			mustFlush(t, manager)
			created := oneFrame(t, transport, 1)
			evicted.MarkDirty(0x1) // 被跳过的效果已经作为增量发给了客户端
			mustFlush(t, manager)
			oneFrame(t, transport, 1)

			evicted.Close() // 驱逐：EntityBase.ClearBase 关闭同步状态
			evicted.MarkDirty(0x2)
			mustFlush(t, manager)
			if frames := transport.take(1); len(frames) != 0 {
				t.Fatalf("a closed subject still sent %d frame(s)", len(frames))
			}
			if err := manager.LastError(); err != nil {
				t.Fatalf("a closed subject counted as a sync failure: %v", err)
			}

			reloaded := labelledSubject(3001, "mongo")
			var err error
			if via == "Rebind" {
				err = manager.Rebind(reloaded)
			} else {
				err = manager.Register(reloaded)
			}
			if err != nil {
				t.Fatalf("%s the reloaded state: %v", via, err)
			}
			mustFlush(t, manager)
			full := oneFrame(t, transport, 1)
			update := full.updates[3001]
			if full.objects[3001] != frame.ObjectUpdate || !update.Full || string(update.Payload.BytesCopy()) != "snapshot:mongo" {
				t.Fatalf("rebind did not resend a full snapshot of the reloaded state: objects=%v update=%+v payload=%q", full.objects, update, update.Payload.BytesCopy())
			}
			if created.wire.Objects[0].Ref != full.wire.Objects[0].Ref {
				t.Fatalf("the object changed reference across the rebind: %v → %v", created.wire.Objects[0].Ref, full.wire.Objects[0].Ref)
			}
			reloaded.MarkDirty(0x4)
			mustFlush(t, manager)
			delta := oneFrame(t, transport, 1).updates[3001]
			if delta.Full || delta.BaseVersion != update.Version || string(delta.Payload.BytesCopy()) != "delta:mongo" {
				t.Fatalf("delta after the rebind does not continue from the full snapshot: full=%+v delta=%+v", update, delta)
			}
		})
	}
}

func TestRebindRefusesToReplaceALiveState(t *testing.T) {
	manager := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	live := labelledSubject(3002, "live")
	if err := manager.Rebind(live); !errors.Is(err, ErrSubjectNotRegistered) {
		t.Fatalf("rebind of an unregistered subject=%v", err)
	}
	if err := manager.Register(live); err != nil {
		t.Fatal(err)
	}
	if err := manager.Rebind(live); err != nil {
		t.Fatalf("rebind to the registered state itself=%v, want nil", err)
	}
	if err := manager.Rebind(labelledSubject(3002, "other")); !errors.Is(err, ErrSubjectRegistered) {
		t.Fatalf("rebind over a live state=%v, want ErrSubjectRegistered", err)
	}
	if err := manager.Register(labelledSubject(3002, "other")); !errors.Is(err, ErrSubjectRegistered) {
		t.Fatalf("register over a live state=%v, want ErrSubjectRegistered", err)
	}
}
