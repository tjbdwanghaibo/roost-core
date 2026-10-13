// 快照调度：大快照在字节预算下有进度、被挡窗口不重复捕获、交付最新内容、硬上限明确失败、请求索引与统计、trace。

package entitysync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR 复现：字节软预算下大对象被其他会话持续插队（有限积压，3 会话）。
func TestLargeSnapshotFiniteBacklog(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxBytes: 300, PerSessionObjects: 1}})
	open(t, m, 1, 2, 3)
	if err := m.Register(largeSubject(1000, 1000)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 2, 1000, entity.SyncProfile{}) // 大对象最先订阅
	for i := int64(1); i <= 30; i++ {
		for _, sid := range []SessionID{1, 3} {
			id := int64(sid)*100 + i
			if err := m.Register(largeSubject(id, 10)); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, sid, id, entity.SyncProfile{})
		}
	}
	for tick := 1; tick <= 3; tick++ {
		mustFlush(t, m)
		got := map[SessionID]int{}
		for sid := SessionID(1); sid <= 3; sid++ {
			for _, raw := range r.take(sid) {
				got[sid] += len(decodeFrame(t, raw).updates)
			}
		}
		_, big := m.session(2).objects[1000]
		t.Logf("tick=%d s1=%d s2=%d s3=%d bigDelivered=%v", tick, got[1], got[2], got[3], big)
		if big {
			return
		}
	}
	t.Fatal("large snapshot never delivered in 3 ticks")
}

// RR 复现：每轮其他会话持续新增小对象，大对象永远轮不到本窗口首个准入。
func TestLargeSnapshotStarvedUnderChurn(t *testing.T) {
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			r := newRecordingTransport()
			m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 300, PerSessionObjects: 1}})
			open(t, m, 1, 2, 3)
			if err := m.Register(largeSubject(1000, 1000)); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, 2, 1000, entity.SyncProfile{})
			for tick := int64(1); tick <= 4; tick++ {
				for _, sid := range []SessionID{1, 3} {
					id := int64(sid)*10000 + tick
					if err := m.Register(largeSubject(id, 10)); err != nil {
						t.Fatal(err)
					}
					mustSubscribe(t, m, sid, id, entity.SyncProfile{})
				}
				m.budgetWindow = time.Now().Add(-2 * time.Hour) // on_change：进入下一窗口
				mustFlush(t, m)
				for sid := SessionID(1); sid <= 3; sid++ {
					r.take(sid)
				}
				if _, ok := m.session(2).objects[1000]; ok {
					t.Logf("big delivered at window %d", tick)
					return
				}
			}
			s := m.Stats()
			t.Fatalf("big snapshot for session 2 never delivered in 4 windows; s1 objects=%d s3 objects=%d SnapshotsCaptured=%d SnapshotsDeferred=%d PendingSnapshots=%d", len(m.session(1).objects), len(m.session(3).objects), s.SnapshotsCaptured, s.SnapshotsDeferred, s.PendingSnapshots)
		})
	}
}

// 补充：单会话内按 subject 轮转。三种小对象到达模式，大对象 X=1000。
func TestLargeSnapshotSingleSession(t *testing.T) {
	type arrival func(tick int64, rng *rand.Rand) []int64
	cases := map[string]arrival{
		// 新 ID 单调递增且大于 X（常见发号），初始积压 20 个小于 X 的小对象。
		"monotonic_above": func(tick int64, _ *rand.Rand) []int64 { return []int64{2000 + 2*tick, 2001 + 2*tick} },
		// 每轮只来 1 个新 ID（单调递增、大于 X）。
		"monotonic_above_one_per_tick": func(tick int64, _ *rand.Rand) []int64 { return []int64{2000 + tick} },
		// 新 ID 随机分布在 X 两侧。
		"random_both_sides": func(tick int64, rng *rand.Rand) []int64 {
			return []int64{1 + rng.Int64N(999), 1001 + rng.Int64N(999)}
		},
		// 新 ID 单调递增且小于 X。
		"monotonic_below": func(tick int64, _ *rand.Rand) []int64 { return []int64{100 + 2*tick, 101 + 2*tick} },
	}
	for name, next := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRecordingTransport()
			m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxBytes: 300}})
			open(t, m, 1)
			rng := rand.New(rand.NewPCG(1, 2))
			seen := map[int64]bool{1000: true}
			add := func(id int64) {
				if seen[id] {
					return
				}
				seen[id] = true
				if err := m.Register(largeSubject(id, 10)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, 1, id, entity.SyncProfile{})
			}
			for id := int64(1); id <= 20; id++ {
				add(id)
			}
			if err := m.Register(largeSubject(1000, 1000)); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, 1, 1000, entity.SyncProfile{})
			// 会话对象上限 100（frame 默认），在 90 前停止，避免上限本身阻止大对象。
			for tick := int64(1); tick <= 14; tick++ {
				for _, id := range next(tick, rng) {
					add(id)
				}
				mustFlush(t, m)
				delivered := 0
				for _, raw := range r.take(1) {
					delivered += len(decodeFrame(t, raw).updates)
				}
				sess := m.session(1)
				t.Logf("%s tick=%d delivered=%d objects=%d snapshotAfter=%v pendingSnapshots=%d", name, tick, delivered, len(sess.objects), sess.snapshotAfter, m.Stats().PendingSnapshots)
				if _, ok := sess.objects[1000]; ok {
					t.Logf("%s: big delivered at tick %d", name, tick)
					return
				}
				if len(sess.objects) >= 90 {
					t.Errorf("%s: big not delivered after %d ticks; small objects delivered=%d (stopped before session object limit)", name, tick, len(sess.objects))
					return
				}
			}
			t.Errorf("%s: big never delivered in 14 ticks", name)
		})
	}
}

