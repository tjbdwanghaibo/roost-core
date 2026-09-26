package entitysync

import (
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// 验证的是客户端真正收到的 create：数量预选与字节准入不能互相推翻顺序。
func TestSnapshotArrivalAndRecoveryAlternate(t *testing.T) {
	for name, budget := range map[string]SnapshotBudget{
		"objects": {MaxObjects: 1}, "bytes": {MaxBytes: 1}, "session": {PerSessionObjects: 1},
	} {
		for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
			for _, recoveryFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/mode%d/lowRecovery%t", name, mode, recoveryFirst), func(t *testing.T) {
					r := newRecordingTransport()
					m := newTestManager(t, r, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: budget})
					open(t, m, 1)
					old, added := []int64{1, 2}, []int64{100, 101}
					if !recoveryFirst {
						old, added = added, old
					}
					packs := 0
					for _, id := range append(append([]int64{}, old...), added...) {
						if err := m.Register(testSubject(t, id, &packs)); err != nil {
							t.Fatal(err)
						}
					}
					flush := func() {
						m.budgetWindow = time.Now().Add(-2 * time.Hour)
						mustFlush(t, m)
					}
					for _, id := range old {
						mustSubscribe(t, m, 1, id, entity.SyncProfile{})
						flush()
						oneFrame(t, r, 1)
					}
					if err := m.HoldSession(1); err != nil {
						t.Fatal(err)
					}
					// 改 Profile 不能把恢复转换为新入场。
					mustSubscribe(t, m, 1, old[0], entity.SyncProfile{Key: "changed"})
					for _, id := range added {
						mustSubscribe(t, m, 1, id, entity.SyncProfile{})
					}
					mustFlush(t, m)
					if len(r.take(1)) != 0 {
						t.Fatal("held session received frame")
					}
					if err := m.ReadySession(1); err != nil {
						t.Fatal(err)
					}
					var previous bool
					for i := range 4 {
						flush()
						f := oneFrame(t, r, 1)
						if len(f.updates) != 1 {
							t.Fatalf("budget must admit one create: %+v", f.updates)
						}
						for id := range f.updates {
							recovery := id == old[0] || id == old[1]
							if i > 0 && recovery == previous {
								t.Fatalf("one class starved: tick=%d recovery=%t", i, recovery)
							}
							previous = recovery
						}
						if mode == ModeOnChange {
							mustFlush(t, m)
							if len(r.take(1)) != 0 {
								t.Fatal("immediate flush spent the same window twice")
							}
						}
					}
					if m.Stats().Pending != 0 || len(m.session(1).objects) != 4 {
						t.Fatal("baselines failed to converge")
					}
				})
			}
		}
	}
}

func TestSnapshotIdleClassBorrowsBudgetAndRotatesSessions(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 3, PerSessionObjects: 2}})
	open(t, m, 1, 2)
	packs := 0
	for id := int64(1); id <= 4; id++ {
		if err := m.Register(testSubject(t, id, &packs)); err != nil {
			t.Fatal(err)
		}
		for _, sid := range []SessionID{1, 2} {
			mustSubscribe(t, m, sid, id, entity.SyncProfile{})
		}
	}
	for _, want := range [][2]int{{2, 1}, {2, 1}, {0, 2}} {
		mustFlush(t, m)
		for sid := SessionID(1); sid <= 2; sid++ {
			got := 0
			for _, raw := range r.take(sid) {
				got += len(decodeFrame(t, raw).updates)
			}
			if got != want[sid-1] {
				t.Fatalf("session %d got %d want %d", sid, got, want[sid-1])
			}
		}
	}
	if m.Stats().Pending != 0 {
		t.Fatal("idle class reserved unused budget")
	}
}

func TestSnapshotClassesRotateAcrossSessions(t *testing.T) {
	r := newRecordingTransport()
	m := newTestManager(t, r, ManagerConfig{SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	open(t, m, 1, 2, 3, 4)
	packs := 0
	if err := m.Register(testSubject(t, 1, &packs)); err != nil {
		t.Fatal(err)
	}
	for _, sid := range []SessionID{1, 2} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
		mustFlush(t, m)
		oneFrame(t, r, sid)
	}
	for _, sid := range []SessionID{1, 2} {
		if err := m.HoldSession(sid); err != nil {
			t.Fatal(err)
		}
		if err := m.ReadySession(sid); err != nil {
			t.Fatal(err)
		}
	}
	for _, sid := range []SessionID{3, 4} {
		mustSubscribe(t, m, sid, 1, entity.SyncProfile{})
	}
	seen := make(map[SessionID]bool)
	var previous bool
	for tick := range 4 {
		mustFlush(t, m)
		admitted := 0
		for sid := SessionID(1); sid <= 4; sid++ {
			for _, raw := range r.take(sid) {
				admitted++
				if seen[sid] || len(decodeFrame(t, raw).updates) != 1 {
					t.Fatalf("session %d failed to make independent progress", sid)
				}
				seen[sid] = true
				recovery := sid <= 2
				if tick > 0 && recovery == previous {
					t.Fatal("session order overrode class fairness")
				}
				previous = recovery
			}
		}
		if admitted != 1 {
			t.Fatalf("global budget: admitted %d", admitted)
		}
	}
	if len(seen) != 4 || m.Stats().Pending != 0 {
		t.Fatal("class session rotation did not converge")
	}
}
