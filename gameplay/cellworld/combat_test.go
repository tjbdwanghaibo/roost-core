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

// ---- 战斗面板（维护者 2026-11-04 第 3 项 = A，FEATURE §12.2）----
//
// 面板（最大 HP、攻击、战力）的权威在 Real：Base 从养成输入推出面板、带序号发给 owner，Real 只接受更新的序号。下面的用例
// 钉住：伤害用攻击者面板的攻击（没有面板时取请求里的缺省值）；当前 HP 按上限夹、面板变大不回血；面板命令丢失会重发、
// 重复与乱序只应用一次、交接中的命令改投新 owner；结论未知时照实报告且不重复报告；Base 的确认随 witness / 回写回来。

// refreshPanel 改 Base 上玩家的面板来源并让 Base 读一次（相当于 game 里升级 / 换装之后的那笔 Player 事务）。
func (w *world) refreshPanel(unique int64, p Panel) {
	w.t.Helper()
	w.base.player(unique).setCellPanel(p)
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.base.node.RefreshPanel(ctx, playerEntityID(unique)); err != nil {
		w.t.Fatal(err)
	}
}

// waitPanel 等到 cell 上 id 的 Real 报告的面板序号到 ver，返回那份记录。
func (w *world) waitPanel(cell CellID, id int64, ver uint64, what string) EntitySnapshot {
	w.t.Helper()
	var last EntitySnapshot
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := w.roleOf(cell, id); ok && s.Role == "real" {
			last = s
			if s.State.Panel.Ver >= ver {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("%s: real %d in %v has panel %+v hp %d, want panel ver %d", what, id, cell, last.State.Panel, last.State.HP, ver)
	return last
}

func (w *world) appliedPanels(id int64) []Panel {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Panel(nil), w.panels[id]...)
}

func (w *world) panelResults() []PanelResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]PanelResult(nil), w.panelRes...)
}

// 伤害取攻击者 Real 上面板的攻击（本格目标与跨格转发都是），没有面板的攻击者取请求里的缺省值；结论带实际伤害。
func TestDamageUsesTheAttackersPanel(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	near, across := npcID(71), npcID(72)
	w.spawn(a, EntityState{ID: near, X: 9800, Y: 5000, HP: 100})
	w.spawn(b, EntityState{ID: across, X: 10300, Y: 5000, HP: 100})
	w.base.player(1).setCellPanel(Panel{MaxHP: 200, Attack: 17, Power: 54})
	w.enter(1, spatial.Point{X: 9700, Y: 5000})
	w.enter(2, spatial.Point{X: 9650, Y: 5000}) // 没有面板来源
	time.Sleep(3 * worldPeriod)
	w.attack(1, near, 2)
	w.attack(1, across, 2)
	w.attack(2, across, 3)
	results := w.waitResults(3, 3*time.Second)
	got := map[[2]int64]int32{}
	for _, r := range results {
		if r.Outcome != OutcomeApplied || r.Duplicate {
			t.Fatalf("result %+v, want applied", r)
		}
		got[[2]int64{r.Hit.Attacker, r.Target}] = r.Damage
	}
	want := map[[2]int64]int32{{actorID(1), near}: 17, {actorID(1), across}: 17, {actorID(2), across}: 3}
	for k, d := range want {
		if got[k] != d {
			t.Errorf("attacker %d on %d: damage %d, want %d (results %+v)", k[0], k[1], got[k], d, results)
		}
	}
	waitFor(t, 2*time.Second, "hp settles", func() bool {
		sn, _ := w.roleOf(a, near)
		sa, _ := w.roleOf(b, across)
		return sn.State.HP == 83 && sa.State.HP == 80
	})
}

