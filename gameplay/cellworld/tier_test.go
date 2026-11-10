package cellworld

import (
	"math/rand/v2"
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
	w.spawn(a, EntityState{ID: npc, X: 9800, Y: 5000, HP: 100})
	w.waitIdle(a, npc, true, 3*time.Second)
	if beats := w.beatsOver(a, 2*time.Second); beats > 4 {
		t.Fatalf("idle npc beat %d times in 2s, want <= 4 (period %v)", beats, testIdlePeriod)
	}

	// 玩家在 B（离边界 1000），走到离 NPC 700 左右：仍是 B 的 Real，A 只有他的 Ghost。看到 witness 才停，停之前玩家按
	// 实际经过的时间照常走（控制延迟，负载下可能赶上一拍被夹到 1s 的经过时间），所以停点离 B 交给 A 的线（9900）留 600，
	// 400/s 下约 1.5s。
	w.enter(1, spatial.Point{X: 11000, Y: 5000})
	w.move(1, -400, 0)
	w.waitView(1, 5*time.Second, "player near the npc", func(v View) bool { return v.Self.X <= 10500 })
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
	w.waitView(1, 5*time.Second, "player far away", func(v View) bool { return v.Self.X >= 11800 })
	w.move(1, 0, 0)
	w.waitIdle(a, npc, true, testIdleAfter+3*time.Second)
}

// 玩家在 W 边上来回（W-30 与 W+50 之间，一部分时间在 W 外）：NPC 升档一次后保持快档，不来回换档；玩家离开后只降一次。
//
// 承诺是“在 W 外连续不到 IdleAfter 就回来，不降档”。换向看 witness 里玩家自己的位置（闭环）：按 sleep 定时换向是开环，
// 每段的实际时长差（输入一跳的延迟抖动）会累积成位置漂移，漂出 W 后玩家就真的连续在外面了。闭环时每次在 W 外的时长约
// 是走 50 再回来（250ms）加两倍控制延迟（witness 一跳、测试轮询、输入一跳；位置按心跳实际经过的时间积分，负载下还可能
// 赶上一拍被夹到 1s 的经过时间），所以本用例的 IdleAfter 取生产默认 2s，与这段延迟相称；300ms 的用例滞后只够 50ms 的
// 余量（见 docs/maintenance/evidence/cellworld-realclock-flakes-20261110）。滞后的另一面（连续 IdleAfter 没人才降）由
// 用例结尾的降档与 TestIdleTierWithoutPlayersAndWakeOnApproach 覆盖。
func TestIdleTierNoFlappingAtWakeEdge(t *testing.T) {
	const idleAfter = 2 * time.Second
	spec, _ := idleSpec(t, 1, 1)
	idleMod := spec.cellMod
	spec.cellMod = func(cfg *NodeConfig) {
		idleMod(cfg)
		cfg.IdleAfter = idleAfter
	}
	w := startWorld(t, spec)
	a := CellID{0, 0}
	npc := npcID(22)
	npcAt := spatial.Point{X: 5000, Y: 5000}
	w.spawn(a, EntityState{ID: npc, X: npcAt.X, Y: npcAt.Y, HP: 100})
	w.waitIdle(a, npc, true, idleAfter+3*time.Second)
	node := w.cells[a].node
	start := node.Stats()

	edge := npcAt.X + worldViewRadius // W 取缺省 = ViewRadius；NPC 静止，没有漂移余量
	w.enter(1, spatial.Point{X: edge - 30, Y: 5000})
	w.waitIdle(a, npc, false, 2*time.Second)
	w.resetViews(1)
	for i := range 20 {
		vx := int64(400)
		if i%2 == 1 {
			vx = -400
		}
		w.move(1, vx, 0)
		w.waitView(1, 5*time.Second, "turn at the wake edge", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= edge+50
			}
			return v.Self.X <= edge-30
		})
	}
	mid := node.Stats()
	if up, down := mid.TierFast-start.TierFast, mid.TierIdle-start.TierIdle; up != 1 || down != 0 {
		// 在 W 外连续够 IdleAfter 时降档是正确行为：用 witness 估计玩家最长一次在 W 外待了多久，区分“用例失去控制”与“抖动”。
		outside := longestOutside(w.viewsOf(1), npcAt, worldViewRadius)
		if outside >= idleAfter {
			t.Fatalf("while the player hovered at the wake edge: %d promotions, %d demotions, but the witness shows it outside W for %v >= IdleAfter %v: control latency kept it out", up, down, outside, idleAfter)
		}
		t.Fatalf("while the player hovered at the wake edge: %d promotions, %d demotions; want 1 and 0 (longest stretch outside W in the witness %v < IdleAfter %v)", up, down, outside, idleAfter)
	}
	w.move(1, 1000, 0)
	w.waitView(1, 5*time.Second, "player far away", func(v View) bool { return v.Self.X >= 7500 })
	w.move(1, 0, 0)
	w.waitIdle(a, npc, true, idleAfter+3*time.Second)
	time.Sleep(2 * testIdlePeriod)
	end := node.Stats()
	if up, down := end.TierFast-start.TierFast, end.TierIdle-start.TierIdle; up != 1 || down != 1 {
		t.Fatalf("after the player left: %d promotions, %d demotions; want 1 and 1", up, down)
	}
}

