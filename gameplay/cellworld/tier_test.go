package cellworld

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// D1：附近没有玩家的 NPC 心跳降到慢档（IdlePeriod），玩家靠近（W 内有本格玩家 Real 或邻格玩家 Ghost）就回到 Period。
// 用例的慢档取 1s，降档滞后 300ms 以缩短用例。

const (
	testIdlePeriod = time.Second
	testIdleAfter  = 300 * time.Millisecond
)

func idleSpec(t testing.TB, cols, rows int32) (worldSpec, *loopback) {
	spec, lb := loopbackSpec(t, cols, rows)
	spec.cellMod = func(cfg *NodeConfig) {
		cfg.IdlePeriod, cfg.IdleAfter = testIdlePeriod, testIdleAfter
	}
	return spec, lb
}

// idleOf 返回实体在该格的快照里是否慢档（只看 Real）。
func (w *world) idleOf(cell CellID, id int64) (idle, real bool) {
	s, ok := w.roleOf(cell, id)
	if !ok || s.Role != "real" {
		return false, false
	}
	return s.State.Idle, true
}

func (w *world) waitIdle(cell CellID, id int64, want bool, timeout time.Duration) time.Time {
	w.t.Helper()
	waitFor(w.t, timeout, "npc tier", func() bool {
		idle, real := w.idleOf(cell, id)
		return real && idle == want
	})
	return time.Now()
}

// beatsOver 返回该格在 d 内执行的角色心跳数。
func (w *world) beatsOver(cell CellID, d time.Duration) uint64 {
	before := w.cells[cell].node.Stats().ActorBeats
	time.Sleep(d)
	return w.cells[cell].node.Stats().ActorBeats - before
}

// 没有玩家：NPC 降到慢档、心跳次数降到慢档的量级；邻格玩家越过边界走到 W 内（本格只看得见他的 Ghost）：升回快档；
// 玩家走远：滞后之后再降。
func TestIdleTierWithoutPlayersAndWakeOnApproach(t *testing.T) {
	spec, _ := idleSpec(t, 2, 1)
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	npc := npcID(21)
	w.spawn(a, EntityState{ID: npc, X: 9500, Y: 5000, HP: 100})
	w.waitIdle(a, npc, true, 3*time.Second)
	if beats := w.beatsOver(a, 2*time.Second); beats > 4 {
		t.Fatalf("idle npc beat %d times in 2s, want <= 4 (period %v)", beats, testIdlePeriod)
	}

	// 玩家在 B（离边界 1000），走到离 NPC 700：仍是 B 的 Real，A 只有他的 Ghost。
	w.enter(1, spatial.Point{X: 11000, Y: 5000})
	w.move(1, -1000, 0)
	w.waitView(1, 5*time.Second, "player near the npc", func(v View) bool { return v.Self.X <= 10200 })
	w.move(1, 0, 0)
	near := time.Now()
	if sid, _ := w.owner(1); sid != w.grid.SID(b) {
		t.Fatalf("player owned by %d, want B (%d): the npc must be woken by a ghost", sid, w.grid.SID(b))
	}
	woke := w.waitIdle(a, npc, false, 2*time.Second)
	t.Logf("npc fast %v after the player stopped 700 from it (it was woken while the player walked in)", woke.Sub(near))
	if beats := w.beatsOver(a, time.Second); beats < 15 {
		t.Fatalf("woken npc beat %d times in 1s, want >= 15 (period %v)", beats, worldPeriod)
	}

	// 玩家走远（离 NPC 2000）：滞后 IdleAfter 之后降档。
	w.move(1, 1000, 0)
	w.waitView(1, 5*time.Second, "player far away", func(v View) bool { return v.Self.X >= 11500 })
	w.move(1, 0, 0)
	w.waitIdle(a, npc, true, testIdleAfter+3*time.Second)
}

