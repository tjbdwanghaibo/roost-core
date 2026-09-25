package entitysync

import (
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

func TestSnapshotWaitDoesNotRescanOrBlockLiveAndRemove(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1)
	states := make([]*entity.SubjectSyncState, 3)
	for i := range states {
		packs := 0
		states[i] = testSubject(t, int64(i+1), &packs)
		if err := m.Register(states[i]); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, int64(i+1), entity.SyncProfile{})
	}
	mustFlush(t, m)
	before := m.Counters()
	for range 10 {
		mustFlush(t, m)
	}
	if got := m.Counters(); got.SnapshotsDeferred != before.SnapshotsDeferred || got.EmptyFlushes != before.EmptyFlushes+10 {
		t.Fatalf("waiting snapshots rescanned: %+v", got)
	}
	if stats := m.Stats(); stats.Pending != 2 || stats.WaitingSnapshotSubjects != 2 || stats.PendingSnapshots != 2 {
		t.Fatalf("lost deferred work: %+v", stats)
	}
	states[0].MarkDirty(1)
	mustFlush(t, m)
	if m.Counters().UpdatesAdmitted != 1 {
		t.Fatal("snapshot wait blocked live update")
	}
	if err := m.Unsubscribe(1, 1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if m.Counters().RemovesAdmitted != 1 {
		t.Fatal("snapshot wait blocked remove")
	}
	m.CloseSession(1)
	open(t, m, 1)
	m.budgetWindow = time.Now().Add(-2 * time.Hour)
	mustFlush(t, m)
	if got := m.Stats(); got.Pending != 0 || got.Subscriptions != 0 {
		t.Fatalf("old lifetime resurrected: %+v", got)
	}
}

func TestTraceIncludesEverySplitFrameAndByteBudgetResumes(t *testing.T) {
	trace, err := NewSyncTrace(100)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Trace: trace, Mode: ModeOnChange, Interval: time.Hour, Limits: frame.Limits{MaxFrameBytes: 160}})
	open(t, m, 1)
	for id := int64(1); id <= 3; id++ {
		packs := 0
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	events, lost := trace.Drain()
	encoded, admitted := map[uint32]bool{}, map[uint32]bool{}
	for _, e := range events {
		if e.Stage == "encoded" {
			encoded[e.Tick] = true
		}
		if e.Stage == "admitted" {
			admitted[e.Tick] = true
		}
	}
	if lost != 0 || len(encoded) < 2 || len(encoded) != len(admitted) {
		t.Fatalf("missing split trace: %v / %v, lost=%d", encoded, admitted, lost)
	}
	for tick := range admitted {
		if !encoded[tick] {
			t.Fatalf("missing encoded frame %d", tick)
		}
	}

	// 独占软字节预算的大包仍可在新窗口推进，等待期间不重新调用 packer。
	b := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 1}})
	open(t, b, 1)
	for id := int64(1); id <= 2; id++ {
		packs := 0
		if err := b.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, b, 1, id, entity.SyncProfile{})
	}
	mustFlush(t, b)
	if b.Counters().CreatesAdmitted != 1 {
		t.Fatal("oversize soft-budget object made no progress")
	}
	before := b.Counters()
	mustFlush(t, b)
	if b.Counters().SnapshotsCaptured != before.SnapshotsCaptured {
		t.Fatal("repacked byte-budget waiter")
	}
	b.budgetWindow = time.Now().Add(-2 * time.Hour)
	mustFlush(t, b)
	if b.Counters().CreatesAdmitted != 2 || b.Stats().Pending != 0 {
		t.Fatal("byte budget did not resume")
	}
}

func TestRetiredSubjectCannotReenterSnapshotWait(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{Mode: ModeOnChange})
	packs := 0
	if err := m.Register(testSubject(t, 1, &packs)); err != nil {
		t.Fatal(err)
	}
	old := m.subject(1)
	if err := m.Unregister(1); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(testSubject(t, 1, &packs)); err != nil {
		t.Fatal(err)
	}
	// 模拟旧捕获在 subject 退休/同 ID 重建之后才完成预算判断。
	m.deferSnapshot(old)
	if m.Stats().WaitingSnapshotSubjects != 0 {
		t.Fatal("stale capture retained an undrainable waiter")
	}
}

func TestProfileDemandCacheTracksIntentAndRetainsPublishedSlices(t *testing.T) {
	m := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	open(t, m, 1)
	packs := 0
	if err := m.Register(testSubject(t, 1, &packs)); err != nil {
		t.Fatal(err)
	}
	s := m.subject(1)
	check := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		d, f := s.profilesLocked(nil)
		wantD, wantF := s.collectProfilesLocked(nil)
		if !slices.Equal(d, wantD) || !slices.Equal(f, wantF) {
			t.Fatalf("stale profile demand: %v/%v want %v/%v", d, f, wantD, wantF)
		}
	}
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "old"})
	check()
	_, old := s.profilesLocked(nil)
	mustFlush(t, m)
	check()
	mustSubscribe(t, m, 1, 1, entity.SyncProfile{Key: "new"})
	check()
	if len(old) != 1 || old[0].Key != "old" {
		t.Fatal("published demand slice mutated")
	}
	mustFlush(t, m)
	check()
	if err := m.HoldSession(1); err != nil {
		t.Fatal(err)
	}
	check()
	if err := m.ReadySession(1); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	check()
	if err := m.Unsubscribe(1, 1); err != nil {
		t.Fatal(err)
	}
	check()
	mustFlush(t, m)
	check()
}

func TestSyncTraceBoundedIndependentAndOptional(t *testing.T) {
	trace, err := NewSyncTrace(2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		trace.Record(SyncTraceEvent{Stage: "test", SubjectID: int64(i)})
	}
	events, overwritten := trace.Drain()
	if overwritten != 1 || len(events) != 2 || events[0].SubjectID != 1 || events[1].SubjectID != 2 {
		t.Fatalf("bad ring %v/%d", events, overwritten)
	}
	trace.Record(SyncTraceEvent{SubjectID: 4})
	if events[0].SubjectID != 1 {
		t.Fatal("drained slice reused")
	}
	var disabled *SyncTrace
	if got := testing.AllocsPerRun(100, func() { disabled.Record(SyncTraceEvent{}) }); got != 0 {
		t.Fatalf("disabled allocated %g", got)
	}
	for _, capacity := range []int{0, -1, (1 << 20) + 1} {
		if _, err := NewSyncTrace(capacity); err == nil {
			t.Fatal("invalid capacity accepted")
		}
	}
}
