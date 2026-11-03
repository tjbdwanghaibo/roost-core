package cellworld

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 进程内多 Cell 集成：每个 Cell 与 Base 都跑自己的事件循环 goroutine，经 Transport 通信（进程内替身或真实
// NATS，见 nats_test.go）。场景函数与传输无关，两种传输跑同一套断言。

const (
	worldPeriod     = 50 * time.Millisecond
	worldViewRadius = 800
	worldBaseSID    = 900
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
	grid *Grid
	// transport 为每个 Cell 与 Base 各返回一个通道（NATS 下是各自的连接）。
	transport func(t testing.TB, name string) Transport
	cellMod   func(*CellConfig)
}

type runningCell struct {
	cell   *Cell
	cancel context.CancelFunc
	done   chan struct{}
}

type world struct {
	t     *testing.T
	spec  worldSpec
	grid  *Grid
	real  *realChecker
	base  *Base
	cells map[CellID]*runningCell

	mu         sync.Mutex
	views      map[int64][]View
	results    []AttackResult
	hits       map[HitID]int
	handoffLat []time.Duration
	ghostLat   []time.Duration
}

func startWorld(t *testing.T, spec worldSpec) *world {
	w := &world{t: t, spec: spec, grid: spec.grid, real: newRealChecker(t), cells: map[CellID]*runningCell{},
		views: map[int64][]View{}, hits: map[HitID]int{}}
	for _, id := range w.grid.Cells() {
		w.startCell(id)
	}
	base, err := NewBase(BaseConfig{World: "w", SID: worldBaseSID, Grid: w.grid, Transport: spec.transport(t, "base"),
		OnView: func(v View) {
			w.mu.Lock()
			w.views[v.Player] = append(w.views[v.Player], v)
			w.mu.Unlock()
		},
		OnAttackResult: func(r AttackResult) {
			w.mu.Lock()
			w.results = append(w.results, r)
			w.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.base = base
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = base.Run(ctx) }()
	<-base.Ready()
	t.Cleanup(func() {
		cancel()
		<-done
		for id := range w.cells {
			w.stopCell(id)
		}
	})
	return w
}

func (w *world) startCell(id CellID) {
	cfg := CellConfig{World: "w", Grid: w.grid, Cell: id, Transport: w.spec.transport(w.t, "cell"+id.String()),
		ViewRadius: worldViewRadius, Period: worldPeriod}
	cfg.Hooks = Hooks{
		RealChanged: w.real.hook,
		DamageApplied: func(_ CellID, _ int64, hit HitID, _ int32) {
			w.mu.Lock()
			w.hits[hit]++
			w.mu.Unlock()
		},
		HandoffDone: func(_ CellID, _ int64, _ uint64, accepted bool, d time.Duration) {
			if accepted {
				w.mu.Lock()
				w.handoffLat = append(w.handoffLat, d)
				w.mu.Unlock()
			}
		},
		GhostApplied: func(_ CellID, d time.Duration, _ int) {
			w.mu.Lock()
			w.ghostLat = append(w.ghostLat, d)
			w.mu.Unlock()
		},
	}
	if w.spec.cellMod != nil {
		w.spec.cellMod(&cfg)
	}
	c, err := NewCell(cfg)
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rc := &runningCell{cell: c, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(rc.done); _ = c.Run(ctx) }()
	<-c.Ready()
	w.cells[id] = rc
}

// stopCell 模拟 Cell 进程崩溃：事件循环停止，状态全部丢失。
func (w *world) stopCell(id CellID) {
	rc, ok := w.cells[id]
	if !ok {
		return
	}
	rc.cancel()
	<-rc.done
	delete(w.cells, id)
}

func (w *world) spawn(id CellID, st EntityState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.cells[id].cell.SpawnNPC(ctx, st); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) viewsOf(id int64) []View {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]View(nil), w.views[id]...)
}

func (w *world) resetViews(id int64) {
	w.mu.Lock()
	delete(w.views, id)
	w.mu.Unlock()
}

