package entitysync

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// 现有对象的全量视图替换不能因新对象恢复额度耗尽而一直停留在旧视图。
func TestExistingObjectRefreshDoesNotWaitForColdSnapshotBudget(t *testing.T) {
	for name, budget := range map[string]SnapshotBudget{
		"objects": {MaxObjects: 1}, "bytes": {MaxBytes: 1}, "session": {PerSessionObjects: 1},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRecordingTransport()
			m := newTestManager(t, r, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: budget})
			open(t, m, 1)
			for id := int64(1); id <= 2; id++ {
				packs := 0
				if err := m.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
			}
			mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "old"})
			mustFlush(t, m)
			oneFrame(t, r, 1)
			ref := m.session(1).objects[1]
			mustSubscribe(t, m, 1, 2, entity.SyncProfile{})
			mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "new"})
			mustFlush(t, m)
			frames := r.take(1)
			if len(frames) != 1 {
				t.Fatalf("existing object refresh waited for cold snapshot budget: frames=%d", len(frames))
			}
			f := decodeFrame(t, frames[0])
			if len(f.updates) != 1 || !f.updates[1].Full || f.updates[1].Profile.Key != "new" || f.objects[1] != frame.ObjectUpdate {
				t.Fatalf("wrong refresh: %+v", f)
			}
			if m.session(1).objects[1] != ref || m.Counters().CreatesAdmitted != 1 {
				t.Fatal("refresh recreated object or cold object escaped budget")
			}
			if m.Stats().PendingSnapshots != 1 {
				t.Fatal("cold baseline lost")
			}
			m.budgetWindow = time.Now().Add(-2 * time.Hour)
			mustFlush(t, m)
			if m.Counters().CreatesAdmitted != 2 || m.Stats().Pending != 0 {
				t.Fatal("cold baseline did not resume")
			}
		})
	}
}

func TestExistingObjectRefreshLeavesColdQuotaAndHoldRestoresIt(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1, PerSessionObjects: 1}})
	open(t, m, 1)
	for id := int64(1); id <= 2; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
	}
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
	mustFlush(t, m)
	oneFrame(t, r, 1)
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "refresh"})
	mustSubscribe(t, m, 1, 2, entity.SyncProfile{})
	mustFlush(t, m)
	if got := m.Counters(); got.CreatesAdmitted != 2 || got.UpdatesAdmitted != 1 {
		t.Fatalf("refresh consumed cold quota: %+v", got)
	}
	if err := m.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	if err := m.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if got := m.Counters().CreatesAdmitted; got != 3 {
		t.Fatalf("reset references escaped cold quota: %d", got)
	}
	if m.Stats().PendingSnapshots != 1 {
		t.Fatal("reset should still have one cold baseline")
	}
	mustFlush(t, m)
	if m.Counters().CreatesAdmitted != 4 || m.Stats().Pending != 0 {
		t.Fatal("reset did not complete")
	}
}