// 玩家在 W 边上来回（每 200ms 换向，一半时间在 W 内）：NPC 升档一次后保持快档，不来回换档；玩家离开后只降一次。
func TestIdleTierNoFlappingAtWakeEdge(t *testing.T) {
	spec, _ := idleSpec(t, 1, 1)
	w := startWorld(t, spec)
	a := CellID{0, 0}
	npc := npcID(22)
	w.spawn(a, EntityState{ID: npc, X: 5000, Y: 5000, HP: 100})
	w.waitIdle(a, npc, true, 3*time.Second)
	node := w.cells[a].node
	start := node.Stats()

	w.enter(1, spatial.Point{X: 5000 + worldViewRadius - 30, Y: 5000})
	w.waitIdle(a, npc, false, 2*time.Second)
	for i := range 20 {
		vx := int64(400)
		if i%2 == 1 {
			vx = -400
		}
		w.move(1, vx, 0) // ±80：在 W-30 与 W+50 之间来回
		time.Sleep(200 * time.Millisecond)
	}
	mid := node.Stats()
	if up, down := mid.TierFast-start.TierFast, mid.TierIdle-start.TierIdle; up != 1 || down != 0 {
		t.Fatalf("while the player hovered at the wake edge: %d promotions, %d demotions; want 1 and 0", up, down)
	}
	w.move(1, 1000, 0)
	w.waitView(1, 5*time.Second, "player far away", func(v View) bool { return v.Self.X >= 7500 })
	w.move(1, 0, 0)
	w.waitIdle(a, npc, true, testIdleAfter+3*time.Second)
	time.Sleep(2 * testIdlePeriod)
	end := node.Stats()
	if up, down := end.TierFast-start.TierFast, end.TierIdle-start.TierIdle; up != 1 || down != 1 {
		t.Fatalf("after the player left: %d promotions, %d demotions; want 1 and 1", up, down)
	}
}

// 慢档 NPC 的 Ghost 一个慢拍才更新一次：邻格不能在两次更新之间把它当过期删掉（GhostTTL 400ms < 慢档 1s）。
func TestIdleTierGhostSurvivesSlowBeats(t *testing.T) {
	spec, _ := idleSpec(t, 2, 1)
	idleMod := spec.cellMod
	spec.cellMod = func(cfg *NodeConfig) {
		idleMod(cfg)
		cfg.GhostTTL = 400 * time.Millisecond
	}
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	npc := npcID(23)
	w.spawn(a, EntityState{ID: npc, X: 9500, Y: 5000, HP: 100})
	w.waitIdle(a, npc, true, 3*time.Second)
	missing := 0
	for deadline := time.Now().Add(3 * testIdlePeriod); time.Now().Before(deadline); {
		if s, ok := w.roleOf(b, npc); !ok || s.Role != "ghost" {
			missing++
		}
		time.Sleep(25 * time.Millisecond)
	}
	if missing != 0 {
		t.Fatalf("ghost of the idle npc missing from B in %d samples over %v", missing, 3*testIdlePeriod)
	}
}

