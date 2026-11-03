package cellworld

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 多 Cell 集成（v2 执行模型）：每个格、每个 Base 都是一个“进程”（自己的 EntityManager、Nest 引擎、心跳驱动、Node），
// 角色与 NPC 是 Nest 实体、由心跳驱动，交接以 Nest 事务为边界；Base 的 witness 经 Sync mirror 推给客户端替身。
// 场景函数与传输无关：进程内替身（可注入丢失 / 重复 / 乱序）与真实 NATS（nats_test.go）跑同一套断言。

const (
	worldPeriod     = 50 * time.Millisecond
	worldViewRadius = 800
	worldBaseSID    = 900
	// Base 的 Interest：客户端看见的半径小于 witness 半径，对象离开客户端视野之前位置一直有 witness 更新。
	baseBlock       = 700
	baseEnterRadius = 600
	baseLeaveRadius = 700
)

func worldGrid(t testing.TB, cols, rows int32) *Grid {
	t.Helper()
	owners := map[CellID]int32{}
	for c := range cols {
		for r := range rows {
			owners[CellID{c, r}] = 101 + r*cols + c
		}
	}
	g, err := NewGrid(GridConfig{CellSize: 10000, Cols: cols, Rows: rows, GhostBand: 1000, Hysteresis: 100, Owners: owners})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type worldSpec struct {
	grid      *Grid
	transport func(t testing.TB, name string) Transport
	cellMod   func(*NodeConfig)
	strict    bool
}

type world struct {
	t     *testing.T
	spec  worldSpec
	grid  *Grid
	real  *realChecker
	base  *testProc
	cells map[CellID]*testProc

	mu         sync.Mutex
	views      map[int64][]View
	results    []AttackResult
	hits       map[HitID]int
	handoffLat []time.Duration
	ghostLat   []time.Duration
	writebacks map[int64][]EntityState
}

func startWorld(t *testing.T, spec worldSpec) *world {
	w := &world{t: t, spec: spec, grid: spec.grid, real: newRealChecker(t), cells: map[CellID]*testProc{},
		views: map[int64][]View{}, hits: map[HitID]int{}, writebacks: map[int64][]EntityState{}}
	for _, id := range w.grid.Cells() {
		w.startCell(id)
	}
	w.base = startProc(t, procSpec{sid: worldBaseSID, base: true, grid: w.grid, transport: spec.transport(t, "base"), strict: spec.strict, mod: func(cfg *NodeConfig) {
		cfg.Hooks.Witness = func(v View) {
			w.mu.Lock()
			w.views[v.Player] = append(w.views[v.Player], v)
			w.mu.Unlock()
		}
		cfg.Hooks.AttackResult = func(r AttackResult) {
			w.mu.Lock()
			w.results = append(w.results, r)
			w.mu.Unlock()
		}
		cfg.Hooks.Writeback = func(id int64, s EntityState) {
			w.mu.Lock()
			w.writebacks[id] = append(w.writebacks[id], s)
			w.mu.Unlock()
		}
	}})
	t.Cleanup(func() {
		w.base.stop()
		for id := range w.cells {
			w.stopCell(id)
		}
	})
	return w
}

func (w *world) startCell(id CellID) {
	w.cells[id] = startProc(w.t, procSpec{sid: w.grid.SID(id), cell: &id, grid: w.grid, transport: w.spec.transport(w.t, "cell"+id.String()), strict: w.spec.strict, mod: func(cfg *NodeConfig) {
		cfg.Hooks.RealChanged = w.real.hook
		cfg.Hooks.DamageApplied = func(_ CellID, _ int64, hit HitID, _ int32) {
			w.mu.Lock()
			w.hits[hit]++
			w.mu.Unlock()
		}
		cfg.Hooks.HandoffDone = func(_ CellID, _ int64, _ uint64, accepted bool, d time.Duration) {
			if accepted {
				w.mu.Lock()
				w.handoffLat = append(w.handoffLat, d)
				w.mu.Unlock()
			}
		}
		cfg.Hooks.GhostApplied = func(_ CellID, d time.Duration, _ int) {
			w.mu.Lock()
			w.ghostLat = append(w.ghostLat, d)
			w.mu.Unlock()
		}
		if w.spec.cellMod != nil {
			w.spec.cellMod(cfg)
		}
	}})
}

// stopCell 模拟 Cell 进程崩溃：状态全部丢失。
func (w *world) stopCell(id CellID) {
	if p, ok := w.cells[id]; ok {
		p.stop()
		delete(w.cells, id)
	}
}

func (w *world) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (w *world) spawn(id CellID, st EntityState) {
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.cells[id].node.Spawn(ctx, st); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) spawnAll(npcs map[int64]spatial.Point) {
	for id, p := range npcs {
		cell, ok := w.grid.CellAt(p)
		if !ok {
			w.t.Fatalf("npc %d outside world", id)
		}
		w.spawn(cell, EntityState{ID: id, X: p.X, Y: p.Y, HP: 100})
	}
}

func (w *world) viewsOf(unique int64) []View {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]View(nil), w.views[actorID(unique)]...)
}

