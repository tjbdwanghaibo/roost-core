package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
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

// OPEN-ITEMS B41 → RR-20260927-28：RetractSyncSubject(state) 只撤回以 state 登记的那一次。取到 subject 之后、注销生效之前，
// 同 ID 换成另一个状态登记（旧状态关闭后 Rebind 到新状态；或旧登记被注销、忘掉，新状态重新登记为新 subject）时，
// 撤回不能落到新登记上：新登记保持登记、订阅者不被退役。窗口由 retractLookedUp 测试缝确定性进入。
func TestRetractSyncSubjectDoesNotRetireALaterRegistration(t *testing.T) {
	for _, shape := range []string{"rebound_to_new_state", "forgotten_then_registered_again"} {
		t.Run(shape, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			open(t, manager, 1)
			const id = 1211
			first := labelledSubject(id, "first")
			if err := manager.Register(first); err != nil {
				t.Fatal(err)
			}
			second := labelledSubject(id, "second")
			windowSeen := false
			manager.retractLookedUp = func(int64) {
				manager.retractLookedUp = nil
				windowSeen = true
				switch shape {
				case "rebound_to_new_state":
					first.Close() // 旧状态关闭，同 ID 接到新状态上（同一个 subject）
					if err := manager.Rebind(second); err != nil {
						t.Fatalf("premise: rebind to the new state: %v", err)
					}
				case "forgotten_then_registered_again":
					// 旧登记被注销；没有会话持有对象，随即被忘掉，新状态登记为新 subject
					if err := manager.Unregister(id); err != nil {
						t.Fatalf("premise: unregister the old registration: %v", err)
					}
					if manager.subject(id) != nil {
						t.Fatal("premise: the old subject was not forgotten")
					}
					if err := manager.Register(second); err != nil {
						t.Fatalf("premise: register the new state: %v", err)
					}
				}
				mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
			}
			manager.RetractSyncSubject(first)
			if !windowSeen {
				t.Fatal("premise: the window between lookup and unregister was not entered")
			}
			subj := manager.subject(id)
			if subj == nil {
				t.Fatalf("RetractSyncSubject(first) retired the registration of the second state: subject %d is gone", id)
			}
			subj.mu.Lock()
			current, retiring := subj.state, subj.retiring
			subj.mu.Unlock()
			if current != second || retiring {
				t.Fatalf("RetractSyncSubject(first) retired the registration of the second state: state is second=%v, retiring=%v", current == second, retiring)
			}
			mustFlush(t, manager)
			if got := manager.Subscribers(id); len(got) != 1 || got[0] != 1 {
				t.Fatalf("session 1's subscription to the second registration was dropped: %v", got)
			}
		})
	}
}