// 慢档 NPC 一拍走 800（> H，整拍积分后再判交接会在边界外 800 以内任意处冻结）冲过边界：按 Period 分步积分、每步判
// 交接，所以冻结点离边界不超过 H + 一步，旧格留下 Ghost；新格以慢档装入（档位随交接带走，不因交接回到快档）；全程
// 至多一个 Real。一拍超过边界带（1500 > G）的几何由 TestIdleBeatIntegratesWholePeriodAndFreezesInsideBand 覆盖。
func TestIdleTierFastCrossingHandsOffInsideBand(t *testing.T) {
	spec, lb := idleSpec(t, 2, 1)
	var offeredAt atomic.Int64 // 第一份 Offer 里的位置 = 冻结点
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindOffer {
			offeredAt.CompareAndSwap(0, m.Offer.State.X)
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	npc := npcID(24)
	w.spawn(a, EntityState{ID: npc, X: 7000, Y: 5000, VX: 800, HP: 100})
	w.waitIdle(a, npc, true, 2*time.Second)
	waitFor(t, 8*time.Second, "npc real in B", func() bool { s, ok := w.roleOf(b, npc); return ok && s.Role == "real" })
	step := int64(800) * int64(worldPeriod) / int64(time.Second)
	limit := 10000 + w.grid.Hysteresis() + step
	if x := offeredAt.Load(); x == 0 || x > limit {
		t.Fatalf("idle npc froze at x=%d, want x <= %d (boundary + H + one %v step)", x, limit, worldPeriod)
	}
	// 旧格在收到“已接受”的那笔事务里留下 Ghost，之后新格的慢拍更新它（一拍 800，仍在旧格边界带 1000 以内）。
	waitFor(t, testIdlePeriod/2, "ghost left in A", func() bool { g, ok := w.roleOf(a, npc); return ok && g.Role == "ghost" && g.Owner == b })
	if idle, _ := w.idleOf(b, npc); !idle {
		t.Fatal("npc handed over while idle was installed fast; the tier should travel with the handoff")
	}
	w.assertSingleReal(npc, b)
}

// 慢档 NPC 被跨界攻击（邻格发给本格的 Ghost 扣住 600ms，NPC 在头几次伤害到达时仍在慢档），同时它慢档走过边界、
// 消息 20% 丢失 / 10% 重复 / 乱序：每次伤害只结算一次，最终 HP 与结算次数一致。
func TestIdleTierDamageAcrossBoundaryAppliedOnce(t *testing.T) {
	spec, lb := idleSpec(t, 2, 1)
	var firstHit atomic.Int64
	idleMod := spec.cellMod
	spec.cellMod = func(cfg *NodeConfig) {
		idleMod(cfg)
		applied := cfg.Hooks.DamageApplied
		cfg.Hooks.DamageApplied = func(cell CellID, id int64, hit HitID, hp int32) {
			firstHit.CompareAndSwap(0, time.Now().UnixNano())
			applied(cell, id, hit, hp)
		}
	}
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	target := npcID(25)
	w.spawn(b, EntityState{ID: target, X: 10300, Y: 5000, VX: -400, HP: 100})
	w.waitIdle(b, target, true, 3*time.Second)

	lossy := lossyFault(5)
	var hold atomic.Int64
	toB := Subject("w", w.grid.SID(b))
	lb.setFault(func(subject string, m *wireMsg) (bool, bool, time.Duration) {
		drop, dup, d := lossy(subject, m)
		if subject == toB && m.Kind == kindGhost && time.Now().UnixNano() < hold.Load() {
			d += 600 * time.Millisecond
		}
		return drop, dup, d
	})
	// 记下 B 第一次升档的时间，与第一次结算比较。
	promoted := make(chan time.Time, 1)
	go func() {
		base := w.cells[b].node.Stats().TierFast
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if w.cells[b] != nil && w.cells[b].node.Stats().TierFast > base {
				promoted <- time.Now()
				return
			}
		}
	}()
	hold.Store(time.Now().Add(600 * time.Millisecond).UnixNano())
	w.enter(1, spatial.Point{X: 9600, Y: 5000})
	var hits []HitID
	for range 25 {
		hits = append(hits, w.attack(1, target, 2))
		time.Sleep(100 * time.Millisecond)
	}
	results := w.waitResults(len(hits), 5*time.Second)
	applications := w.applications()
	byHit := map[HitID]AttackResult{}
	for _, r := range results {
		if _, dup := byHit[r.Hit]; dup {
			t.Errorf("two results for %v", r.Hit)
		}
		byHit[r.Hit] = r
	}
	for hit, n := range applications {
		if n != 1 {
			t.Errorf("hit %v applied %d times", hit, n)
		}
	}
	for hit, r := range byHit {
		if r.Outcome == OutcomeApplied && applications[hit] != 1 {
			t.Errorf("hit %v reported applied but applied %d times", hit, applications[hit])
		}
	}
	select {
	case at := <-promoted:
		if first := firstHit.Load(); first == 0 || first > at.UnixNano() {
			t.Fatalf("first damage applied at %v, npc promoted at %v: no damage reached the idle npc", time.Unix(0, first), at)
		}
	case <-time.After(time.Second):
		t.Fatal("npc never promoted while the player stood next to it")
	}
	time.Sleep(3 * worldPeriod)
	var hp int32 = -1
	for _, cell := range []CellID{a, b} {
		for _, s := range w.inspect(cell) {
			if s.State.ID == target && s.Role == "real" {
				hp = s.State.HP
			}
		}
	}
	if want := int32(100 - 2*len(applications)); hp != want {
		t.Fatalf("target hp = %d, want %d (applications %d)", hp, want, len(applications))
	}
	t.Logf("results %d, applications %d", len(byHit), len(applications))
}