func (w *world) lastView(unique int64) (View, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	vs := w.views[actorID(unique)]
	if len(vs) == 0 {
		return View{}, false
	}
	return vs[len(vs)-1], true
}

func (w *world) resetViews(unique int64) {
	w.mu.Lock()
	delete(w.views, actorID(unique))
	w.mu.Unlock()
}

// waitView 等到玩家最近的 witness 满足 cond。
func (w *world) waitView(unique int64, timeout time.Duration, what string, cond func(View) bool) View {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := w.lastView(unique); ok && cond(v) {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := w.lastView(unique)
	w.t.Fatalf("player %d: timed out waiting for %s; last view cell=%d epoch=%d self=(%d,%d)", unique, what, v.Cell, v.Epoch, v.Self.X, v.Self.Y)
	return View{}
}

func (w *world) enter(unique int64, at spatial.Point) View {
	w.t.Helper()
	pl := w.base.player(unique)
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.base.node.Enter(ctx, pl.ID(), actorID(unique), at, 100); err != nil {
		w.t.Fatal(err)
	}
	cell, _ := w.grid.CellAt(at)
	return w.waitView(unique, 5*time.Second, "first view", func(v View) bool { return v.Cell == w.grid.SID(cell) })
}

func (w *world) move(unique int64, vx, vy int64) {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	if err := w.base.node.Move(ctx, playerEntityID(unique), vx, vy); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) attack(unique, target int64, amount int32) HitID {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	hit, err := w.base.node.Attack(ctx, playerEntityID(unique), target, amount)
	if err != nil {
		w.t.Fatal(err)
	}
	return hit
}

func (w *world) owner(unique int64) (int32, uint64) {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	st, err := w.base.node.BaseStateOf(ctx, playerEntityID(unique))
	if err != nil {
		w.t.Fatal(err)
	}
	return st.Cell, st.Epoch
}

func (w *world) baseState(unique int64) BaseState {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	st, err := w.base.node.BaseStateOf(ctx, playerEntityID(unique))
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

func (w *world) inspect(id CellID) []EntitySnapshot {
	w.t.Helper()
	ctx, cancel := w.ctx()
	defer cancel()
	snap, err := w.cells[id].node.Inspect(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	return snap
}

// assertSingleReal 在静止后检查：实体恰好在 want 格是 Real，其他格没有 Real / Frozen；也没有格观察到冲突的 Real 状态。
func (w *world) assertSingleReal(id int64, want CellID) {
	w.t.Helper()
	for cid, p := range w.cells {
		for _, s := range w.inspect(cid) {
			if s.State.ID != id {
				continue
			}
			if (s.Role == "real") != (cid == want) || s.Role == "frozen" {
				w.t.Errorf("entity %d is %s in %v (want Real only in %v)", id, s.Role, cid, want)
			}
		}
		if st := p.node.Stats(); st.Conflicts != 0 {
			w.t.Errorf("cell %v saw %d conflicting Real states", cid, st.Conflicts)
		}
	}
}

// checkContinuity：静止 NPC 落在玩家 witness 半径内（按 witness 里玩家自己的位置算）时必须出现在 witness 里；连续缺失
// 不超过 maxMiss 份。相邻两份 witness 的到达间隔不超过 maxGap（交接窗口里旧格已停发、新格还没发）。
func checkContinuity(t *testing.T, views []View, npcs map[int64]spatial.Point, maxMiss int, maxGap time.Duration) (worstMiss int, worstGap time.Duration) {
	t.Helper()
	misses := map[int64]int{}
	for i, v := range views {
		present := map[int64]bool{}
		for _, e := range v.Entities {
			present[e.ID] = true
		}
		for id, p := range npcs {
			if spatial.WithinDistance(v.Self.Pos(), p, worldViewRadius) && !present[id] {
				misses[id]++
				worstMiss = max(worstMiss, misses[id])
				if misses[id] > maxMiss {
					t.Errorf("view %d (cell %d epoch %d tick %d self (%d,%d)): npc %d at %v missing %d views in a row",
						i, v.Cell, v.Epoch, v.Tick, v.Self.X, v.Self.Y, id, p, misses[id])
				}
			} else {
				misses[id] = 0
			}
		}
		if i > 0 {
			gap := v.At.Sub(views[i-1].At)
			worstGap = max(worstGap, gap)
			if gap > maxGap {
				t.Errorf("view gap %v between view %d (cell %d) and %d (cell %d) exceeds %v", gap, i-1, views[i-1].Cell, i, v.Cell, maxGap)
			}
			if v.Epoch < views[i-1].Epoch {
				t.Errorf("view epoch went back %d -> %d", views[i-1].Epoch, v.Epoch)
			}
		}
	}
	return worstMiss, worstGap
}

// checkClient 检查客户端（Base 进程 Manager 推出的帧）看到的连续性：玩家附近（离开半径以内，减一段余量）的静止 NPC
// 从未被 remove（交接不会让对象 remove 再 create）；结束时离玩家不远（进入半径以内，减余量）的都在客户端手里。
func (w *world) checkClient(unique int64, npcs map[int64]spatial.Point) {
	w.t.Helper()
	time.Sleep(4 * worldPeriod) // 让最后的 witness 与帧到达
	views := w.viewsOf(unique)
	posAt := func(at time.Time) (spatial.Point, bool) {
		var p spatial.Point
		found := false
		for _, v := range views {
			if v.At.After(at) {
				break
			}
			p, found = v.Self.Pos(), true
		}
		return p, found
	}
	const margin = 150
	for _, r := range w.base.client.removed(unique) {
		p, known := npcs[r.id]
		if !known {
			continue
		}
		if self, ok := posAt(r.at); ok && spatial.WithinDistance(self, p, baseLeaveRadius-margin) {
			w.t.Errorf("client of player %d: npc %d at %v removed while the player was at %v (within %d)", unique, r.id, p, self, baseLeaveRadius-margin)
		}
	}
	last, ok := w.lastView(unique)
	if !ok {
		return
	}
	held := w.base.client.held(unique)
	for id, p := range npcs {
		if spatial.WithinDistance(last.Self.Pos(), p, baseEnterRadius-margin) {
			if _, ok := held[id]; !ok {
				w.t.Errorf("client of player %d at %v does not hold npc %d at %v", unique, last.Self.Pos(), id, p)
			}
		}
	}
	if _, ok := held[actorID(unique)]; !ok {
		w.t.Errorf("client of player %d does not hold its own SceneActor", unique)
	}
}

func boundaryNPCs() map[int64]spatial.Point {
	return map[int64]spatial.Point{
		npcID(201): {X: 9500, Y: 5000}, npcID(202): {X: 9800, Y: 5400}, npcID(203): {X: 9950, Y: 4700},
		npcID(204): {X: 10050, Y: 5300}, npcID(205): {X: 10300, Y: 5000}, npcID(206): {X: 10700, Y: 4900},
	}
}

// 玩家沿直线 8800 → 11400 穿过 A/B 边界（10 m/s）：witness 与客户端视野连续，交接一次，B 成为唯一 Real。
func scenarioStraightCrossing(t *testing.T, w *world, maxMiss int, maxGap time.Duration) {
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 8800, Y: 5000})
	w.resetViews(1)
	w.move(1, 1000, 0)
	w.waitView(1, 10*time.Second, "reach x>=11300", func(v View) bool { return v.Self.X >= 11300 })
	w.move(1, 0, 0)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	t.Logf("views=%d worst consecutive miss=%d worst gap=%v", len(w.viewsOf(1)), miss, gap)
	w.checkClient(1, npcs)
	sid, epoch := w.owner(1)
	if sid != w.grid.SID(b) || epoch != 2 {
		t.Fatalf("owner = sid %d epoch %d, want B epoch 2", sid, epoch)
	}
	w.assertSingleReal(actorID(1), b)
}

// 在边界来回抖动（幅度 > 迟滞，反复交接）：witness 与客户端视野连续，每次交接 epoch +1，任一时刻至多一个 Real。
func scenarioJitter(t *testing.T, w *world, maxMiss int, maxGap time.Duration) {
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9990, Y: 5000})
	w.resetViews(1)
	for i := range 12 {
		vx := int64(1000)
		if i%2 == 1 {
			vx = -1000
		}
		w.move(1, vx, 0)
		w.waitView(1, 5*time.Second, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10130
			}
			return v.Self.X <= 9870
		})
	}
	w.move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	w.checkClient(1, npcs)
	sid, epoch := w.owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v final owner sid=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	if epoch < 12 {
		t.Fatalf("epoch %d after 12 boundary crossings, want >= 12", epoch)
	}
	cell, _ := w.grid.CellOf(sid)
	w.assertSingleReal(actorID(1), cell)
	if last, _ := w.lastView(1); last.Cell != sid {
		t.Fatalf("last view from %d, owner %d", last.Cell, sid)
	}
}

