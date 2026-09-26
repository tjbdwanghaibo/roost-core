package entitysync

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
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