// waitView 等到玩家最近的视野满足 cond。
func (w *world) waitView(id int64, timeout time.Duration, what string, cond func(View) bool) View {
	w.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v, ok := w.base.LastView(id); ok && cond(v) {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	v, _ := w.base.LastView(id)
	w.t.Fatalf("player %d: timed out waiting for %s; last view cell=%d epoch=%d self=(%d,%d)", id, what, v.Cell, v.Epoch, v.Self.X, v.Self.Y)
	return View{}
}

func (w *world) enter(id int64, at spatial.Point) View {
	w.t.Helper()
	if err := w.base.Enter(id, at, 100); err != nil {
		w.t.Fatal(err)
	}
	cell, _ := w.grid.CellAt(at)
	return w.waitView(id, 5*time.Second, "first view", func(v View) bool { return v.Cell == w.grid.SID(cell) })
}

// checkContinuity：静止 NPC 落在玩家视野半径内（按视野里玩家自己的位置算）时，必须出现在视野里；连续缺失
// 不超过 maxMiss 帧。相邻两份视野的到达间隔不超过 maxGap（交接窗口里旧格已停发、新格还没发）。
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
					t.Errorf("view %d (cell %d epoch %d tick %d self (%d,%d)): npc %d at %v missing %d frames in a row",
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

func (w *world) spawnAll(npcs map[int64]spatial.Point) {
	for id, p := range npcs {
		cell, ok := w.grid.CellAt(p)
		if !ok {
			w.t.Fatalf("npc %d outside world", id)
		}
		w.spawn(cell, EntityState{ID: id, X: p.X, Y: p.Y, HP: 100})
	}
}

// assertSingleReal 在静止后检查：实体恰好在 want 格是 Real，其他格没有 Real / Frozen。
func (w *world) assertSingleReal(id int64, want CellID) {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for cid, rc := range w.cells {
		snap, err := rc.cell.Inspect(ctx)
		if err != nil {
			w.t.Fatal(err)
		}
		for _, s := range snap {
			if s.State.ID != id {
				continue
			}
			if (s.Role == "real") != (cid == want) || s.Role == "frozen" {
				w.t.Errorf("entity %d is %s in %v (want Real only in %v)", id, s.Role, cid, want)
			}
		}
		if st := rc.cell.Stats(); st.Conflicts != 0 {
			w.t.Errorf("cell %v saw %d conflicting Real states", cid, st.Conflicts)
		}
	}
}

func boundaryNPCs() map[int64]spatial.Point {
	return map[int64]spatial.Point{
		201: {X: 9500, Y: 5000}, 202: {X: 9800, Y: 5400}, 203: {X: 9950, Y: 4700},
		204: {X: 10050, Y: 5300}, 205: {X: 10300, Y: 5000}, 206: {X: 10700, Y: 4900},
	}
}

// 玩家沿直线 8800 → 11400 穿过 A/B 边界（10 m/s）：视野连续，交接一次，B 成为唯一 Real。
func scenarioStraightCrossing(t *testing.T, w *world, maxMiss int, maxGap time.Duration) {
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	a, b := CellID{0, 0}, CellID{1, 0}
	w.enter(1, spatial.Point{X: 8800, Y: 5000})
	w.resetViews(1)
	if err := w.base.Move(1, 1000, 0); err != nil {
		t.Fatal(err)
	}
	w.waitView(1, 10*time.Second, "reach x>=11300", func(v View) bool { return v.Self.X >= 11300 })
	_ = w.base.Move(1, 0, 0)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	t.Logf("views=%d worst consecutive miss=%d worst gap=%v", len(w.viewsOf(1)), miss, gap)
	sid, epoch, _ := w.base.Owner(1)
	if sid != w.grid.SID(b) || epoch != 2 {
		t.Fatalf("owner = sid %d epoch %d, want B epoch 2", sid, epoch)
	}
	time.Sleep(3 * worldPeriod)
	w.assertSingleReal(1, b)
	_ = a
}

// 在边界来回抖动（幅度 > 迟滞，反复交接）：视野连续，每次交接 epoch +1，任一时刻至多一个 Real。
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
		_ = w.base.Move(1, vx, 0)
		w.waitView(1, 5*time.Second, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10130
			}
			return v.Self.X <= 9870
		})
	}
	_ = w.base.Move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	sid, epoch, _ := w.base.Owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v final owner sid=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	if epoch < 12 {
		t.Fatalf("epoch %d after 12 boundary crossings, want >= 12", epoch)
	}
	last, _ := w.base.LastView(1)
	cell, _ := w.grid.CellOf(sid)
	w.assertSingleReal(1, cell)
	if last.Cell != sid {
		t.Fatalf("last view from %d, owner %d", last.Cell, sid)
	}
}

// 斜穿四格交点：(9300,9300) → (10700,10700)，可能直接 A→D，也可能经 B 或 C；视野连续，最终 D 是唯一 Real。
func scenarioDiagonal(t *testing.T, w *world, maxMiss int, maxGap time.Duration) {
	npcs := map[int64]spatial.Point{
		301: {X: 9600, Y: 9700}, 302: {X: 10300, Y: 9700}, 303: {X: 9700, Y: 10300}, 304: {X: 10400, Y: 10400},
		305: {X: 9950, Y: 10050}, 306: {X: 10050, Y: 9950}, 307: {X: 10000, Y: 10000},
	}
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9300, Y: 9300})
	w.resetViews(1)
	_ = w.base.Move(1, 700, 700)
	w.waitView(1, 10*time.Second, "reach corner+700", func(v View) bool { return v.Self.X >= 10700 && v.Self.Y >= 10700 })
	_ = w.base.Move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, maxGap)
	sid, epoch, _ := w.base.Owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v owner sid=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	d := CellID{1, 1}
	if sid != w.grid.SID(d) {
		t.Fatalf("owner = %d, want D", sid)
	}
	w.assertSingleReal(1, d)
}