// 斜穿四格交点：(9300,9300) → (10700,10700)，可能直接 A→D，也可能经 B 或 C；视野连续，最终 D 是唯一 Real。
func scenarioDiagonal(t *testing.T, w *world, maxMiss int, maxGap time.Duration) {
	npcs := map[int64]spatial.Point{
		npcID(301): {X: 9600, Y: 9700}, npcID(302): {X: 10300, Y: 9700}, npcID(303): {X: 9700, Y: 10300}, npcID(304): {X: 10400, Y: 10400},
		npcID(305): {X: 9950, Y: 10050}, npcID(306): {X: 10050, Y: 9950}, npcID(307): {X: 10000, Y: 10000},
	}
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9300, Y: 9300})
	w.resetViews(1)
	w.move(1, 700, 700)
	w.waitView(1, 10*time.Second, "reach corner+700", func(v View) bool { return v.Self.X >= 10700 && v.Self.Y >= 10700 })
	w.move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	w.checkClient(1, npcs)
	sid, epoch := w.owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v owner sid=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	d := CellID{1, 1}
	if sid != w.grid.SID(d) {
		t.Fatalf("owner = %d, want D", sid)
	}
	w.assertSingleReal(actorID(1), d)
}

// 跨界攻击：A 区玩家每 100ms 攻击一次从 B 走进 A 的 NPC（途中迁移）。每个 HitID 至多结算一次；HP 等于初值减去
// 实际结算次数 × 伤害；Applied 结论都真的结算过，结算过的都报了 Applied 或 Unknown。
func scenarioAttackAcrossAndDuringMigration(t *testing.T, w *world, wantRetries bool) {
	a := CellID{0, 0}
	target := npcID(7)
	w.enter(1, spatial.Point{X: 9600, Y: 5000})
	w.spawn(CellID{1, 0}, EntityState{ID: target, X: 10300, Y: 5000, VX: -400, HP: 100})
	time.Sleep(3 * worldPeriod)
	var hits []HitID
	for range 25 {
		hits = append(hits, w.attack(1, target, 2))
		time.Sleep(100 * time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		n := len(w.results)
		w.mu.Unlock()
		if n >= len(hits) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.mu.Lock()
	results := append([]AttackResult(nil), w.results...)
	applications := map[HitID]int{}
	for k, v := range w.hits {
		applications[k] = v
	}
	w.mu.Unlock()
	byHit := map[HitID]AttackResult{}
	counts := map[DamageOutcome]int{}
	for _, r := range results {
		if prev, dup := byHit[r.Hit]; dup {
			t.Errorf("two results for %v: %v then %v", r.Hit, prev.Outcome, r.Outcome)
		}
		byHit[r.Hit] = r
		counts[r.Outcome]++
	}
	if len(byHit) != len(hits) {
		t.Fatalf("got results for %d of %d attacks: %v", len(byHit), len(hits), counts)
	}
	for hit, n := range applications {
		if n != 1 {
			t.Errorf("hit %v applied %d times", hit, n)
		}
		if r := byHit[hit]; r.Outcome != OutcomeApplied && r.Outcome != OutcomeUnknown {
			t.Errorf("hit %v applied but reported %v", hit, r.Outcome)
		}
	}
	for hit, r := range byHit {
		if r.Outcome == OutcomeApplied && applications[hit] != 1 {
			t.Errorf("hit %v reported applied but applied %d times", hit, applications[hit])
		}
	}
	time.Sleep(3 * worldPeriod)
	var hp int32 = -1
	for _, s := range w.inspect(a) {
		if s.State.ID == target && s.Role == "real" {
			hp = s.State.HP
		}
	}
	if want := int32(100 - 2*len(applications)); hp != want {
		t.Fatalf("target hp in A = %d, want %d (applications %d)", hp, want, len(applications))
	}
	var retries uint64
	for _, p := range w.cells {
		retries += p.node.Stats().DamageRetries
	}
	t.Logf("results %v, applications %d, retries %d", counts, len(applications), retries)
	if wantRetries && retries == 0 {
		t.Fatal("expected attacks to hit the migration window and be retried")
	}
	w.assertSingleReal(target, a)
}

func loopbackSpec(t testing.TB, cols, rows int32) (worldSpec, *loopback) {
	lb := newLoopback()
	return worldSpec{grid: worldGrid(t, cols, rows), transport: func(testing.TB, string) Transport { return lb }}, lb
}

// witness 连续性的约定：静止 NPC 在半径内时连续缺失不超过 1 份；witness 间隔不超过 3 拍 + 50ms（与 v1 相同）。
const (
	contMaxMiss = 1
	contMaxGap  = 3*worldPeriod + 50*time.Millisecond
)

func TestWorldStraightCrossing(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioStraightCrossing(t, startWorld(t, spec), contMaxMiss, contMaxGap)
}

// 接 demo 时发现（2026-11-03）：DataEngine 装好加载器的 getter 对不在内存的不持久实体（SceneActor / Monster）返回错误，
// 不是 nil。Node 把还不存在的角色放进加锁集合（交接装入、进入、生成、伤害打到不在本格的目标）时整个请求失败。
// strict getter 下照样走直线跨界、跨界攻击：Node 只锁在内存里的角色（NodeConfig.Loaded）。
func TestWorldWithAGetterThatRefusesAbsentEntities(t *testing.T) {
	t.Run("straight", func(t *testing.T) {
		spec, _ := loopbackSpec(t, 2, 1)
		spec.strict = true
		scenarioStraightCrossing(t, startWorld(t, spec), contMaxMiss, contMaxGap)
	})
	t.Run("attack", func(t *testing.T) {
		spec, _ := loopbackSpec(t, 2, 1)
		spec.strict = true
		scenarioAttackAcrossAndDuringMigration(t, startWorld(t, spec), false)
	})
}

func TestWorldJitterAtBoundary(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioJitter(t, startWorld(t, spec), contMaxMiss, contMaxGap)
}

func TestWorldDiagonalThroughFourCellCorner(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 2)
	scenarioDiagonal(t, startWorld(t, spec), contMaxMiss, contMaxGap)
}

func TestWorldAttackAcrossBoundary(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioAttackAcrossAndDuringMigration(t, startWorld(t, spec), false)
}

// 交接 Offer 延迟 300ms：目标冻结窗口变长，攻击落进窗口后按“重试”改投新 owner，仍只结算一次、不报未知。
func TestWorldAttackDuringSlowMigration(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindOffer {
			return false, false, 300 * time.Millisecond
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	scenarioAttackAcrossAndDuringMigration(t, w, true)
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.results {
		if r.Outcome == OutcomeUnknown {
			t.Errorf("hit %v unknown with a 300ms migration window (deadline 2s)", r.Hit)
		}
	}
}

// Cell 之间的消息 20% 丢失、10% 重复、0～30ms 随机延迟（乱序）：来回穿越与攻击仍然不双写、不出现两个 Real、
// 伤害不重复结算，最终收敛到唯一 owner。
func lossyFault(seed uint64) func(string, *wireMsg) (bool, bool, time.Duration) {
	var mu sync.Mutex
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	return func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		switch m.Kind {
		case kindGhost, kindOffer, kindOfferReply, kindDamage, kindDamageReply:
		default:
			return false, false, 0 // Cell ↔ Base 不注入
		}
		mu.Lock()
		defer mu.Unlock()
		return r.Float64() < 0.2, r.Float64() < 0.1, time.Duration(r.IntN(30)) * time.Millisecond
	}
}

func TestWorldLossyNetworkCrossingAndBack(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(lossyFault(1))
	w := startWorld(t, spec)
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9000, Y: 5000})
	w.resetViews(1)
	for i := range 6 {
		vx := int64(1000)
		if i%2 == 1 {
			vx = -1000
		}
		w.move(1, vx, 0)
		w.waitView(1, 10*time.Second, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10500
			}
			return v.Self.X <= 9500
		})
	}
	w.move(1, 0, 0)
	time.Sleep(10 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, 3, 2*time.Second)
	sid, epoch := w.owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v owner=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	cell, _ := w.grid.CellOf(sid)
	w.assertSingleReal(actorID(1), cell)
	if epoch < 7 {
		t.Fatalf("epoch %d after 6 crossings, want >= 7", epoch)
	}
}

func TestWorldLossyNetworkAttack(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(lossyFault(2))
	scenarioAttackAcrossAndDuringMigration(t, startWorld(t, spec), false)
}
