package cellworld

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 跨格伤害在 v2 执行模型下的用例（v1 combat_test 按新模型改写，断言含义不变）：每个 HitID 至多扣一次血；Applied 都真的
// 结算过；Unknown 可能已结算；去重表随交接带走（在目标实体的 DAO 上）。

func (w *world) waitResults(n int, timeout time.Duration) []AttackResult {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		w.mu.Lock()
		got := append([]AttackResult(nil), w.results...)
		w.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *world) applications() map[HitID]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[HitID]int{}
	for k, v := range w.hits {
		out[k] = v
	}
	return out
}

// 全部消息重复（含攻击本身）：10 次攻击各只结算一次，HP 100 → 80。
func TestDamageAcrossBoundaryAppliedOnce(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(string, *wireMsg) (bool, bool, time.Duration) { return false, true, 0 })
	w := startWorld(t, spec)
	target := npcID(21)
	w.spawn(CellID{1, 0}, EntityState{ID: target, X: 10300, Y: 5000, HP: 100})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	time.Sleep(3 * worldPeriod)
	for range 10 {
		w.attack(1, target, 2)
	}
	results := w.waitResults(10, 5*time.Second)
	time.Sleep(3 * worldPeriod)
	apps := w.applications()
	if len(apps) != 10 {
		t.Fatalf("%d distinct hits applied, want 10 (results %d)", len(apps), len(results))
	}
	for hit, n := range apps {
		if n != 1 {
			t.Errorf("hit %v applied %d times", hit, n)
		}
	}
	if s, ok := w.roleOf(CellID{1, 0}, target); !ok || s.State.HP != 80 {
		t.Fatalf("target %+v, want hp 80", s)
	}
}

// 前置检查：本格知道但超出攻击距离 → OutOfRange；邻格边界带以外（本格看不见）→ NotVisible；都不结算。
func TestDamageRejectedOutOfRangeOrInvisible(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	far, invisible := npcID(31), npcID(32)
	w.spawn(CellID{0, 0}, EntityState{ID: far, X: 7000, Y: 5000, HP: 100})
	w.spawn(CellID{1, 0}, EntityState{ID: invisible, X: 15000, Y: 5000, HP: 100})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	time.Sleep(3 * worldPeriod)
	w.attack(1, far, 5)
	w.attack(1, invisible, 5)
	results := w.waitResults(2, 3*time.Second)
	reasons := map[int64]RejectReason{}
	for _, r := range results {
		if r.Outcome != OutcomeRejected {
			t.Errorf("attack on %d: %v, want rejected", r.Target, r.Outcome)
		}
		reasons[r.Target] = r.Reason
	}
	if reasons[far] != ReasonOutOfRange || reasons[invisible] != ReasonNotVisible {
		t.Fatalf("reasons %v, want out-of-range for %d and not-visible for %d", reasons, far, invisible)
	}
	if apps := w.applications(); len(apps) != 0 {
		t.Fatalf("rejected attacks applied: %v", apps)
	}
}

// 去重随交接带走：B 结算后回复丢失，目标随即走进 A；攻击者格的重试先被 B 回“重试、owner 是 A”，再到 A——A 的去重表里
// 已有这次伤害（随 Offer 带来），回 Applied 且 Duplicate，HP 只扣一次。
func TestDamageDedupTravelsWithHandoff(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var start atomic.Int64
	start.Store(time.Now().UnixNano())
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		// 前 1.5s 丢掉“已结算”的回复：重试要跨过目标的交接。
		early := time.Since(time.Unix(0, start.Load())) < 1500*time.Millisecond
		return early && m.Kind == kindDamageReply && m.DamageReply.Outcome == OutcomeApplied, false, 0
	})
	w := startWorld(t, spec)
	target := npcID(41)
	w.enter(1, spatial.Point{X: 9500, Y: 5000})
	w.spawn(CellID{1, 0}, EntityState{ID: target, X: 10150, Y: 5000, VX: -300, HP: 100})
	time.Sleep(2 * worldPeriod)
	start.Store(time.Now().UnixNano())
	hit := w.attack(1, target, 7)
	results := w.waitResults(1, 4*time.Second)
	if len(results) != 1 || results[0].Hit != hit || results[0].Outcome != OutcomeApplied {
		t.Fatalf("results %+v, want one applied result for %v", results, hit)
	}
	if apps := w.applications(); apps[hit] != 1 || len(apps) != 1 {
		t.Fatalf("applications %v, want %v exactly once", apps, hit)
	}
	if !results[0].Duplicate {
		t.Logf("result was not a duplicate (the reply got through before the handoff): %+v", results[0])
	}
	waitFor(t, 3*time.Second, "target real in A", func() bool { s, ok := w.roleOf(CellID{0, 0}, target); return ok && s.Role == "real" })
	if s, _ := w.roleOf(CellID{0, 0}, target); s.State.HP != 93 {
		t.Fatalf("target hp %d in A, want 93", s.State.HP)
	}
}

// 回复全部丢失：期限到了报 Unknown（可能已结算——确实结算了一次）。
func TestDamageUnknownAfterDeadline(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) { return m.Kind == kindDamageReply, false, 0 })
	spec.cellMod = func(cfg *NodeConfig) { cfg.AttackDeadline = 500 * time.Millisecond }
	w := startWorld(t, spec)
	target := npcID(51)
	w.spawn(CellID{1, 0}, EntityState{ID: target, X: 10300, Y: 5000, HP: 100})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	time.Sleep(3 * worldPeriod)
	hit := w.attack(1, target, 3)
	results := w.waitResults(1, 3*time.Second)
	if len(results) != 1 || results[0].Outcome != OutcomeUnknown {
		t.Fatalf("results %+v, want unknown", results)
	}
	if apps := w.applications(); apps[hit] != 1 {
		t.Fatalf("applications %v: the unknown hit was applied %d times, want exactly once", apps, apps[hit])
	}
}

// 去重表满（窗口内 3 条）时拒绝新伤害（Busy），不淘汰旧记录。
func TestDamageBusyWhenDedupTableFull(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.MaxAppliedHits = 3 }
	w := startWorld(t, spec)
	target := npcID(61)
	w.spawn(CellID{0, 0}, EntityState{ID: target, X: 9800, Y: 5000, HP: 100})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	time.Sleep(3 * worldPeriod)
	for range 4 {
		w.attack(1, target, 1)
		time.Sleep(20 * time.Millisecond)
	}
	results := w.waitResults(4, 3*time.Second)
	busy := 0
	for _, r := range results {
		if r.Outcome == OutcomeRejected && r.Reason == ReasonBusy {
			busy++
		}
	}
	if busy != 1 || len(w.applications()) != 3 {
		t.Fatalf("busy %d, applications %d, results %+v; want 1 busy and 3 applied", busy, len(w.applications()), results)
	}
}
