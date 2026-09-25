package entitysync

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// 等待新快照不等于客户端没有旧对象：换 profile、撤回退订都会进入这个状态。
// 只要旧对象已交付，最终退订或退役就必须向客户端发送对应 ObjectRef 的 remove。
func TestPendingSnapshotStillRemovesAnAlreadyDeliveredObject(t *testing.T) {
	for _, transition := range []string{"profile_change", "resubscribe"} {
		for _, removal := range []string{"unsubscribe", "unregister"} {
			t.Run(transition+"/"+removal, func(t *testing.T) {
				transport := newRecordingTransport()
				manager := newTestManager(t, transport, ManagerConfig{})
				packs := 0
				state := testSubject(t, 1101, &packs)
				if err := manager.Register(state); err != nil {
					t.Fatal(err)
				}
				open(t, manager, 1, 2)
				mustSubscribe(t, manager, 1, 1101, entity.SyncProfile{})
				mustFlush(t, manager)
				created := oneFrame(t, transport, 1)
				ref := created.wire.Objects[0].Ref

				profile := entity.SyncProfile{Key: "far", LOD: 1}
				if transition == "resubscribe" {
					if err := manager.Unsubscribe(1, 1101); err != nil {
						t.Fatal(err)
					}
					profile = entity.SyncProfile{}
				}
				mustSubscribe(t, manager, 1, 1101, profile)
				// 会话 2 同样等待快照，但从未收到对象，不应收到删除通知。
				mustSubscribe(t, manager, 2, 1101, profile)
				if removal == "unregister" {
					if err := manager.Unregister(1101); err != nil {
						t.Fatal(err)
					}
				} else {
					for _, sid := range []SessionID{1, 2} {
						if err := manager.Unsubscribe(sid, 1101); err != nil {
							t.Fatal(err)
						}
					}
				}
				mustFlush(t, manager)
				removed := oneFrame(t, transport, 1)
				if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != ref {
					t.Fatalf("remove must name the delivered object %v, got %+v", ref, removed.wire.Objects)
				}
				if got := transport.take(2); len(got) != 0 {
					t.Fatalf("a receiver with no object got %d frames", len(got))
				}
				if got := manager.Subscribers(1101); len(got) != 0 {
					t.Fatalf("subscriptions remain after removal: %v", got)
				}
				if removal == "unregister" && manager.Stats().Subjects != 0 {
					t.Fatal("the retired subject is still registered")
				}
			})
		}
	}
}
