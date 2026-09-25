package entitysync

import (
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// 同一 tick 用一个新对象替换一个旧对象，最终容量未增长；准入结果不应由 ID 大小决定。
func TestFullSessionCanReplaceAnObjectRegardlessOfSubjectID(t *testing.T) {
	for _, ids := range [][2]int64{{100, 1}, {1, 100}} {
		t.Run(fmt.Sprintf("%d_to_%d", ids[0], ids[1]), func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{Limits: frame.Limits{MaxObjects: 1}})
			packs := 0
			for _, id := range ids {
				if err := manager.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, ids[0], entity.SyncProfile{})
			mustFlush(t, manager)
			original := oneFrame(t, transport, 1)
			if err := manager.Unsubscribe(1, ids[0]); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, manager, 1, ids[1], entity.SyncProfile{})
			mustFlush(t, manager)
			payloads := transport.take(1)
			if len(payloads) != 2 || manager.Stats().SessionsLost != 0 {
				t.Fatalf("legal replacement got %d frames, stats %+v", len(payloads), manager.Stats())
			}
			removed := decodeFrame(t, payloads[0])
			created := decodeFrame(t, payloads[1])
			if removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != original.wire.Objects[0].Ref {
				t.Fatalf("first frame must release the old object: %+v", removed.wire.Objects)
			}
			if created.objects[ids[1]] != frame.ObjectCreate || created.wire.BaseTick != removed.wire.Tick {
				t.Fatalf("replacement did not continue the client's frame history: %+v", created.wire)
			}
			if created.wire.Objects[0].Ref == original.wire.Objects[0].Ref {
				t.Fatal("a reused reference must advance its generation")
			}
		})
	}
}
