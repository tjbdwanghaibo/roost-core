package entitysync

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// OPEN-ITEMS B21：RR-20260926-78 修复记录自报的未验证窗口——register 放开 m.mu 之后、releaseRetracted 交还撤销记录之前，
// 新登记的同 ID subject 又被 RetractUnloadedSubject 撤销。验证：记录不交给这个已退役的 subject（第二次登记后政策收不到
// 任何交还），留在撤销表里；第三次登记后恰好交还一次，订阅者收到 create，之后不留记录。RR-78 之前 releaseRetracted
// 不检查 subject 状态，记录会被交给已退役的 subject、政策的 Subscribe 被拒绝后就此丢失（审计 audit4 探针 E：修前丢、修后
// 交还一次）。窗口由 registeredBeforeHandBack 测试缝确定性进入，不靠 sleep 或放大轮数。
func TestRetractedBetweenRegisterAndHandBackIsHandedBackAfterTheNextRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	const id = 3501
	recorder := newResubmitRecorder(manager)
	retractWithPolicy(t, manager, recorder, id, 1)
	mustFlush(t, manager) // remove → forget
	mustFlush(t, manager)
	transport.take(1)
	if got := manager.resubmitEntries.Load(); got != 1 {
		t.Fatalf("premise: records after the first retraction=%d, want 1", got)
	}

	second := labelledSubject(id, "second")
	retractedInWindow := false
	manager.registeredBeforeHandBack = func(subjectID int64) {
		manager.registeredBeforeHandBack = nil
		// 重载器在登记放开 m.mu 之后、交还之前又把刚登记的 subject 退回 remove。
		second.Close()
		retractedInWindow = manager.RetractUnloadedSubject(subjectID)
	}
	if err := manager.Register(second); err != nil {
		t.Fatal(err)
	}
	if !retractedInWindow {
		t.Fatal("premise: the retraction between register and hand-back was refused")
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if len(recorder.batches) != 0 {
		t.Fatalf("records handed back to a subject retracted before the hand-back: %v", recorder.batches)
	}
	if got := manager.Subscribers(id); len(got) != 0 {
		t.Fatalf("premise: the retracted subject still has subscribers %v", got)
	}
	if got := manager.resubmitEntries.Load(); got != 1 {
		t.Fatalf("records after the retraction in the window=%d, want the one record kept for the next registration", got)
	}
	transport.take(1)

	if err := manager.Register(labelledSubject(id, "third")); err != nil {
		t.Fatalf("third register: %v", err)
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 1 || recorder.batches[0][0] != (RetractedSubscription{Session: 1, Subject: id}) {
		t.Fatalf("hand-back after the third registration=%v, want session 1 exactly once", recorder.batches)
	}
	if got := manager.Subscribers(id); len(got) != 1 || got[0] != 1 {
		t.Fatalf("subscribers after the hand-back=%v, want [1]", got)
	}
	if created := oneFrame(t, transport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("session 1 after the hand-back=%v, want a create", created.objects)
	}
	if manager.resubmitEntries.Load() != 0 || manager.policiesPending() {
		t.Fatalf("records left behind after delivery: %d", manager.resubmitEntries.Load())
	}
}
