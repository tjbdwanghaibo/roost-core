package entitysync

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20260926-85：释放通知带戳（ReleasedSubscription.Stamp），与 SubscribeStamped 同一时钟。承诺：这一次会话打开 / 这一次
// 登记里做的订阅，戳都小于它的释放通知；通知送达之前在重开的会话 / 重新登记的 subject 上重新做的订阅，戳都大于它——
// 政策据此只删除通知所指那一次生命期里的簿记。新 API，无修前版本；政策层的修前红见 policy/direct_release_lifetime_promises_test.go。
func TestReleaseStampsSeparateTheReleasedLifetimeFromTheNextOne(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	recorder := newReleaseRecorder(manager)
	stamped := func(subject int64) SubscriptionStamp {
		t.Helper()
		stamp, err := recorder.source.SubscribeStamped(1, subject, entity.SyncProfile{})
		if err != nil || stamp == 0 {
			t.Fatalf("SubscribeStamped(1, %d) = %d, %v", subject, stamp, err)
		}
		return stamp
	}
	released := func() ReleasedSubscription {
		t.Helper()
		mustFlush(t, manager)
		got := recorder.released
		recorder.released = nil
		if len(got) != 1 {
			t.Fatalf("released = %v, want one notification", got)
		}
		return got[0]
	}

	// 会话关闭后重开：通知落在两次打开之间。同视图的重复订阅也拿到新的戳。
	if err := manager.Register(labelledSubject(3601, "live")); err != nil {
		t.Fatal(err)
	}
	first := stamped(3601)
	if again := stamped(3601); again <= first {
		t.Fatalf("a repeated subscription did not get a later stamp: %d after %d", again, first)
	}
	before := stamped(3601)
	mustFlush(t, manager)
	manager.CloseSession(1)
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	after := stamped(3601)
	if got := released(); got.Session != 1 || got.Subject != 3601 || got.Stamp <= before || got.Stamp >= after {
		t.Fatalf("session release %+v, want stamp between the closed lifetime's %d and the reopened one's %d", got, before, after)
	}

	// 业务注销后同 ID 重新登记：通知落在两次登记之间（未交付的 subject 在 Unregister 时立刻被忘掉）。
	if err := manager.Register(labelledSubject(3602, "live")); err != nil {
		t.Fatal(err)
	}
	before = stamped(3602)
	if err := manager.Unregister(3602); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(labelledSubject(3602, "again")); err != nil {
		t.Fatalf("premise: 3602 was not forgotten at once: %v", err)
	}
	after = stamped(3602)
	if got := released(); got.Session != 1 || got.Subject != 3602 || got.Stamp <= before || got.Stamp >= after {
		t.Fatalf("unregister release %+v, want stamp between the first registration's %d and the next one's %d", got, before, after)
	}
}
