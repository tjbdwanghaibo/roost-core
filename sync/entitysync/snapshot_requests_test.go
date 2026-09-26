package entitysync

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func assertStatsAudited(t *testing.T, m *Manager) {
	t.Helper()
	fast, audit := m.Stats(), m.AuditStats()
	if fast.Subjects != audit.Subjects || fast.Sessions != audit.Sessions || fast.HeldSessions != audit.HeldSessions || fast.Subscriptions != audit.Subscriptions || fast.PendingSnapshots != audit.PendingSnapshots || fast.Pending != audit.Pending || fast.WaitingSnapshotSubjects != audit.WaitingSnapshotSubjects || (fast.OldestSnapshotWait == 0) != (audit.OldestSnapshotWait == 0) {
		t.Fatalf("incremental=%+v audit=%+v", fast, audit)
	}
	m.snapshotRequests.mu.Lock()
	defer m.snapshotRequests.mu.Unlock()
	count := 0
	for _, groups := range m.snapshotRequests.groups {
		for _, group := range groups {
			count += len(group.entries)
		}
	}
	if count != audit.PendingSnapshots {
		t.Fatalf("indexed requests=%d snapshots=%d", count, audit.PendingSnapshots)
	}
}
func TestSnapshotIndexAndStatsFollowLifecycle(t *testing.T) {
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
			packs := 0
			open(t, m, 1, 2)
			for id := int64(1); id <= 3; id++ {
				if err := m.Register(testSubject(t, id, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, 1, id, entity.SyncProfile{})
				mustSubscribe(t, m, 2, id, entity.SyncProfile{})
			}
			check := func() { assertStatsAudited(t, m) }
			check()
			mustFlush(t, m)
			check()
			if err := m.HoldSession(1); err != nil {
				t.Fatal(err)
			}
			check()
			if err := m.HoldSession(1); err != nil {
				t.Fatal(err)
			}
			check()
			source := m.NewSubscriptionSource()
			if err := source.Subscribe(2, 2, entity.SyncProfile{Key: "secondary"}); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, 2, 1, entity.SyncProfile{Key: "new"})
			check()
			if err := m.Unsubscribe(2, 2); err != nil {
				t.Fatal(err)
			}
			check()
			if err := m.Unsubscribe(1, 3); err != nil {
				t.Fatal(err)
			}
			check()
			if err := m.ReadySession(1); err != nil {
				t.Fatal(err)
			}
			check()
			for range 8 {
				m.budgetWindow = time.Now().Add(-2 * time.Hour)
				mustFlush(t, m)
				check()
			}
			m.CloseSession(2)
			check()
			open(t, m, 2)
			mustSubscribe(t, m, 2, 3, entity.SyncProfile{})
			check()
			if err := m.Unregister(1); err != nil {
				t.Fatal(err)
			}
			check()
			mustFlush(t, m)
			check()
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			check()
			if m.Stats().Subscriptions != 0 || m.Stats().PendingSnapshots != 0 {
				t.Fatal("close retained subscriptions")
			}
		})
	}
}

func TestSnapshotPlanningAndStatsDoNotAcquireSubjectLocks(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1)
	packs := 0
	for id := int64(1); id <= 3; id++ {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	subj := m.subject(2)
	subj.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		plan := m.planSnapshotCaptures([]int64{1, 2, 3})
		if len(plan.order) != 1 {
			t.Errorf("selected=%d", len(plan.order))
		}
		_ = m.Stats()
	}()
	select {
	case <-done:
		subj.mu.Unlock()
	case <-time.After(time.Second):
		subj.mu.Unlock()
		<-done
		t.Fatal("planning or Stats waited on unrelated subject")
	}
}
