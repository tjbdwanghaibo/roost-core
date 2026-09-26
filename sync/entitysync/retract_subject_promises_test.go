package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-35：事务内新建的实体被撤销时，Nest 经 RetractSyncSubject 注销它。
// 已持有对象的会话先收到 ObjectRemove，未持有的订阅直接移除；状态随后关闭也不算 Flush 失败；
// 退役完成前同 ID 不能重新登记（remove-before-create）。只撤回同一个状态对象。
func TestRetractSyncSubjectRemovesHeldObjectsBeforeRecreate(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1201, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1201, entity.SyncProfile{})
	mustFlush(t, manager)
	created := oneFrame(t, transport, 1)
	ref := created.wire.Objects[0].Ref
	mustSubscribe(t, manager, 2, 1201, entity.SyncProfile{}) // 会话 2 尚未收到对象

	other := testSubject(t, 1201, &packs)
	manager.RetractSyncSubject(other) // 不同的状态对象：不是这次登记，不能误撤
	if got := manager.Subscribers(1201); len(got) != 2 {
		t.Fatalf("retracting a foreign state touched the subject: %v", got)
	}

	manager.RetractSyncSubject(state)
	state.Close() // 撤销方随后关闭实体同步状态
	if err := manager.Register(other); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("re-register before the remove went out: %v", err)
	}
	mustFlush(t, manager)
	removed := oneFrame(t, transport, 1)
	if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != ref {
		t.Fatalf("remove must name the delivered object %v, got %+v", ref, removed.wire.Objects)
	}
	if got := transport.take(2); len(got) != 0 {
		t.Fatalf("a session that never held the object got %d frames", len(got))
	}
	if err := manager.Register(other); err != nil {
		t.Fatalf("re-register after the remove: %v", err)
	}
}