// longestOutside 按 witness 估计玩家最长一次连续离 p 超过 r 的时长：从第一份在外面的 witness 到下一份回到里面的 witness
// （到最后一份仍在外面时算到最后一份）。witness 一拍一份，估计误差约一拍。
func longestOutside(views []View, p spatial.Point, r int64) time.Duration {
	var longest time.Duration
	var since time.Time
	for _, v := range views {
		out := !spatial.WithinDistance(v.Self.Pos(), p, r)
		switch {
		case out && since.IsZero():
			since = v.At
		case !out && !since.IsZero():
			longest = max(longest, v.At.Sub(since))
			since = time.Time{}
		}
	}
	if !since.IsZero() && len(views) > 0 {
		longest = max(longest, views[len(views)-1].At.Sub(since))
	}
	return longest
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

// ---- 地面钩子（NodeConfig.Surface，可编辑地形 P3 的跨格移动）----

// stripeGround 是测试用的带高度地面：地面格 100 单位一格（四舍五入归格，与 terrain.Walker 的 1m 节点同形），
// blocked 里的格不可进入，speed 里的格是减速格（千分比），undefined 里的格没有定义（块没装入）。Step 不相邻时答 false，
// 用来证明积分每次至多跨到相邻的格。
type stripeGround struct {
	blocked, undefined map[spatial.Point]bool
	speed              map[spatial.Point]int32
	steps              atomic.Int64
	jumps              atomic.Int64
}

func (g *stripeGround) Cell(p spatial.Point) spatial.Point {
	floor := func(v int64) int64 {
		q := (v + 50) / 100
		if (v+50)%100 < 0 {
			q--
		}
		return q
	}
	return spatial.Point{X: floor(p.X), Y: floor(p.Y)}
}

func (g *stripeGround) Step(from, to spatial.Point) (int32, bool) {
	g.steps.Add(1)
	if g.undefined[from] {
		return 0, false
	}
	if dx, dy := to.X-from.X, to.Y-from.Y; dx < -1 || dx > 1 || dy < -1 || dy > 1 {
		g.jumps.Add(1)
		return 0, false
	}
	if g.blocked[to] || g.undefined[to] {
		return 0, false
	}
	if s, ok := g.speed[to]; ok {
		return s, true
	}
	return 1000, true
}

// openGround 处处放行、速度 1000。
type openGround struct{}

func (openGround) Cell(p spatial.Point) spatial.Point    { return p }
func (openGround) Step(_, _ spatial.Point) (int32, bool) { return 1000, true }

func surfaceNode(t testing.TB, ground func() Surface) *Node {
	grid := worldGrid(t, 2, 1)
	cell := CellID{0, 0}
	n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, ViewRadius: worldViewRadius, Surface: ground}.withDefaults(), cell: cell, hostsCell: true}
	n.rect = grid.Rect(cell)
	return n
}

