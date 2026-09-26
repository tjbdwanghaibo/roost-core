package entitysync

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-69：退役收尾（forget）把 subject 从表里摘下之后、清除旧 subject 装在状态上的脏通知器之前，
// 同一状态对象被 RegisterAfterRetirement 直接登记（subject 已不在表里，queued=false）。旧实现随后的
// SetDirtyNotifier(nil) 清掉的是新 subject 的通知器：登记“成功”，此后 MarkDirty 不再进入 pending，
// 观察者收不到后续变化。forgetUnlinked 测试缝让登记确定性地落在这个窗口里，不靠 sleep 或放大轮数。
func TestRegistrationInTheForgetWindowKeepsItsDirtyNotifier(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	const id = int64(1401)
	state := retiringSubject(t, manager, transport, id, &packs)

	var results registrationResults
	var queued bool
	var regErr error
	windows := 0
	manager.forgetUnlinked = func(subjectID int64) {
		if subjectID != id || windows > 0 {
			return
		}
		windows++
		// 退役刚完成、subject 已离表：业务此刻的登记直接走 Register。
		queued, regErr = manager.RegisterAfterRetirement(state, results.done)
	}
	mustFlush(t, manager) // 交付 remove → forget
	manager.forgetUnlinked = nil
	if windows != 1 {
		t.Fatalf("premise: the forget window was reached %d time(s), want 1", windows)
	}
	if regErr != nil || queued {
		t.Fatalf("premise: registration in the forget window = (queued=%v, err=%v), want a direct registration", queued, regErr)
	}
	if removed := oneFrame(t, transport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("retirement frame=%+v, want ObjectRemove", removed.wire.Objects)
	}
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done called for a registration that was not queued: %v", got)
	}
	if subj := manager.subject(id); subj == nil || subj.currentState() != state {
		t.Fatalf("the registration in the forget window is not in place: %v", subj)
	}

	manager.takePending()
	state.MarkDirty(1)
	manager.pendingMu.Lock()
	_, pending := manager.pending[id]
	manager.pendingMu.Unlock()
	if !pending {
		t.Fatalf("registered again in the forget window (queued=false err=nil), but MarkDirty does not schedule it: forget cleared the new subject's dirty notifier")
	}

	// 后续变化照常同步：新订阅先得到 create，之后的 MarkDirty 得到 update。
	mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	mustFlush(t, manager)
	if created := oneFrame(t, transport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("subscription after the re-registration=%v, want a create", created.objects)
	}
	state.MarkDirty(1)
	mustFlush(t, manager)
	if updated := oneFrame(t, transport, 1); updated.objects[id] != frame.ObjectUpdate {
		t.Fatalf("change after the re-registration=%v, want an update", updated.objects)
	}
}

// forget 只忘掉调用方看到的那个 subject：同 ID 已重新登记为新 subject 之后，对旧 subject 迟到的 forget
// （两条收尾路径各看到一次“最后一个订阅者已走”）不删新 subject、不清它的通知器。
func TestALateForgetOfTheOldSubjectLeavesTheNewOneAlone(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	const id = int64(1402)
	retiringSubject(t, manager, transport, id, &packs)
	old := manager.subject(id)
	mustFlush(t, manager)
	oneFrame(t, transport, 1)
	next := testSubject(t, id, &packs)
	if err := manager.Register(next); err != nil {
		t.Fatal(err)
	}
	manager.forget(old)
	if subj := manager.subject(id); subj == nil || subj.currentState() != next {
		t.Fatalf("a late forget of the old subject removed the new one: %v", subj)
	}
	manager.takePending()
	next.MarkDirty(1)
	manager.pendingMu.Lock()
	_, pending := manager.pending[id]
	manager.pendingMu.Unlock()
	if !pending {
		t.Fatal("a late forget of the old subject cleared the new subject's dirty notifier")
	}
}