// advance 单元：慢档一拍按 Period 分步走满整个慢档周期；中途越界就停在那一步冻结。
func TestIdleBeatIntegratesWholePeriodAndFreezesInsideBand(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	cell := CellID{0, 0}
	n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, IdlePeriod: testIdlePeriod, ViewRadius: worldViewRadius}.withDefaults(), cell: cell, hostsCell: true}
	n.rect = grid.Rect(cell)
	now := time.Now()

	st := ActorState{EntityState: EntityState{ID: npcID(26), Kind: KindNPC, X: 5000, Y: 5000, VX: 1000, HP: 100, Idle: true, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&st, now, testIdlePeriod)
	if st.X != 6000 || st.Role != RoleReal {
		t.Fatalf("idle beat moved npc to x=%d role %v, want x=6000 (one %v at 1000/s) and still real", st.X, st.Role, testIdlePeriod)
	}

	st = ActorState{EntityState: EntityState{ID: npcID(27), Kind: KindNPC, X: 9500, Y: 5000, VX: 1500, HP: 100, Idle: true, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	fx := n.stepActor(&st, now, testIdlePeriod)
	limit := 10000 + grid.Hysteresis() + 1500*int64(worldPeriod)/int64(time.Second)
	if st.Role != RoleFrozen || st.Handoff == nil || st.Handoff.Target != (CellID{1, 0}) {
		t.Fatalf("idle beat across the boundary: role %v handoff %+v, want frozen towards (1,0)", st.Role, st.Handoff)
	}
	if st.X < 10000+grid.Hysteresis() || st.X > limit {
		t.Fatalf("froze at x=%d, want in [%d, %d]", st.X, 10000+grid.Hysteresis(), limit)
	}
	if len(fx.sends) != 1 || fx.sends[0].msg.Kind != kindOffer || !fx.sends[0].msg.Offer.State.Idle {
		t.Fatalf("offer %+v, want one offer carrying the idle tier", fx.sends)
	}
}

// 积分按经过时间，不按回调次数（docs/framework/impl/10-time.md）：心跳驱动变慢、跳拍时（Beat.Elapsed 是三个周期），
// 快档角色一拍走完三个周期的位移；中途越界时仍在走到边界带的那一步冻结（离本格至多 H + 一个 Period 的位移）。
// Elapsed 为 0（同一刻的重复拍）不移动。
func TestActorIntegratesTheElapsedTimeNotTheBeatCount(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	cell := CellID{0, 0}
	n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, ViewRadius: worldViewRadius}.withDefaults(), cell: cell, hostsCell: true}
	n.rect = grid.Rect(cell)
	period := n.cfg.Period
	now := time.Now()

	st := ActorState{EntityState: EntityState{ID: npcID(30), Kind: KindNPC, X: 2000, Y: 5000, VX: 1000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&st, now, 3*period)
	if want := 2000 + 3*1000*int64(period)/int64(time.Second); st.X != want {
		t.Fatalf("a beat that covered %v moved the npc to x=%d, want x=%d (1000/s for the whole time, not one %v step)", 3*period, st.X, want, period)
	}
	x := st.X
	n.stepActor(&st, now, 0)
	if st.X != x {
		t.Fatalf("a beat with no elapsed time moved the npc from %d to %d", x, st.X)
	}

	st = ActorState{EntityState: EntityState{ID: npcID(31), Kind: KindNPC, X: 9950, Y: 5000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&st, now, 10*period)
	limit := 10000 + grid.Hysteresis() + 2000*int64(period)/int64(time.Second)
	if st.Role != RoleFrozen || st.X < 10000+grid.Hysteresis() || st.X > limit {
		t.Fatalf("a long beat across the boundary: role %v at x=%d, want frozen in [%d, %d]", st.Role, st.X, 10000+grid.Hysteresis(), limit)
	}
}