// 当前 HP 按面板上限夹住：进入时快照 HP 高于上限 → 夹到上限；上限变小 → 夹到新上限；上限变大 → 不回血；挨打照常扣。
// 结果随回写进 Base 的快照，客户端看到的自己带新面板。
func TestPanelCapsCurrentHPWithoutHealing(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) { cfg.WritebackEvery = 200 * time.Millisecond }
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.base.player(1).setCellPanel(Panel{MaxHP: 60, Attack: 5})
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	if s := w.waitPanel(b, actorID(1), 1, "enter"); s.State.HP != 60 || s.State.Panel.MaxHP != 60 {
		t.Fatalf("entered with snapshot hp 100 under max hp 60: hp %d panel %+v, want hp 60", s.State.HP, s.State.Panel)
	}
	w.refreshPanel(1, Panel{MaxHP: 40, Attack: 5})
	if s := w.waitPanel(b, actorID(1), 2, "lower max hp"); s.State.HP != 40 {
		t.Fatalf("max hp 60 -> 40: hp %d, want 40", s.State.HP)
	}
	w.refreshPanel(1, Panel{MaxHP: 200, Attack: 5})
	if s := w.waitPanel(b, actorID(1), 3, "raise max hp"); s.State.HP != 40 || s.State.Panel.MaxHP != 200 {
		t.Fatalf("max hp 40 -> 200: hp %d panel %+v, want hp 40 (no heal) under 200", s.State.HP, s.State.Panel)
	}
	w.base.player(2).setCellPanel(Panel{Attack: 9})
	w.enter(2, spatial.Point{X: 12200, Y: 5000})
	time.Sleep(3 * worldPeriod)
	w.attack(2, actorID(1), 1)
	w.waitResults(1, 3*time.Second)
	waitFor(t, 3*time.Second, "writeback of hp 31 under panel 3", func() bool {
		st := w.baseState(1)
		return st.HP == 31 && st.PanelAcked == 3
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		self, ok := w.base.client.held(1)[actorID(1)]
		if ok && self.Panel.MaxHP == 200 && self.Panel.Ver == 3 && self.HP == 31 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("client's own scene actor %+v (held %v), want panel ver 3 max hp 200 hp 31", self, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 面板命令丢失：Real 一直是旧面板、Base 没有确认；网络恢复后 Base 按重发间隔重发，Real 应用一次。
func TestPanelCommandLostIsResent(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var dropping atomic.Bool
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		return dropping.Load() && m.Kind == kindPanel, false, 0
	})
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.base.player(1).setCellPanel(Panel{MaxHP: 150, Attack: 4})
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	w.waitPanel(b, actorID(1), 1, "enter")
	dropping.Store(true)
	w.refreshPanel(1, Panel{MaxHP: 150, Attack: 11})
	time.Sleep(1200 * time.Millisecond)
	if s, _ := w.roleOf(b, actorID(1)); s.State.Panel.Ver != 1 {
		t.Fatalf("panel reached the real while every panel command was dropped: %+v", s.State.Panel)
	}
	if st := w.baseState(1); st.Panel.Ver != 2 || st.PanelAcked != 1 {
		t.Fatalf("base panel %+v acked %d, want ver 2 pending with ver 1 acked", st.Panel, st.PanelAcked)
	}
	dropping.Store(false)
	if s := w.waitPanel(b, actorID(1), 2, "resent panel"); s.State.Panel.Attack != 11 {
		t.Fatalf("resent panel %+v, want attack 11", s.State.Panel)
	}
	waitFor(t, 2*time.Second, "base acknowledges ver 2", func() bool { return w.baseState(1).PanelAcked == 2 })
	if got := w.appliedPanels(actorID(1)); len(got) != 1 || got[0].Ver != 2 {
		t.Fatalf("panels applied on the real %+v, want ver 2 exactly once", got)
	}
	if s := w.base.node.Stats(); s.PanelsSent < 2 {
		t.Fatalf("base sent %d panel commands, want the first plus resends", s.PanelsSent)
	}
}

// 面板命令全部重复、较早的一版晚到：每一版至多应用一次，晚到的旧版不覆盖新版。
func TestPanelCommandDuplicatedOrReorderedAppliesOnce(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind != kindPanel {
			return false, false, 0
		}
		if m.Panel.Panel.Ver == 2 {
			return false, true, 300 * time.Millisecond
		}
		return false, true, 0
	})
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.base.player(1).setCellPanel(Panel{MaxHP: 100, Attack: 3})
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	w.waitPanel(b, actorID(1), 1, "enter")
	w.refreshPanel(1, Panel{MaxHP: 100, Attack: 5})
	time.Sleep(20 * time.Millisecond)
	w.refreshPanel(1, Panel{MaxHP: 100, Attack: 8})
	w.waitPanel(b, actorID(1), 3, "latest panel")
	time.Sleep(700 * time.Millisecond) // 晚到的 ver 2（两份）已经到了
	if s, _ := w.roleOf(b, actorID(1)); s.State.Panel.Ver != 3 || s.State.Panel.Attack != 8 {
		t.Fatalf("real panel %+v after the late ver 2 arrived, want ver 3 attack 8", s.State.Panel)
	}
	got := w.appliedPanels(actorID(1))
	seen := map[uint64]int{}
	for i, p := range got {
		seen[p.Ver]++
		if i > 0 && p.Ver <= got[i-1].Ver {
			t.Errorf("panel ver %d applied after ver %d", p.Ver, got[i-1].Ver)
		}
	}
	for ver, n := range seen {
		if n != 1 {
			t.Errorf("panel ver %d applied %d times", ver, n)
		}
	}
	if seen[3] != 1 {
		t.Fatalf("panels applied %+v, want ver 3 once", got)
	}
	if s := w.cells[b].node.Stats(); s.PanelsDuplicate == 0 {
		t.Fatalf("cell stats %+v: the duplicated / late commands were not answered as duplicates", s)
	}
	waitFor(t, 2*time.Second, "base acknowledges ver 3", func() bool { return w.baseState(1).PanelAcked == 3 })
}