func TestByteBlockedWindowDoesNotRecaptureUntilNextWindow(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 300}})
	open(t, m, 1)
	for _, item := range []struct {
		id   int64
		size int
	}{{1, 10}, {2, 1000}, {3, 10}} {
		if err := m.Register(largeSubject(item.id, item.size)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, item.id, entity.SyncProfile{})
	}
	mustFlush(t, m)
	if !m.windowByteBlocked {
		t.Fatal("expected exhausted byte window")
	}
	captured := m.Stats().SnapshotsCaptured
	for range 5 {
		m.markPending(2)
		m.markPending(3)
		mustFlush(t, m)
	}
	if m.Stats().SnapshotsCaptured != captured {
		t.Fatal("repeated cold capture in blocked window")
	}
	m.refreshSnapshotWindowAt(m.budgetWindow.Add(time.Hour))
	mustFlush(t, m)
	if _, ok := m.session(1).objects[2]; !ok {
		t.Fatal("large snapshot did not get exclusive window")
	}
	m.refreshSnapshotWindowAt(m.budgetWindow.Add(time.Hour))
	mustFlush(t, m)
	if len(m.session(1).objects) != 3 || m.Stats().PendingSnapshots != 0 || m.AuditStats().PendingSnapshots != 0 {
		t.Fatal("small suffix failed to drain")
	}
}

// mutableSnapshot 的快照内容可在等待期间变化；packs 统计实际打包（捕获编码）次数。
type mutableSnapshot struct {
	marker byte
	size   int
	packs  int
}

func (s *mutableSnapshot) subject(id int64) *entity.SubjectSyncState {
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		s.packs++
		return entity.TakeFrozenSyncPayload(1, bytes.Repeat([]byte{s.marker}, s.size)), nil
	}
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Packer: entity.SubjectSyncPackFunc{Snapshot: pack}})
}

// 大对象 X 排在 backlog 个小对象之后、每窗口另来 1 个新小对象：被字节预算挡住的窗口不重新捕获 / 编码 X，也不把积压小对象反复
// 捕获后再延后；超出实际交付的捕获数有常数上界，积压翻倍时上界不变。
// 历史：RR-20260926-04。
func TestByteBlockedSnapshotsAreNotRecapturedEveryWindow(t *testing.T) {
	for _, backlog := range []int{30, 60} {
		for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
			t.Run(fmt.Sprintf("%s/backlog%d", mode, backlog), func(t *testing.T) {
				testByteBlockedSnapshotRecapture(t, mode, backlog)
			})
		}
	}
}

func testByteBlockedSnapshotRecapture(t *testing.T, mode SyncMode, backlog int) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 300}})
	open(t, m, 1)
	next := int64(1)
	addSmall := func() {
		if err := m.Register(largeSubject(next, 10)); err != nil {
			t.Fatal(err)
		}
		mustSubscribe(t, m, 1, next, entity.SyncProfile{})
		next++
	}
	for range backlog {
		addSmall()
	}
	x := &mutableSnapshot{marker: 7, size: 1000}
	if err := m.Register(x.subject(5000)); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, m, 1, 5000, entity.SyncProfile{})
	for window := 1; window <= 80; window++ {
		addSmall()
		m.budgetWindow = time.Now().Add(-2 * time.Hour) // on_change：进入下一窗口
		mustFlush(t, m)
		if mode == ModeOnChange {
			for range 3 { // 同窗口即时通知反复 Flush
				m.markPending(5000)
				mustFlush(t, m)
			}
		}
		r.take(1)
		objects := len(m.session(1).objects)
		if _, ok := m.session(1).objects[5000]; !ok {
			continue
		}
		captured := m.Stats().SnapshotsCaptured
		t.Logf("X delivered at window %d: X packs=%d SnapshotsCaptured=%d objects=%d", window, x.packs, captured, objects)
		if x.packs > 2 || captured > uint64(objects)+2 {
			t.Fatalf("blocked windows re-encoded snapshots: X packs=%d SnapshotsCaptured=%d delivered objects=%d windows=%d", x.packs, captured, objects, window)
		}
		return
	}
	t.Fatalf("X never delivered in 80 windows; X packs=%d SnapshotsCaptured=%d", x.packs, m.Stats().SnapshotsCaptured)
}

