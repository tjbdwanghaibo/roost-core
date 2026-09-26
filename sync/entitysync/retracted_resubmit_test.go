package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-70：只有经政策来源（NewSubscriptionSourceWithResubmit）订阅的 pair 会在重新登记时交还政策；
// 直接 Manager.Subscribe 与普通 SubscriptionSource 的订阅没有政策可问，行为不变：重新登记后不自动恢复。
func TestRetractedSubscriptionsWithoutAPolicyAreNotRestored(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2)
	retracted := labelledSubject(3301, "phantom")
	if err := manager.Register(retracted); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, manager, 1, 3301, entity.SyncProfile{})
	if err := manager.NewSubscriptionSource().Subscribe(2, 3301, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)
	retracted.Close()
	if !manager.RetractUnloadedSubject(3301) {
		t.Fatal("premise: retract refused")
	}
	mustFlush(t, manager)
	if err := manager.Register(labelledSubject(3301, "mongo")); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)
	if got := manager.Subscribers(3301); len(got) != 0 {
		t.Fatalf("subscriptions without a policy came back on their own: %v", got)
	}
}

// resubmitRecorder is a minimal policy: it records what the manager hands back and, like a real policy,
// subscribes again from inside the callback (Flush's policy phase, no manager lock held).
type resubmitRecorder struct {
	source  *SubscriptionSource
	batches [][]RetractedSubscription
	keep    func(RetractedSubscription) bool
}

func newResubmitRecorder(manager *Manager) *resubmitRecorder {
	r := &resubmitRecorder{keep: func(RetractedSubscription) bool { return true }}
	r.source = manager.NewSubscriptionSourceWithResubmit(func(batch []RetractedSubscription) {
		r.batches = append(r.batches, batch)
		for _, subscription := range batch {
			if r.keep(subscription) {
				_ = r.source.Subscribe(subscription.Session, subscription.Subject, entity.SyncProfile{})
			}
		}
	})
	return r
}

func retractWithPolicy(t *testing.T, manager *Manager, recorder *resubmitRecorder, id int64, sessions ...SessionID) *entity.SubjectSyncState {
	t.Helper()
	state := labelledSubject(id, "phantom")
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if err := recorder.source.Subscribe(session, id, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	mustFlush(t, manager)
	state.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}
	return state
}

// 交还的时机与次数：撤销后、重新登记前不交还；登记后在下一次 Flush 的政策阶段交还一次（同一 Flush 内的重新订阅
// 就被捕获，订阅者收到 create）；之后不再重复。
func TestRetractedPolicySubscriptionsAreHandedBackOnceAfterReRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1, 2)
	recorder := newResubmitRecorder(manager)
	retractWithPolicy(t, manager, recorder, 3302, 1, 2)
	mustFlush(t, manager) // removes
	mustFlush(t, manager)
	transport.take(1)
	transport.take(2)
	if len(recorder.batches) != 0 {
		t.Fatalf("handed back before the subject was registered again: %v", recorder.batches)
	}
	if err := manager.Register(labelledSubject(3302, "mongo")); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batches) != 0 {
		t.Fatalf("handed back on the Register call path instead of the policy phase: %v", recorder.batches)
	}
	if !manager.policiesPending() {
		t.Fatal("a pending hand-back is not reported to Drain")
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || len(recorder.batches[0]) != 2 {
		t.Fatalf("hand-back batches=%v, want one batch with both sessions", recorder.batches)
	}
	for _, session := range []SessionID{1, 2} {
		if created := oneFrame(t, transport, session); created.objects[3302] != frame.ObjectCreate {
			t.Fatalf("session %d after the resubmit=%v, want a create in the same flush", session, created.objects)
		}
	}
	mustFlush(t, manager)
	if len(recorder.batches) != 1 || manager.policiesPending() {
		t.Fatalf("handed back more than once: %v", recorder.batches)
	}
}

// 来源在缺席期间自己释放了 pair（Unsubscribe 得到 ErrSubjectNotRegistered），那条不再交还；其余照常交还。
// 业务 Unregister 该 ID（退役中）则全部不再交还。
func TestReleasedOrUnregisteredRetractionsAreNotHandedBack(t *testing.T) {
	t.Run("released by its source", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1, 2)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3303, 1, 2)
		mustFlush(t, manager)
		if err := recorder.source.Unsubscribe(1, 3303); !errors.Is(err, ErrSubjectNotRegistered) {
			t.Fatalf("releasing a pair of an absent subject = %v, want ErrSubjectNotRegistered", err)
		}
		if err := manager.Register(labelledSubject(3303, "mongo")); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 1 || len(recorder.batches[0]) != 1 || recorder.batches[0][0] != (RetractedSubscription{Session: 2, Subject: 3303}) {
			t.Fatalf("hand-back=%v, want only session 2 (session 1's pair was released)", recorder.batches)
		}
		if got := manager.Subscribers(3303); len(got) != 1 || got[0] != 2 {
			t.Fatalf("subscribers after the hand-back=%v, want [2]", got)
		}
	})
	t.Run("released while the removes are still owed", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3304, 1)
		if err := recorder.source.Unsubscribe(1, 3304); !errors.Is(err, ErrSubscriptionNotFound) {
			t.Fatalf("releasing a retracted pair while retiring = %v, want ErrSubscriptionNotFound", err)
		}
		mustFlush(t, manager)
		if err := manager.Register(labelledSubject(3304, "mongo")); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 0 {
			t.Fatalf("a released pair was handed back: %v", recorder.batches)
		}
	})
	t.Run("business unregister", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3305, 1)
		if err := manager.Unregister(3305); err != nil {
			t.Fatal(err)
		}
		mustFlush(t, manager)
		if queued, err := manager.RegisterAfterRetirement(labelledSubject(3305, "mongo"), nil); err != nil || queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 0 {
			t.Fatalf("handed back after the business unregistered the subject: %v", recorder.batches)
		}
	})
	t.Run("queued registration behind the retraction", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		recorder := newResubmitRecorder(manager)
		retractWithPolicy(t, manager, recorder, 3306, 1)
		transport.take(1)                                                      // 撤销前的 create
		if err := manager.Rebind(labelledSubject(3306, "mongo")); err != nil { // kit OnEntityLoaded：排到 remove 之后
			t.Fatal(err)
		}
		mustFlush(t, manager) // remove 交付 → forget → 登记排队的状态
		if removed := oneFrame(t, transport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
			t.Fatalf("retraction frame=%+v, want ObjectRemove", removed.wire.Objects)
		}
		mustFlush(t, manager)
		if len(recorder.batches) != 1 {
			t.Fatalf("hand-back after the queued registration=%v, want one", recorder.batches)
		}
		if created := oneFrame(t, transport, 1); created.objects[3306] != frame.ObjectCreate {
			t.Fatalf("resubmitted subscription=%v, want a create after the remove", created.objects)
		}
	})
}