// 没有地面（Surface 不设）、Surface 返回 nil、处处放行且速度 1000 的地面三者逐位相同：位置、零头（FracX / FracY）、
// 速度与身份（按整数点走的终点就是没有地面时的终点，世界边缘的速度规则照旧）。随机的位置、速度（含碰世界边缘、越过格
// 边界冻结）与拍长，NPC 与玩家。承诺没有变：地面钩子只在被挡、减速时改变结果；与引入千分位零头之前相比，三者一起多了
// 不丢截断余数这一处变化（由 TestIntegrationKeepsTheSubUnitRemainder 断言），彼此仍逐位相同。
func TestANilSurfaceIntegratesExactlyLikeNoSurface(t *testing.T) {
	plain := surfaceNode(t, nil)
	viaNil := surfaceNode(t, func() Surface { return nil })
	open := surfaceNode(t, func() Surface { return openGround{} })
	rng := rand.New(rand.NewPCG(11, 17))
	now := time.Now()
	for i := range 2000 {
		kind := KindNPC
		if i%2 == 1 {
			kind = KindPlayer
		}
		st := ActorState{EntityState: EntityState{ID: npcID(int64(100 + i)), Kind: kind, X: rng.Int64N(10400), Y: rng.Int64N(10000),
			VX: rng.Int64N(4001) - 2000, VY: rng.Int64N(4001) - 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
		elapsed := time.Duration(rng.Int64N(int64(8 * worldPeriod)))
		a, b, c := st, st, st
		for beat := range 4 {
			plain.stepActor(&a, now, elapsed)
			viaNil.stepActor(&b, now, elapsed)
			open.stepActor(&c, now, elapsed)
			same := func(p, q ActorState) bool {
				return p.EntityState == q.EntityState && p.Role == q.Role && p.FracX == q.FracX && p.FracY == q.FracY
			}
			if !same(a, b) || !same(a, c) {
				t.Fatalf("case %d beat %d (elapsed %v): no surface %+v %v frac (%d, %d), nil surface %+v %v frac (%d, %d), open surface %+v %v frac (%d, %d)",
					i, beat, elapsed, a.EntityState, a.Role, a.FracX, a.FracY, b.EntityState, b.Role, b.FracX, b.FracY, c.EntityState, c.Role, c.FracX, c.FracY)
			}
		}
	}
	if s := plain.Stats().SurfaceBlocks + viaNil.Stats().SurfaceBlocks + open.Stats().SurfaceBlocks; s != 0 {
		t.Fatalf("surface blocks %d without a refusing surface", s)
	}
}

// 地面不放行下一格时停在上一个整数点：玩家速度清零（与停在世界边缘相同），NPC 原路反向（与碰世界边缘反弹相同）、
// 下一拍往回走；对角走时每次至多跨到相邻的格（Step 从不被问到不相邻的格）。
func TestSurfaceStopsTheWalkAtTheLastPointBeforeARefusedCell(t *testing.T) {
	ground := &stripeGround{blocked: map[spatial.Point]bool{}}
	for y := int64(-5); y <= 105; y++ {
		ground.blocked[spatial.Point{X: 12, Y: y}] = true // x ∈ [1150, 1250)
	}
	n := surfaceNode(t, func() Surface { return ground })
	now := time.Now()

	player := ActorState{EntityState: EntityState{ID: actorID(1), Kind: KindPlayer, X: 1000, Y: 5000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&player, now, 4*worldPeriod)
	if player.X != 1149 || player.Y != 5000 || player.VX != 0 || player.VY != 0 || player.Role != RoleReal {
		t.Fatalf("player walking into the refused cell: at (%d, %d) v (%d, %d) %v, want stopped at (1149, 5000) with v 0", player.X, player.Y, player.VX, player.VY, player.Role)
	}
	n.stepActor(&player, now, 4*worldPeriod)
	if player.X != 1149 {
		t.Fatalf("a stopped player moved on to x=%d", player.X)
	}

	npc := ActorState{EntityState: EntityState{ID: npcID(2), Kind: KindNPC, X: 1000, Y: 5000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&npc, now, worldPeriod)
	n.stepActor(&npc, now, worldPeriod)
	if npc.X != 1149 || npc.VX != -2000 {
		t.Fatalf("npc walking into the refused cell: at x=%d vx %d, want stopped at 1149 and turned back (vx -2000)", npc.X, npc.VX)
	}
	n.stepActor(&npc, now, worldPeriod)
	if npc.X != 1049 || npc.VX != -2000 {
		t.Fatalf("npc after turning back: at x=%d vx %d, want 1049 walking west", npc.X, npc.VX)
	}

	diag := ActorState{EntityState: EntityState{ID: actorID(3), Kind: KindPlayer, X: 1000, Y: 1000, VX: 1500, VY: 1100, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&diag, now, 6*worldPeriod)
	if diag.X != 1149 || diag.VX != 0 {
		t.Fatalf("diagonal walk into the refused column: at (%d, %d) v (%d, %d), want stopped at x 1149", diag.X, diag.Y, diag.VX, diag.VY)
	}
	if j := ground.jumps.Load(); j != 0 {
		t.Fatalf("the walk asked Step about %d non-adjacent cells", j)
	}
	if n.Stats().SurfaceBlocks == 0 {
		t.Fatal("blocks are not counted")
	}
}

// 减速格（游泳）里同样的时间走得近：速度 500 的格里一拍走一半的路，速度不变；站立的格没有定义（块没装入）时一步
// 不走（不猜平地），玩家速度清零。
func TestSurfaceSlowsTheWalkAndAnUndefinedCellHoldsIt(t *testing.T) {
	water := map[spatial.Point]int32{}
	for x := int64(20); x <= 40; x++ {
		water[spatial.Point{X: x, Y: 50}] = 500
	}
	ground := &stripeGround{speed: water, undefined: map[spatial.Point]bool{{X: 60, Y: 50}: true}}
	n := surfaceNode(t, func() Surface { return ground })
	now := time.Now()
	swim := ActorState{EntityState: EntityState{ID: actorID(4), Kind: KindPlayer, X: 3000, Y: 5000, VX: 1000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&swim, now, 4*worldPeriod)
	if swim.X != 3100 || swim.VX != 1000 {
		t.Fatalf("swimming 200ms at 1000/s with speed 500‰: at x=%d vx %d, want 3100 (half the way) and the speed kept", swim.X, swim.VX)
	}
	// 朝世界边缘（y 0）游：一拍的位移本会被夹到边缘、NPC 反弹，但减速后没走到边缘——速度不变，世界边缘的规则只在走到
	// 终点时生效；路程按没夹过的位移算，够走到边缘的那一拍才反弹。
	for y := int64(0); y <= 2; y++ {
		water[spatial.Point{X: 30, Y: y}] = 500
	}
	edge := ActorState{EntityState: EntityState{ID: npcID(7), Kind: KindNPC, X: 3000, Y: 140, VY: -2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	for i, want := range []struct{ y, vy int64 }{{90, -2000}, {40, -2000}, {0, 2000}} {
		n.stepActor(&edge, now, worldPeriod)
		if edge.Y != want.y || edge.VY != want.vy {
			t.Fatalf("npc swimming towards the world edge, beat %d: at y=%d vy %d, want y=%d vy %d", i, edge.Y, edge.VY, want.y, want.vy)
		}
	}
	stuck := ActorState{EntityState: EntityState{ID: actorID(5), Kind: KindPlayer, X: 6000, Y: 5000, VX: 1000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&stuck, now, 4*worldPeriod)
	if stuck.X != 6000 || stuck.VX != 0 {
		t.Fatalf("standing on an undefined cell: at x=%d vx %d, want not moved and stopped", stuck.X, stuck.VX)
	}
}

// 越过格边界之前被地面挡住就不交接：NPC 朝东走向边界（x 10000，冻结线 10100），挡在边界前一格时原路折回，一直是
// 本格的 Real；不挡时同样的拍冻结、交给 (1,0)。
func TestSurfaceBlockBeforeTheBoundaryKeepsTheActorReal(t *testing.T) {
	ground := &stripeGround{blocked: map[spatial.Point]bool{}}
	for y := int64(0); y <= 100; y++ {
		ground.blocked[spatial.Point{X: 100, Y: y}] = true // x ∈ [9950, 10050)
	}
	now := time.Now()
	for _, tc := range []struct {
		name   string
		ground func() Surface
		frozen bool
	}{{"blocked", func() Surface { return ground }, false}, {"open", func() Surface { return openGround{} }, true}} {
		n := surfaceNode(t, tc.ground)
		st := ActorState{EntityState: EntityState{ID: npcID(6), Kind: KindNPC, X: 9800, Y: 5000, VX: 2000, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
		n.stepActor(&st, now, 10*worldPeriod)
		if frozen := st.Role == RoleFrozen; frozen != tc.frozen {
			t.Fatalf("%s: role %v at x=%d, want frozen %v", tc.name, st.Role, st.X, tc.frozen)
		}
		if !tc.frozen && (st.X >= 9950 || st.VX != -2000) {
			t.Fatalf("%s: at x=%d vx %d, want turned back before 9950", tc.name, st.X, st.VX)
		}
	}
}

// uniformGround 处处放行、速度都是 speed 千分比（地面格就是整数点本身）。
type uniformGround struct{ speed int32 }

func (g uniformGround) Cell(p spatial.Point) spatial.Point    { return p }
func (g uniformGround) Step(_, _ spatial.Point) (int32, bool) { return g.speed, true }

// 积分不丢不足 1 单位的零头（维护者 2026-11-10：位移按千分位整数计，零头留在角色状态里、下一子步接着累加）：速度固定、
// 拍长不是 Period 的整数倍时，总位移与 速度 × 总时间（减速的地面再乘速度千分比）相差不到 1 单位；每拍不足 1 单位的
// 低速同样走；负速度、对角，没有地面、平地与游泳都一样。拍长取整毫秒，千分位的位移没有截断，误差只剩零头本身（< 1）。
func TestIntegrationKeepsTheSubUnitRemainder(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	irregular := make([]time.Duration, 40)
	for i := range irregular {
		irregular[i] = time.Duration(31+rng.IntN(100)) * time.Millisecond // 31～130ms：多数不是 50ms 的整数倍
	}
	steady := make([]time.Duration, 200)
	for i := range steady {
		steady[i] = worldPeriod
	}
	grounds := []struct {
		name    string
		surface func() Surface
		permil  int64
	}{
		{"none", nil, 1000},
		{"open", func() Surface { return openGround{} }, 1000},
		{"swim", func() Surface { return uniformGround{speed: 500} }, 500},
	}
	cases := []struct {
		name   string
		vx, vy int64
		beats  []time.Duration
	}{
		{"400/s irregular beats", 400, 0, irregular},
		{"diagonal backwards irregular beats", -400, 130, irregular},
		{"10/s irregular beats", 10, 0, irregular},
		{"10/s for 10s in period beats", 0, -10, steady},
		{"slow diagonal backwards", -6, 4, irregular},
	}
	now := time.Now()
	for _, g := range grounds {
		n := surfaceNode(t, g.surface)
		for _, tc := range cases {
			kinds := []EntityKind{KindPlayer, KindNPC}
			for _, kind := range kinds {
				st := ActorState{EntityState: EntityState{ID: npcID(40), Kind: kind, X: 5000, Y: 5000, VX: tc.vx, VY: tc.vy, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
				var total time.Duration
				for _, e := range tc.beats {
					n.stepActor(&st, now, e)
					total += e
				}
				ms := int64(total / time.Millisecond)
				// 千分位：速度 × 毫秒 = 千分之一单位；减速地面再乘千分比。
				wantX, wantY := tc.vx*ms*g.permil/1000, tc.vy*ms*g.permil/1000
				gotX, gotY := (st.X-5000)*1000, (st.Y-5000)*1000
				if st.Role != RoleReal || abs64(gotX-wantX) >= 1000 || abs64(gotY-wantY) >= 1000 {
					t.Errorf("%s / %s / kind %d: v (%d, %d) for %v moved (%d, %d), want (%.3f, %.3f) within 1 unit (role %v)",
						g.name, tc.name, kind, tc.vx, tc.vy, total, st.X-5000, st.Y-5000, float64(wantX)/1000, float64(wantY)/1000, st.Role)
				}
			}
		}
	}
}

// 拍长带纳秒抖动时（真实心跳的 Beat.Elapsed），每个子步只在千分位上截断，丢掉不到 1 千分位：长时间积分后，位置加零头
// 与 速度 × 时间 相差不到子步数个千分位；整数位置与之相差不到 1 单位加这一点。
func TestIntegrationLosesLessThanAThousandthPerSubStep(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	n := surfaceNode(t, nil)
	now := time.Now()
	for _, v := range []struct{ vx, vy int64 }{{400, 0}, {-400, 37}, {3, -11}} {
		st := ActorState{EntityState: EntityState{ID: npcID(41), Kind: KindNPC, X: 5000, Y: 5000, VX: v.vx, VY: v.vy, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
		var total time.Duration
		var subSteps int64
		for range 200 {
			e := worldPeriod - 3*time.Millisecond + time.Duration(rng.Int64N(int64(6*time.Millisecond)))
			n.stepActor(&st, now, e)
			total += e
			subSteps += int64((e + worldPeriod - 1) / worldPeriod)
		}
		if st.Role != RoleReal {
			t.Fatalf("v (%d, %d): %v at (%d, %d); the case must stay inside the cell", v.vx, v.vy, st.Role, st.X, st.Y)
		}
		for _, axis := range []struct {
			name      string
			pos, frac int64
			v         int64
		}{{"x", st.X, int64(st.FracX), v.vx}, {"y", st.Y, int64(st.FracY), v.vy}} {
			want := axis.v * int64(total) / int64(time.Millisecond) // 千分位
			got := (axis.pos-5000)*milli + axis.frac
			if d := abs64(got - want); d >= subSteps {
				t.Errorf("v (%d, %d) for %v, axis %s: position + remainder %d thousandths, want %d within %d sub-steps", v.vx, v.vy, total, axis.name, got, want, subSteps)
			}
			if d := abs64((axis.pos-5000)*milli - want); d >= milli+subSteps {
				t.Errorf("v (%d, %d) for %v, axis %s: integer position off by %d thousandths", v.vx, v.vy, total, axis.name, d)
			}
		}
	}
}

// 零头的符号与速度改变：零头与累计的位移同号，往负方向走时整数位置在真实位置的正侧、走满 1 单位才变；新输入改了速度
// （含反向）零头不动，接着从真实位置累加。
func TestTheRemainderKeepsItsSignAndSurvivesASpeedChange(t *testing.T) {
	n := surfaceNode(t, nil)
	now := time.Now()
	st := ActorState{EntityState: EntityState{ID: actorID(42), Kind: KindPlayer, X: 5000, Y: 5000, VX: -10, VY: 4, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	n.stepActor(&st, now, 30*time.Millisecond) // x 真实 4999.7，y 5000.12
	if st.X != 5000 || st.FracX != -300 || st.Y != 5000 || st.FracY != 120 {
		t.Fatalf("after 30ms at (-10, 4)/s: (%d %+d, %d %+d), want (5000 -300, 5000 +120)", st.X, st.FracX, st.Y, st.FracY)
	}
	n.stepActor(&st, now, 80*time.Millisecond) // x 真实 4998.9
	if st.X != 4999 || st.FracX != -100 {
		t.Fatalf("after 110ms at -10/s: x %d %+d, want 4999 -100 (a whole unit walked moves the integer position)", st.X, st.FracX)
	}
	st.VX, st.VY = 10, 0                       // 新的移动输入：反向
	n.stepActor(&st, now, 50*time.Millisecond) // x 真实 4999.4
	if st.X != 4999 || st.FracX != 400 || st.Y != 5000 || st.FracY != 440 {
		t.Fatalf("after turning round for 50ms: (%d %+d, %d %+d), want (4999 +400, 5000 +440): the remainder is position, not speed", st.X, st.FracX, st.Y, st.FracY)
	}
	n.stepActor(&st, now, 60*time.Millisecond) // x 真实 5000.0
	if st.X != 5000 || st.FracX != 0 {
		t.Fatalf("x %d %+d, want exactly 5000", st.X, st.FracX)
	}
}

// 世界边缘：真实位置出了 [Min, Max-1]（整数位置越界，或恰在边上而零头指向界外）就停在边上、这条轴的零头清零；玩家这条
// 轴的速度清零、NPC 反向，另一条轴的零头照旧。真实位置恰好落在边上不算越界。没有地面与平地相同。
func TestTheRemainderStopsAtTheWorldEdge(t *testing.T) {
	now := time.Now()
	for _, g := range []struct {
		name    string
		surface func() Surface
	}{{"none", nil}, {"open", func() Surface { return openGround{} }}} {
		n := surfaceNode(t, g.surface)
		hi := n.cfg.Grid.Bounds().Max.X - 1
		for _, kind := range []EntityKind{KindPlayer, KindNPC} {
			wantV := func(v int64) int64 {
				if kind == KindNPC {
					return -v
				}
				return 0
			}
			st := ActorState{EntityState: EntityState{ID: npcID(43), Kind: kind, X: 1, Y: 5000, VX: -10, VY: 6, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
			n.stepActor(&st, now, 100*time.Millisecond) // 真实 x 正好 0
			if st.X != 0 || st.FracX != 0 || st.VX != -10 {
				t.Fatalf("%s kind %d: reaching x 0 exactly: x %d %+d vx %d, want 0 +0 still -10", g.name, kind, st.X, st.FracX, st.VX)
			}
			n.stepActor(&st, now, 50*time.Millisecond) // 真实 x -0.5：越界
			if st.X != 0 || st.FracX != 0 || st.VX != wantV(-10) || st.Y != 5000 || st.FracY != 900 || st.VY != 6 {
				t.Fatalf("%s kind %d: past the low edge: (%d %+d, %d %+d) v (%d, %d), want (0 +0, 5000 +900) v (%d, 6)",
					g.name, kind, st.X, st.FracX, st.Y, st.FracY, st.VX, st.VY, wantV(-10))
			}
			st = ActorState{EntityState: EntityState{ID: npcID(44), Kind: kind, X: hi, Y: 5000, VX: 10, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
			n.stepActor(&st, now, 50*time.Millisecond) // 真实 x hi + 0.5：越界
			if st.X != hi || st.FracX != 0 || st.VX != wantV(10) {
				t.Fatalf("%s kind %d: past the high edge: x %d %+d vx %d, want %d +0 vx %d", g.name, kind, st.X, st.FracX, st.VX, hi, wantV(10))
			}
		}
	}
}

// 地面挡住：停在上一个合法整数点，两条轴的零头都清零（玩家速度清零、NPC 原路反向）；站立的地面格没有定义时同样不动、
// 零头清零。
func TestASurfaceBlockClearsTheRemainder(t *testing.T) {
	ground := &stripeGround{blocked: map[spatial.Point]bool{}, undefined: map[spatial.Point]bool{{X: 60, Y: 50}: true}}
	for y := int64(-5); y <= 105; y++ {
		ground.blocked[spatial.Point{X: 12, Y: y}] = true // x ∈ [1150, 1250)
	}
	n := surfaceNode(t, func() Surface { return ground })
	now := time.Now()
	for _, kind := range []EntityKind{KindPlayer, KindNPC} {
		st := ActorState{EntityState: EntityState{ID: npcID(45), Kind: kind, X: 1149, Y: 5000, VX: 10, VY: 4, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal, FracX: 600, FracY: 300}
		n.stepActor(&st, now, 50*time.Millisecond) // 真实 x 1149.6 → 1150.1：进入被挡的格
		wantVX, wantVY := int64(0), int64(0)
		if kind == KindNPC {
			wantVX, wantVY = -10, -4
		}
		if st.X != 1149 || st.Y != 5000 || st.FracX != 0 || st.FracY != 0 || st.VX != wantVX || st.VY != wantVY {
			t.Fatalf("kind %d into the refused cell: (%d %+d, %d %+d) v (%d, %d), want (1149 +0, 5000 +0) v (%d, %d)",
				kind, st.X, st.FracX, st.Y, st.FracY, st.VX, st.VY, wantVX, wantVY)
		}
	}
	stuck := ActorState{EntityState: EntityState{ID: actorID(46), Kind: KindPlayer, X: 6000, Y: 5000, VX: 10, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal, FracX: 700}
	n.stepActor(&stuck, now, 50*time.Millisecond)
	if stuck.X != 6000 || stuck.FracX != 0 || stuck.VX != 0 {
		t.Fatalf("standing on an undefined cell: x %d %+d vx %d, want 6000 +0 and stopped", stuck.X, stuck.FracX, stuck.VX)
	}
}

// 游泳：减速格里位移按速度千分比缩短、零头照常累计，进出减速格时从进入新格的那个整数点起把剩下的位移按新旧速度之比
// 缩放（进入新格的那一步按旧格的速度算）：从陆地游进水里、从水里走上陆地，位置都与分段的 速度 × 时间 相差不到 1 千分位。
func TestSwimmingAcrossASpeedChangeKeepsTheRemainder(t *testing.T) {
	water := map[spatial.Point]int32{}
	for x := int64(20); x <= 40; x++ {
		water[spatial.Point{X: x, Y: 50}] = 500 // x ∈ [1950, 4050)
	}
	n := surfaceNode(t, func() Surface { return &stripeGround{speed: water} })
	rng := rand.New(rand.NewPCG(13, 21))
	now := time.Now()
	for _, tc := range []struct {
		name  string
		start int64
		// want 是走了 ms 毫秒后的真实位置（千分位）。
		want func(ms int64) int64
	}{
		{"land into water", 1000, func(ms int64) int64 {
			if d := 400 * ms; d <= 950_000 {
				return 1_000_000 + d
			}
			return 1_950_000 + (400*ms-950_000)/2
		}},
		{"water onto land", 4000, func(ms int64) int64 {
			if d := 200 * ms; d <= 50_000 {
				return 4_000_000 + d
			}
			return 4_050_000 + 400*(ms-250)
		}},
	} {
		st := ActorState{EntityState: EntityState{ID: actorID(47), Kind: KindPlayer, X: tc.start, Y: 5000, VX: 400, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
		var ms int64
		for range 60 {
			e := int64(31 + rng.IntN(70))
			n.stepActor(&st, now, time.Duration(e)*time.Millisecond)
			ms += e
			got, want := st.X*milli+int64(st.FracX), tc.want(ms)
			if st.Role != RoleReal || st.VX != 400 || abs64(got-want) > 1 {
				t.Fatalf("%s after %dms: at %d thousandths (x %d %+d) vx %d %v, want %d", tc.name, ms, got, st.X, st.FracX, st.VX, st.Role, want)
			}
			if st.X >= 9000 {
				break
			}
		}
	}
}