// 准入判定只借用上次编码大小；真正交付时用最新冻结内容，等待期间内容变大、变小都不发出陈旧编码。
func TestByteBlockedSnapshotDeliversLatestContent(t *testing.T) {
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			r := newRecordingTransport()
			m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 300}})
			open(t, m, 1)
			for id := int64(1); id <= 9; id++ {
				if err := m.Register(largeSubject(id, 10)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, 1, id, entity.SyncProfile{})
			}
			x := &mutableSnapshot{marker: 1, size: 1000}
			if err := m.Register(x.subject(5000)); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, 1, 5000, entity.SyncProfile{})
			var got entity.SubjectSyncUpdate
			for window := 1; window <= 10 && got.SubjectID == 0; window++ {
				switch window {
				case 2:
					x.marker, x.size = 2, 1200
				case 3:
					x.marker, x.size = 3, 40
				}
				m.budgetWindow = time.Now().Add(-2 * time.Hour) // on_change：进入下一窗口
				mustFlush(t, m)
				for _, raw := range r.take(1) {
					if update, ok := decodeFrame(t, raw).updates[5000]; ok {
						got = update
					}
				}
			}
			if got.SubjectID == 0 {
				t.Fatal("X never delivered")
			}
			payload := got.Payload.AppendTo(nil)
			if len(payload) != 40 || payload[0] != 3 {
				t.Fatalf("stale snapshot delivered: len=%d marker=%d, want len=40 marker=3", len(payload), payload[0])
			}
		})
	}
}

// 软预算放行大对象不能放过硬上限：超出传输帧上限的对象在有限窗口内以 ErrFrameTooLarge 明确失败（只关闭所在会话），其他会话继续有进度。
func TestOverHardLimitSnapshotStillFailsExplicitly(t *testing.T) {
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			r := boundedRecorder{newRecordingTransport(), 150}
			var lost []SessionID
			var cause error
			m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxBytes: 300},
				SessionLost: func(sid SessionID, err error) { lost, cause = append(lost, sid), err }})
			open(t, m, 1, 2)
			next := int64(1)
			addSmall := func(sid SessionID) {
				if err := m.Register(largeSubject(next, 10)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, sid, next, entity.SyncProfile{})
				next++
			}
			addSmall(1)
			if err := m.Register(largeSubject(9000, 1000)); err != nil {
				t.Fatal(err)
			}
			mustSubscribe(t, m, 1, 9000, entity.SyncProfile{})
			for window := 1; window <= 3 && len(lost) == 0; window++ {
				addSmall(2)
				m.budgetWindow = time.Now().Add(-2 * time.Hour) // on_change：进入下一窗口
				mustFlush(t, m)
				for _, sid := range []SessionID{1, 2} {
					for _, raw := range r.take(sid) {
						if len(raw) > 150 {
							t.Fatalf("frame over hard limit sent: %d bytes", len(raw))
						}
					}
				}
			}
			if len(lost) != 1 || lost[0] != 1 || !errors.Is(cause, frame.ErrFrameTooLarge) {
				t.Fatalf("over-hard-limit object did not fail explicitly within 3 windows: lost=%v cause=%v", lost, cause)
			}
			for range 3 {
				addSmall(2)
				m.budgetWindow = time.Now().Add(-2 * time.Hour) // on_change：进入下一窗口
				mustFlush(t, m)
			}
			if pending := m.Stats().PendingSnapshots; pending != 0 || len(m.session(2).objects) != 6 {
				t.Fatalf("other session stalled after failure: pending=%d session2 objects=%d", pending, len(m.session(2).objects))
			}
		})
	}
}

func assertStatsAudited(t *testing.T, m *Manager) {
	t.Helper()
	started := time.Now()
	fast, audit := m.Stats(), m.AuditStats()
	elapsed := time.Since(started)
	if fast.Subjects != audit.Subjects || fast.Sessions != audit.Sessions || fast.HeldSessions != audit.HeldSessions || fast.Subscriptions != audit.Subscriptions || fast.PendingSnapshots != audit.PendingSnapshots || fast.Pending != audit.Pending || fast.WaitingSnapshotSubjects != audit.WaitingSnapshotSubjects {
		t.Fatalf("incremental=%+v audit=%+v", fast, audit)
	}
	// 两次采样时间不同：刚入队时第一次可能为 0，第二次已跨系统时钟刻度。
	// 同一最老请求的年龄只能增加，且增量不超过本次两次读取的真实耗时。
	if growth := audit.OldestSnapshotWait - fast.OldestSnapshotWait; growth < 0 || growth > elapsed {
		t.Fatalf("wait age drift: fast=%v audit=%v elapsed=%v", fast.OldestSnapshotWait, audit.OldestSnapshotWait, elapsed)
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