// 跨界攻击：A 区玩家每 100ms 攻击一次从 B 走进 A 的 NPC（途中迁移）。每个 HitID 至多结算一次；HP 等于初值减去
// 实际结算次数 × 伤害；Applied 结论都真的结算过，结算过的都报了 Applied 或 Unknown。
func scenarioAttackAcrossAndDuringMigration(t *testing.T, w *world, wantRetries bool) {
	a := CellID{0, 0}
	w.enter(1, spatial.Point{X: 9600, Y: 5000})
	w.spawn(CellID{1, 0}, EntityState{ID: 7, X: 10300, Y: 5000, VX: -400, HP: 100})
	time.Sleep(3 * worldPeriod)
	var hits []HitID
	for range 25 {
		hit, err := w.base.Attack(1, 7, 2)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, hit)
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, err := w.cells[a].cell.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hp int32 = -1
	for _, s := range snap {
		if s.State.ID == 7 && s.Role == "real" {
			hp = s.State.HP
		}
	}
	if want := int32(100 - 2*len(applications)); hp != want {
		t.Fatalf("target hp in A = %d, want %d (applications %d)", hp, want, len(applications))
	}
	var retries uint64
	for _, rc := range w.cells {
		retries += rc.cell.Stats().DamageRetries
	}
	t.Logf("results %v, applications %d, retries %d", counts, len(applications), retries)
	if wantRetries && retries == 0 {
		t.Fatal("expected attacks to hit the migration window and be retried")
	}
	w.assertSingleReal(7, a)
}

func loopbackSpec(t testing.TB, cols, rows int32) (worldSpec, *loopback) {
	lb := newLoopback()
	return worldSpec{grid: worldGrid(t, cols, rows), transport: func(testing.TB, string) Transport { return lb }}, lb
}

// 视野连续性的约定：静止 NPC 在视野半径内时连续缺失不超过 1 帧；视野间隔不超过 3 拍 + 50ms（macOS 定时器抖动）。
const (
	contMaxMiss = 1
	contMaxGap  = 3*worldPeriod + 50*time.Millisecond
)

func TestWorldStraightCrossing(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioStraightCrossing(t, startWorld(t, spec), contMaxMiss, contMaxGap)
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
// 伤害不重复结算，最终收敛到唯一 owner。视野允许更大间隔（交接重传期间没有视野）。
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
		_ = w.base.Move(1, vx, 0)
		w.waitView(1, 10*time.Second, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10500
			}
			return v.Self.X <= 9500
		})
	}
	_ = w.base.Move(1, 0, 0)
	time.Sleep(10 * worldPeriod)
	miss, gap := checkContinuity(t, w.viewsOf(1), npcs, 3, 2*time.Second)
	sid, epoch, _ := w.base.Owner(1)
	t.Logf("views=%d worst miss=%d worst gap=%v owner=%d epoch=%d", len(w.viewsOf(1)), miss, gap, sid, epoch)
	cell, _ := w.grid.CellOf(sid)
	w.assertSingleReal(1, cell)
	if epoch < 7 {
		t.Fatalf("epoch %d after 6 crossings, want >= 7", epoch)
	}
}

func TestWorldLossyNetworkAttack(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(lossyFault(2))
	scenarioAttackAcrossAndDuringMigration(t, startWorld(t, spec), false)
}

// Cell 崩溃：玩家在 B，B 的进程停止（状态全部丢失）后重启一个空的 B'。Base 的静默看门狗按最后位置重进，
// epoch 比崩溃前大；视野恢复。崩溃期间的 Enter 发给不存在的 Cell，丢失后由看门狗重发。
func TestWorldCellCrashPlayerReenters(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	_, before, _ := w.base.Owner(1)
	w.stopCell(b)
	w.real.forget(b)
	time.Sleep(1500 * time.Millisecond) // 超过 SilenceTimeout，第一次重进发给已停的 B，丢失
	w.startCell(b)
	v := w.waitView(1, 5*time.Second, "view after restart", func(v View) bool { return v.Epoch > before })
	if v.Cell != w.grid.SID(b) || v.Self.X != 12000 {
		t.Fatalf("after restart view from %d at (%d,%d)", v.Cell, v.Self.X, v.Self.Y)
	}
	if s := w.base.Stats(); s.Reenters == 0 {
		t.Fatalf("base stats = %+v, want re-enters", s)
	}
	w.assertSingleReal(1, b)
}