// 交接中改面板：命令发到正冻结的旧格（答“重试”），Base 看到新 owner 后改投，新格的 Real 应用它；之后的伤害用新攻击。
func TestPanelChangeDuringHandoff(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindOffer {
			return false, false, 300 * time.Millisecond
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	target := npcID(81)
	w.spawn(b, EntityState{ID: target, X: 10700, Y: 5000, HP: 100})
	w.base.player(1).setCellPanel(Panel{MaxHP: 100, Attack: 4})
	w.enter(1, spatial.Point{X: 9950, Y: 5000})
	w.move(1, 400, 0)
	waitFor(t, 3*time.Second, "frozen in A", func() bool { s, ok := w.roleOf(a, actorID(1)); return ok && s.Role == "frozen" })
	w.refreshPanel(1, Panel{MaxHP: 100, Attack: 13})
	w.move(1, 0, 0)
	if s := w.waitPanel(b, actorID(1), 2, "panel after the handoff"); s.State.Panel.Attack != 13 {
		t.Fatalf("panel in B %+v, want attack 13", s.State.Panel)
	}
	waitFor(t, 2*time.Second, "base acknowledges ver 2", func() bool { return w.baseState(1).PanelAcked == 2 })
	w.assertSingleReal(actorID(1), b)
	t.Logf("retries answered by the frozen cell: %d", w.cells[a].node.Stats().PanelRetries)
	w.attack(1, target, 1)
	results := w.waitResults(1, 3*time.Second)
	if len(results) != 1 || results[0].Damage != 13 {
		t.Fatalf("results %+v, want one hit of 13", results)
	}
}

// 结论未知：只丢结论时，witness 里的自己照样带回 Real 上的序号，Base 确认、不报未知；命令与结论都丢时，超过
// PanelDeadline 报一次未知（只报一次，Base 照旧重发），网络恢复后应用、确认。
func TestPanelUnknownWhenUnconfirmed(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var mode atomic.Int32 // 1：丢结论；2：命令与结论都丢
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		switch mode.Load() {
		case 1:
			return m.Kind == kindPanelResult, false, 0
		case 2:
			return m.Kind == kindPanelResult || m.Kind == kindPanel, false, 0
		}
		return false, false, 0
	})
	spec.baseMod = func(cfg *NodeConfig) {
		cfg.LeasePeriod = 200 * time.Millisecond
		cfg.PanelDeadline = 600 * time.Millisecond
	}
	w := startWorld(t, spec)
	b := CellID{1, 0}
	unknown := func(ver uint64) int {
		n := 0
		for _, r := range w.panelResults() {
			if r.Ver == ver && r.Outcome == OutcomeUnknown {
				n++
			}
		}
		return n
	}
	w.base.player(1).setCellPanel(Panel{MaxHP: 100, Attack: 3})
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	w.waitPanel(b, actorID(1), 1, "enter")
	mode.Store(1)
	w.refreshPanel(1, Panel{MaxHP: 100, Attack: 6})
	waitFor(t, 2*time.Second, "base acknowledges ver 2 from the witness", func() bool { return w.baseState(1).PanelAcked == 2 })
	time.Sleep(900 * time.Millisecond)
	if n := unknown(2); n != 0 {
		t.Fatalf("ver 2 reported unknown %d times although the witness confirmed it", n)
	}
	mode.Store(2)
	w.refreshPanel(1, Panel{MaxHP: 100, Attack: 9})
	time.Sleep(1500 * time.Millisecond)
	if n := unknown(3); n != 1 {
		t.Fatalf("ver 3 unconfirmed past the deadline: reported unknown %d times, want once (results %+v)", n, w.panelResults())
	}
	if s, _ := w.roleOf(b, actorID(1)); s.State.Panel.Ver != 2 {
		t.Fatalf("real panel %+v while every panel command was dropped, want ver 2", s.State.Panel)
	}
	mode.Store(0)
	w.waitPanel(b, actorID(1), 3, "panel after the network healed")
	waitFor(t, 2*time.Second, "base acknowledges ver 3", func() bool { return w.baseState(1).PanelAcked == 3 })
	if n := unknown(3); n != 1 {
		t.Fatalf("ver 3 reported unknown %d times in total, want once", n)
	}
}
