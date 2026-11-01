package entitysync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func nextSendWindow(m *Manager) {
	m.budgetWindow = time.Now().Add(-2 * m.config.Interval)
	for _, session := range m.sessions {
		session.lifetime.sendWindow = time.Now().Add(-2 * m.config.Interval)
	}
}

func TestSendBudgetPriorityAgesAndComposesWithColdBudget(t *testing.T) {
	r := newRecordingTransport()
	high := entity.SyncProfile{Key: "high"}
	m := newTestManager(t, r, ManagerConfig{Interval: time.Hour,
		SnapshotBudget: SnapshotBudget{MaxObjects: 1},
		SendBudget:     SendBudget{MaxBytes: 1, MaxWait: time.Minute, Priority: map[entity.SyncProfile]int{high: -1}},
	})
	open(t, m, 1)
	packs := 0
	states := []*entity.SubjectSyncState{testSubject(t, 1, &packs), testSubject(t, 2, &packs)}
	for i, state := range states {
		if err := m.Register(state); err != nil {
			t.Fatal(err)
		}
		profile := entity.SyncProfile{}
		if i == 1 {
			profile = high
		}
		mustSubscribe(t, m, 1, state.SubjectID(), profile)
	}
	// 冷预算先限定候选；两个窗口都应有进度，不得相互预扣导致永久空转。
	for range 2 {
		mustFlush(t, m)
		r.take(1)
		nextSendWindow(m)
	}
	if len(m.session(1).objects) != 2 {
		t.Fatal("cold and send budgets deadlocked")
	}
	for _, state := range states {
		state.MarkDirty(1)
	}
	mustFlush(t, m)
	if _, exists := oneFrame(t, r, 1).updates[2]; !exists {
		t.Fatal("authorized high priority view not selected")
	}
	low := m.subjects[1].subscribers[1]
	low.sendWaiting = time.Now().Add(-2 * time.Minute)
	nextSendWindow(m)
	states[1].MarkDirty(2)
	mustFlush(t, m)
	if _, exists := oneFrame(t, r, 1).updates[1]; !exists {
		t.Fatal("new high-priority changes starved old snapshot")
	}
	if m.Counters().SendDeferred == 0 {
		t.Fatal("missing budget diagnostic")
	}
}

func TestSendBudgetRestoresSkippedDeltaAndDoesNotBlockOtherSessions(t *testing.T) {
	transport := newRecordingTransport()
	m := newTestManager(t, transport, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SendBudget: SendBudget{MaxBytes: 1}})
	open(t, m, 1, 2)
	packs := 0
	a, b := testSubject(t, 1, &packs), testSubject(t, 2, &packs)
	for _, state := range []*entity.SubjectSyncState{a, b} {
		if err := m.Register(state); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, state.SubjectID(), entity.SyncProfile{})
	}
	mustSubscribe(t, m, 2, 2, entity.SyncProfile{})
	mustFlush(t, m)
	if len(m.session(1).objects) != 1 || len(m.session(2).objects) != 1 {
		t.Fatal("per-session progress")
	}
	transport.take(1)
	transport.take(2)
	nextSendWindow(m)
	mustFlush(t, m)
	transport.take(1)
	transport.take(2)
	nextSendWindow(m)
	a.MarkDirty(1)
	b.MarkDirty(1)
	mustFlush(t, m)
	if oneFrame(t, transport, 2).updates[2].Full {
		t.Fatal("uncongested session should receive delta")
	}
	transport.take(1)
	sub := m.subjects[2].subscribers[1]
	if sub.kind != kindSnapshot || sub.inFlight {
		t.Fatal("skipped delta must owe full and release inFlight")
	}
	b.MarkDirty(2)
	mustFlush(t, m)
	if len(transport.take(1)) != 0 {
		t.Fatal("on_change bypassed the interval budget")
	}
	nextSendWindow(m)
	mustFlush(t, m)
	update := oneFrame(t, transport, 1).updates[2]
	if !update.Full || update.Version != b.Version() {
		t.Fatalf("latest full not restored: %+v", update)
	}
	if sub.kind != kindLive || !sub.sendWaiting.IsZero() {
		t.Fatal("settlement did not clear waiting age")
	}
	// 删除不受已耗尽的字节额度限制。
	m.Unsubscribe(1, 2)
	mustFlush(t, m)
	if _, exists := m.session(1).objects[2]; exists {
		t.Fatal("remove blocked by budget")
	}
}

func TestSendBudgetRetryRetainsDebtAndReconnectStartsFresh(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{Interval: time.Hour, SendBudget: SendBudget{MaxBytes: 1}})
	open(t, m, 1)
	if err := m.Register(largeSubject(1, 1000)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
	r.breakSession(1, ErrRetryLater)
	if err := m.Flush(context.Background()); !errors.Is(err, ErrRetryLater) {
		t.Fatal(err)
	}
	r.breakSession(1, nil)
	mustFlush(t, m)
	if len(r.take(1)) != 0 {
		t.Fatal("retry evaded attempt budget")
	}
	nextSendWindow(m)
	mustFlush(t, m)
	if len(r.take(1)) != 1 {
		t.Fatal("large whole subject made no progress")
	}
	m.CloseSession(1)
	open(t, m, 1)
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{})
	mustFlush(t, m)
	if len(r.take(1)) != 1 {
		t.Fatal("new lifetime inherited old debt")
	}
}
