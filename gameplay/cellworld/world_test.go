package cellworld

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
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
	return worldGridBand(t, cols, rows, 1000, 100)
}

// worldGridBand 与 worldGrid 相同（格大小、owner），只换边界带 G 与迟滞 H（用例需要更宽的迟滞时用）。
func worldGridBand(t testing.TB, cols, rows int32, band, hysteresis int64) *Grid {
	t.Helper()
	owners := map[CellID]int32{}
	for c := range cols {
		for r := range rows {
			owners[CellID{c, r}] = 101 + r*cols + c
		}
	}
	g, err := NewGrid(GridConfig{CellSize: 10000, Cols: cols, Rows: rows, GhostBand: band, Hysteresis: hysteresis, Owners: owners})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type worldSpec struct {
	grid      *Grid
	transport func(t testing.TB, name string) Transport
	cellMod   func(*NodeConfig)
	baseMod   func(*NodeConfig)
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
	panels     map[int64][]Panel // Real 上真正应用过的面板（PanelApplied），按角色
	panelRes   []PanelResult
	beats      map[CellID][]time.Time  // 每格区域心跳提交后的时刻（TickDone）：间隔 / 窗口类断言的尺子
	realAts    map[realEvent]time.Time // 角色在各格取得 / 失去 Real 的提交时刻（RealChanged）
}

// realEvent 是一次 RealChanged：某格的某个 epoch 取得（acquired）或失去 Real。
type realEvent struct {
	cell     CellID
	id       int64
	epoch    uint64
	acquired bool
}

func startWorld(t *testing.T, spec worldSpec) *world {
	w := &world{t: t, spec: spec, grid: spec.grid, real: newRealChecker(t), cells: map[CellID]*testProc{},
		views: map[int64][]View{}, hits: map[HitID]int{}, writebacks: map[int64][]EntityState{}, panels: map[int64][]Panel{},
		beats: map[CellID][]time.Time{}, realAts: map[realEvent]time.Time{}}
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
		cfg.Hooks.PanelResult = func(r PanelResult) {
			w.mu.Lock()
			w.panelRes = append(w.panelRes, r)
			w.mu.Unlock()
		}
		if spec.baseMod != nil {
			spec.baseMod(cfg)
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
		cfg.Hooks.RealChanged = func(cell CellID, id int64, epoch uint64, acquired bool) {
			at := time.Now()
			w.real.hook(cell, id, epoch, acquired)
			w.mu.Lock()
			w.realAts[realEvent{cell, id, epoch, acquired}] = at
			w.mu.Unlock()
		}
		cfg.Hooks.TickDone = func(cell CellID, _ time.Duration, _, _ int) {
			at := time.Now()
			w.mu.Lock()
			w.beats[cell] = append(w.beats[cell], at)
			w.mu.Unlock()
		}
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
		cfg.Hooks.PanelApplied = func(_ CellID, id int64, p Panel, _ int32) {
			w.mu.Lock()
			w.panels[id] = append(w.panels[id], p)
			w.mu.Unlock()
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

// spawnAll 生成静止 NPC，并等邻格都收到它们的 Ghost：场景从“边界带里的实体两边都看得见”开始。原来不等，负载下 Ghost
// 还在路上时玩家已经过界，新格的头几份 witness 里缺这些 NPC（连续缺失超限），那是场景前提没成立，不是视野断档。
func (w *world) spawnAll(npcs map[int64]spatial.Point) {
	for id, p := range npcs {
		cell, ok := w.grid.CellAt(p)
		if !ok {
			w.t.Fatalf("npc %d outside world", id)
		}
		w.spawn(cell, EntityState{ID: id, X: p.X, Y: p.Y, HP: 100})
	}
	for id, p := range npcs {
		cell, _ := w.grid.CellAt(p)
		for _, target := range w.grid.GhostTargets(cell, p, nil) {
			if _, running := w.cells[target]; running {
				w.waitKnown(target, id)
			}
		}
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

// beatsUpTo 返回格 sid 到 at（含）为止完成（提交）的区域心跳数：与该格 witness 的 Tick 是同一个序号（区域心跳每拍先
// 加拍号、提交后调 TickDone；本包用例里区域心跳不会失败）。
func (w *world) beatsUpTo(sid int32, at time.Time) int {
	cell, ok := w.grid.CellOf(sid)
	if !ok {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, done := range w.beats[cell] {
		if !done.After(at) {
			n++
		}
	}
	return n
}

// realAt 返回角色 id 的 epoch 在格 sid 取得（acquired）或失去 Real 的提交时刻。
func (w *world) realAt(sid int32, id int64, epoch uint64, acquired bool) (time.Time, bool) {
	cell, ok := w.grid.CellOf(sid)
	if !ok {
		return time.Time{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	at, ok := w.realAts[realEvent{cell, id, epoch, acquired}]
	return at, ok
}

func (w *world) beatCount(cell CellID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.beats[cell])
}

// waitBeats 等格再完成 n 拍区域心跳：“过几拍再看”的窗口以格实际执行的拍数计，不以墙钟计（负载下心跳被推迟时窗口
// 随之拉长，不会在协议还没来得及走的时候就下结论）。期限 settleWithin + n 拍名义时长。
func (w *world) waitBeats(cell CellID, n int) {
	w.t.Helper()
	target := w.beatCount(cell) + n
	eventually(w.t, settleWithin+time.Duration(n)*worldPeriod, fmt.Sprintf("%d region beats of %v", n, cell), func() (bool, string) {
		got := w.beatCount(cell)
		return got >= target, fmt.Sprintf("%d of %d beats", got-target+n, n)
	})
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
	var diag strings.Builder
	if len(w.cells) > 0 {
		w.cellDiag(&diag, actorID(unique))
	}
	w.t.Fatalf("player %d: timed out after %v waiting for %s; last view cell=%d epoch=%d tick=%d self=(%d,%d) at %v (%v ago)\n%s",
		unique, timeout, what, v.Cell, v.Epoch, v.Tick, v.Self.X, v.Self.Y, v.At.Format("15:04:05.000"), time.Since(v.At), diag.String())
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
	return w.waitView(unique, settleWithin, "first view", func(v View) bool { return v.Cell == w.grid.SID(cell) })
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

// assertSingleReal 检查静止后的结论：实体恰好在 want 格是 Real，其他格没有 Real / Frozen；也没有格观察到冲突的 Real 状态。
// 旧格收到 OfferReply（或新格 Ghost 的隐式确认）才从 frozen 转为 Ghost，这是交接之后另外几跳异步事务，停下的那一刻可能
// 还没走完（完整包高负载下见过 1s 以上）：等到结论成立，超时按最后一次检查报错并打出各格诊断。期限 5s 长于缺省
// HandoffAbort（3s）：过了期限旧格还是 frozen，说明既没收到结论也没按超时放弃，不再是正常窗口。“任一时刻至多一个 Real”
// 由 realChecker 在每次提交后检查，不靠这里。
func (w *world) assertSingleReal(id int64, want CellID) {
	w.t.Helper()
	var problems []string
	for deadline := time.Now().Add(settleWithin); ; time.Sleep(10 * time.Millisecond) {
		problems = problems[:0]
		for cid := range w.cells {
			for _, s := range w.inspect(cid) {
				if s.State.ID == id && ((s.Role == "real") != (cid == want) || s.Role == "frozen") {
					problems = append(problems, fmt.Sprintf("entity %d is %s in %v (want Real only in %v)", id, s.Role, cid, want))
				}
			}
		}
		if len(problems) == 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(problems) > 0 {
		var diag strings.Builder
		w.cellDiag(&diag, id)
		w.t.Errorf("%s\n%s", strings.Join(problems, "\n"), diag.String())
	}
	for cid, p := range w.cells {
		if st := p.node.Stats(); st.Conflicts != 0 {
			w.t.Errorf("cell %v saw %d conflicting Real states", cid, st.Conflicts)
		}
	}
}

// waitSettled 等玩家的交接收尾：Base 记的 owner 格上恰好有一个 epoch 相同的 Real，其他格没有它的 Real / Frozen，最近
// 一份 witness 来自 owner。只等这个完成条件，不放宽任何断言；超时打印 Base 状态、各格的区域记录、交接计数与出站队列长度。
func (w *world) waitSettled(unique int64, timeout time.Duration) (int32, uint64) {
	w.t.Helper()
	id := actorID(unique)
	settled := func() (int32, uint64, bool) {
		st := w.baseState(unique)
		owner, ok := w.grid.CellOf(st.Cell)
		if !ok || st.Epoch == 0 {
			return st.Cell, st.Epoch, false
		}
		for cid := range w.cells {
			for _, s := range w.inspect(cid) {
				if s.State.ID != id {
					continue
				}
				switch {
				case s.Role == "frozen":
					return st.Cell, st.Epoch, false
				case s.Role == "real" && (cid != owner || s.State.Ver.Epoch != st.Epoch):
					return st.Cell, st.Epoch, false
				}
			}
		}
		if s, ok := w.roleOf(owner, id); !ok || s.Role != "real" {
			return st.Cell, st.Epoch, false
		}
		last, ok := w.lastView(unique)
		return st.Cell, st.Epoch, ok && last.Cell == st.Cell && last.Epoch == st.Epoch
	}
	deadline := time.Now().Add(timeout)
	for {
		sid, epoch, ok := settled()
		if ok {
			return sid, epoch
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var diag strings.Builder
	st := w.baseState(unique)
	last, _ := w.lastView(unique)
	fmt.Fprintf(&diag, "base: cell=%d epoch=%d inWorld=%v lostEpoch=%d enterHint=%d; last view from %d epoch %d\n",
		st.Cell, st.Epoch, st.InWorld, st.LostEpoch, st.EnterHint, last.Cell, last.Epoch)
	w.cellDiag(&diag, id)
	w.t.Fatalf("handoff of player %d did not settle within %v:\n%s", unique, timeout, diag.String())
	return 0, 0
}

// cellDiag 写出各格区域表里 id 的记录、交接计数与出站队列长度（交接没收尾时的诊断）。
func (w *world) cellDiag(diag *strings.Builder, id int64) {
	for cid, p := range w.cells {
		for _, s := range w.inspect(cid) {
			if s.State.ID == id {
				fmt.Fprintf(diag, "cell %v: role=%s epoch=%d seq=%d x=%d owner/target=%v\n", cid, s.Role, s.State.Ver.Epoch, s.State.Ver.Seq, s.State.X, s.Owner)
			}
		}
		ns := p.node.Stats()
		fmt.Fprintf(diag, "cell %v stats: started=%d accepted=%d rejected=%d aborted=%d offerRetransmits=%d offersReceived=%d staleDrops=%d outboxDrops=%d dispatchErrors=%d actorBeats=%d outbox=%d\n",
			cid, ns.HandoffsStarted, ns.HandoffsAccepted, ns.HandoffsRejected, ns.HandoffsAborted, ns.OfferRetransmits, ns.OffersReceived,
			ns.StaleDrops, ns.OutboxDrops, ns.DispatchErrors, ns.ActorBeats, len(p.node.out))
	}
}

// timeline 是给视野间隔分段用的两类事件：各格区域心跳的完成时刻、角色在各格取得 / 失去 Real 的提交时刻（进程内是
// world 的 hook 记录，多进程是子进程报告的 tick / real 事件）。
type timeline interface {
	beatsUpTo(sid int32, at time.Time) int
	realAt(sid int32, id int64, epoch uint64, acquired bool) (time.Time, bool)
}

// gapRule 是相邻两份 witness 之间允许的间隔。
//   - beats：同一格同一 epoch 的两份之间，该格的拍号之差（每拍区域心跳给本格每个玩家 Real 发一份，正常是 1）；
//   - edge：换手（epoch 变了）时，旧格在最后一份之后、冻结提交之前空过的拍数，与新格在装入提交之后、发出第一份之前
//     空过的拍数，各自的上限（正常是 0～1）；
//   - transit：换手时旧格冻结提交到新格装入提交之间的墙钟上限。这一段只有消息（Offer 出站、传输、新格的 Nest 事务），
//     没有心跳，无法按拍数计；取心跳经过时间的上限 max(1s, Period)（transitCap），只抓“交接被结构性地拖长”的回归。
type gapRule struct {
	beats, edge int
	transit     time.Duration
}

// transitCap 是换手时消息段的墙钟上限：等于心跳 Beat.Elapsed 的上限 max(1s, Period)。负载下（整包 -race 加 8 个满载
// 进程）这一段实测到过 0.6s：Offer 在出站队列、传输与新格 Scene 的 Nest 队列里排队，心跳照常在走，所以按拍数计会误报，
// 原来 200ms 的总间隔上限也因此约一半遍数误报。
const transitCap = time.Second

// continuity 是 checkContinuity 的观察值（最坏情况）。墙钟最大间隔只记录，不断言。
type continuity struct {
	worstMiss, worstBeats, worstEdge int
	worstTransit, worstGap           time.Duration
}

func (c continuity) String() string {
	return fmt.Sprintf("worst consecutive miss=%d, same-cell gap=%d beats, handoff edge=%d beats, handoff transit=%v (worst wall gap %v)",
		c.worstMiss, c.worstBeats, c.worstEdge, c.worstTransit, c.worstGap)
}

// checkContinuity：静止 NPC 落在玩家 witness 半径内（按 witness 里玩家自己的位置算）时必须出现在 witness 里；连续缺失
// 不超过 maxMiss 份。相邻两份 witness 的间隔按 rule 分段检查（见 gapRule）。
//
// 2026-11-10 起以拍数而不是墙钟毫秒为尺子：原来断言相邻两份的到达间隔不超过 3 拍 + 50ms（200ms），负载下心跳与消息都被
// 推迟，整包 -race 加压时约一半遍数误报（view gap 206ms 一类）。现在同一格的间隔用该格自己的拍号（实现真的漏发时跳号），
// 换手的间隔拆成“旧格停发到冻结”“新格装入到第一份”两段按各自的拍数计，中间只有消息的一段按 transitCap 计。
func checkContinuity(t *testing.T, views []View, npcs map[int64]spatial.Point, maxMiss int, rule gapRule, tl timeline) continuity {
	t.Helper()
	var c continuity
	misses := map[int64]int{}
	for i, v := range views {
		present := map[int64]bool{}
		for _, e := range v.Entities {
			present[e.ID] = true
		}
		for id, p := range npcs {
			if spatial.WithinDistance(v.Self.Pos(), p, worldViewRadius) && !present[id] {
				misses[id]++
				c.worstMiss = max(c.worstMiss, misses[id])
				if misses[id] > maxMiss {
					t.Errorf("view %d (cell %d epoch %d tick %d self (%d,%d)): npc %d at %v missing %d views in a row",
						i, v.Cell, v.Epoch, v.Tick, v.Self.X, v.Self.Y, id, p, misses[id])
				}
			} else {
				misses[id] = 0
			}
		}
		if i == 0 {
			continue
		}
		prev := views[i-1]
		c.worstGap = max(c.worstGap, v.At.Sub(prev.At))
		if v.Epoch < prev.Epoch {
			t.Errorf("view epoch went back %d -> %d", prev.Epoch, v.Epoch)
			continue
		}
		if v.Epoch == prev.Epoch {
			if v.Cell != prev.Cell {
				t.Errorf("views %d and %d: epoch %d from cells %d and %d", i-1, i, v.Epoch, prev.Cell, v.Cell)
				continue
			}
			gap := int(v.Tick) - int(prev.Tick)
			c.worstBeats = max(c.worstBeats, gap)
			if gap > rule.beats {
				t.Errorf("view gap of %d beats of cell %d between view %d (tick %d) and %d (tick %d), wall %v: exceeds %d beats",
					gap, v.Cell, i-1, prev.Tick, i, v.Tick, v.At.Sub(prev.At), rule.beats)
			}
			continue
		}
		// 换手：旧格（prev.Cell, prev.Epoch）冻结 → 新格（v.Cell, v.Epoch）装入；中间可能经过别的格（epoch 跳了不止 1），
		// 那段一并算在消息段里。
		released, okRel := tl.realAt(prev.Cell, v.Player, prev.Epoch, false)
		acquired, okAcq := tl.realAt(v.Cell, v.Player, v.Epoch, true)
		if !okRel || !okAcq {
			t.Errorf("views %d and %d: no record of epoch %d leaving cell %d (%v) or epoch %d arriving in cell %d (%v)",
				i-1, i, prev.Epoch, prev.Cell, okRel, v.Epoch, v.Cell, okAcq)
			continue
		}
		before := tl.beatsUpTo(prev.Cell, released) - int(prev.Tick)
		after := int(v.Tick) - tl.beatsUpTo(v.Cell, acquired)
		transit := acquired.Sub(released)
		c.worstEdge, c.worstTransit = max(c.worstEdge, before, after), max(c.worstTransit, transit)
		if before > rule.edge || after > rule.edge || transit > rule.transit {
			t.Errorf("view gap between view %d (cell %d epoch %d tick %d) and %d (cell %d epoch %d tick %d), wall %v: old cell idle %d beats before the freeze, "+
				"new cell idle %d beats after the install (limit %d each), freeze to install %v (limit %v)",
				i-1, prev.Cell, prev.Epoch, prev.Tick, i, v.Cell, v.Epoch, v.Tick, v.At.Sub(prev.At), before, after, rule.edge, transit, rule.transit)
		}
	}
	return c
}

// checkClient 检查客户端（Base 进程 Manager 推出的帧）看到的连续性：玩家附近（离开半径以内，减一段余量）的静止 NPC
// 从未被 remove（交接不会让对象 remove 再 create）；结束时离玩家不远（进入半径以内，减余量）的都在客户端手里。
// 结束时的持有要等最后的 witness 与帧到达：等到成立（有期限），再查整段历史里的 remove。
func (w *world) checkClient(unique int64, npcs map[int64]spatial.Point) {
	w.t.Helper()
	const margin = 150
	eventually(w.t, settleWithin, fmt.Sprintf("client of player %d to hold what is near it", unique), func() (bool, string) {
		last, ok := w.lastView(unique)
		if !ok {
			return true, ""
		}
		held := w.base.client.held(unique)
		var missing []string
		for id, p := range npcs {
			if _, ok := held[id]; !ok && spatial.WithinDistance(last.Self.Pos(), p, baseEnterRadius-margin) {
				missing = append(missing, fmt.Sprintf("npc %d at %v", id, p))
			}
		}
		if _, ok := held[actorID(unique)]; !ok {
			missing = append(missing, "its own SceneActor")
		}
		return len(missing) == 0, fmt.Sprintf("player at %v does not hold %s", last.Self.Pos(), strings.Join(missing, ", "))
	})
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
	for _, r := range w.base.client.removed(unique) {
		p, known := npcs[r.id]
		if !known {
			continue
		}
		if self, ok := posAt(r.at); ok && spatial.WithinDistance(self, p, baseLeaveRadius-margin) {
			w.t.Errorf("client of player %d: npc %d at %v removed while the player was at %v (within %d)", unique, r.id, p, self, baseLeaveRadius-margin)
		}
	}
}

func boundaryNPCs() map[int64]spatial.Point {
	return map[int64]spatial.Point{
		npcID(201): {X: 9500, Y: 5000}, npcID(202): {X: 9800, Y: 5400}, npcID(203): {X: 9950, Y: 4700},
		npcID(204): {X: 10050, Y: 5300}, npcID(205): {X: 10300, Y: 5000}, npcID(206): {X: 10700, Y: 4900},
	}
}

// 玩家沿直线 8800 → 11400 穿过 A/B 边界（10 m/s）：witness 与客户端视野连续，交接一次，B 成为唯一 Real。
func scenarioStraightCrossing(t *testing.T, w *world, maxMiss int, rule gapRule) {
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 8800, Y: 5000})
	w.resetViews(1)
	w.move(1, 1000, 0)
	w.waitView(1, travelWithin, "reach x>=11300", func(v View) bool { return v.Self.X >= 11300 })
	w.move(1, 0, 0)
	cont := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, rule, w)
	t.Logf("views=%d %v", len(w.viewsOf(1)), cont)
	w.checkClient(1, npcs)
	sid, epoch := w.owner(1)
	if sid != w.grid.SID(b) || epoch != 2 {
		t.Fatalf("owner = sid %d epoch %d, want B epoch 2", sid, epoch)
	}
	w.assertSingleReal(actorID(1), b)
}

// 在边界来回抖动（幅度 > 迟滞，反复交接）：witness 与客户端视野连续，每次交接 epoch +1，任一时刻至多一个 Real。
// lastLeg 非 nil 时在最后一段（第 12 次过界）开始前调用，用来给最后一次交接注入故障。
func scenarioJitter(t *testing.T, w *world, maxMiss int, rule gapRule, lastLeg func()) {
	npcs := boundaryNPCs()
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9990, Y: 5000})
	w.resetViews(1)
	for i := range 12 {
		vx := int64(1000)
		if i%2 == 1 {
			vx = -1000
		}
		if i == 11 && lastLeg != nil {
			lastLeg()
		}
		w.move(1, vx, 0)
		w.waitView(1, settleWithin, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10130
			}
			return v.Self.X <= 9870
		})
	}
	w.move(1, 0, 0)
	// RV-20261106-02：停下时最后一次交接可能还没收尾（旧格等 OfferReply 或隐式确认、区域表等角色的报告），这些都是异步
	// 的几跳，固定 sleep 在完整包高负载下不够。等到交接真的收尾（有期限、超时打印诊断），再做与原来相同的断言。
	sid, epoch := w.waitSettled(1, settleWithin)
	if aborted := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }); aborted != 0 {
		t.Fatalf("%d handoff(s) settled by abort, not by the target's answer", aborted) // 放弃也会让旧格的记录消失，不算收尾
	}
	cont := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, rule, w)
	w.checkClient(1, npcs)
	t.Logf("views=%d %v final owner sid=%d epoch=%d", len(w.viewsOf(1)), cont, sid, epoch)
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
func scenarioDiagonal(t *testing.T, w *world, maxMiss int, rule gapRule) {
	npcs := map[int64]spatial.Point{
		npcID(301): {X: 9600, Y: 9700}, npcID(302): {X: 10300, Y: 9700}, npcID(303): {X: 9700, Y: 10300}, npcID(304): {X: 10400, Y: 10400},
		npcID(305): {X: 9950, Y: 10050}, npcID(306): {X: 10050, Y: 9950}, npcID(307): {X: 10000, Y: 10000},
	}
	w.spawnAll(npcs)
	w.enter(1, spatial.Point{X: 9300, Y: 9300})
	w.resetViews(1)
	w.move(1, 700, 700)
	w.waitView(1, travelWithin, "reach corner+700", func(v View) bool { return v.Self.X >= 10700 && v.Self.Y >= 10700 })
	w.move(1, 0, 0)
	// 停下时最后一次交接可能还没收尾（与 scenarioJitter 同理）：等收尾（有期限、超时打诊断），再断言 owner。
	sid, epoch := w.waitSettled(1, settleWithin)
	cont := checkContinuity(t, w.viewsOf(1), npcs, maxMiss, rule, w)
	w.checkClient(1, npcs)
	t.Logf("views=%d %v owner sid=%d epoch=%d", len(w.viewsOf(1)), cont, sid, epoch)
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
	w.waitKnown(a, target) // 前提：攻击者所在格已经看得见目标（B 的 Ghost），否则第一刀按“看不见”拒绝
	var hits []HitID
	for range 25 { // 每 100ms 一刀是场景的节奏（让攻击跨过目标的迁移），不是断言
		hits = append(hits, w.attack(1, target, 2))
		time.Sleep(100 * time.Millisecond)
	}
	results := w.waitResults(len(hits), settleWithin)
	applications := w.applications()
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
	// 目标走进 A 的交接与最后几刀的结算是异步的：等到 A 的 Real 的 HP 与结算次数一致（每次重算结算次数：结论为
	// “未知”的那刀可能稍后才结算）。多结算一次时 HP 会比任何结算次数对应的都低，等不到，按诊断失败。
	eventually(t, settleWithin, "target real in A with hp matching the applications", func() (bool, string) {
		apps := w.applications()
		s, ok := w.roleOf(a, target)
		want := int32(100 - 2*len(apps))
		return ok && s.Role == "real" && s.State.HP == want,
			fmt.Sprintf("A holds %+v (known %v), want real with hp %d (applications %d)", s, ok, want, len(apps))
	})
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

// waitKnown 等格的区域表里有 id（Real、冻结或 Ghost）：攻击、交接等的前提，代替“sleep 几拍让 Ghost 到达”。
func (w *world) waitKnown(cell CellID, id int64) {
	w.t.Helper()
	eventually(w.t, settleWithin, fmt.Sprintf("%v to know entity %d", cell, id), func() (bool, string) {
		_, ok := w.roleOf(cell, id)
		return ok, fmt.Sprintf("%v has no record of %d", cell, id)
	})
}

func loopbackSpec(t testing.TB, cols, rows int32) (worldSpec, *loopback) {
	lb := newLoopback()
	return worldSpec{grid: worldGrid(t, cols, rows), transport: func(testing.TB, string) Transport { return lb }}, lb
}

// witness 连续性的约定：静止 NPC 在半径内时连续缺失不超过 1 份；同一格相邻两份之间不超过 4 拍（原约定“3 拍 + 50ms”
// 按拍数取整），换手时两端各自空过不超过 2 拍、消息段不超过 transitCap（见 gapRule、checkContinuity）。
const contMaxMiss = 1

var contGap = gapRule{beats: 4, edge: 2, transit: transitCap}

func TestWorldStraightCrossing(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioStraightCrossing(t, startWorld(t, spec), contMaxMiss, contGap)
}

// 接 demo 时发现（2026-11-03）：正式进程的 getter 是装了 DataEngine 加载器的 ManagerAccess，原先对不在内存的不持久实体
// （SceneActor / Monster）返回“聚合不存在”的错误而不是 nil，Node 把还不存在的角色放进加锁集合（交接装入、进入、生成、
// 伤害打到不在本格的目标）时整个请求失败。框架统一后（E1 = B）ManagerAccess 对不持久种类内存未命中答 nil、不问加载器，
// Node 不再自己过滤加锁集合：同一组场景在加载器替身下照常通过，加载器一次都没被问到。
func TestWorldOnALoaderBackedGetter(t *testing.T) {
	t.Run("straight", func(t *testing.T) {
		spec, _ := loopbackSpec(t, 2, 1)
		spec.strict = true
		w := startWorld(t, spec)
		scenarioStraightCrossing(t, w, contMaxMiss, contGap)
		w.requireLoaderNeverAsked(t)
	})
	t.Run("attack", func(t *testing.T) {
		spec, _ := loopbackSpec(t, 2, 1)
		spec.strict = true
		w := startWorld(t, spec)
		scenarioAttackAcrossAndDuringMigration(t, w, false)
		w.requireLoaderNeverAsked(t)
	})
}

// requireLoaderNeverAsked 断言每个进程的加载器替身都没被调用（见 procSpec.strict）。
func (w *world) requireLoaderNeverAsked(t *testing.T) {
	t.Helper()
	for _, p := range append([]*testProc{w.base}, mapValues(w.cells)...) {
		if p.loader != nil && p.loader.asked.Load() != 0 {
			t.Errorf("sid %d: the loader was asked %d time(s) for non-persistent kinds", p.sid, p.loader.asked.Load())
		}
	}
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestWorldJitterAtBoundary(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	scenarioJitter(t, startWorld(t, spec), contMaxMiss, contGap, nil)
}

// RV-20261106-02：最后一次交接（B → A）的收尾慢到达：A 发给 B 的 OfferReply 与带着玩家的 Ghost 更新（隐式确认）都晚
// 1 秒，旧格 B 在玩家停下之后还冻结着一段时间。断言与上面相同，只是不能靠固定 sleep 恰好等过收尾。
func TestWorldJitterAtBoundarySlowHandoffConclusion(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	b := Subject("w", w.grid.SID(CellID{1, 0}))
	scenarioJitter(t, w, contMaxMiss, contGap, func() {
		lb.setFault(func(subject string, m *wireMsg) (bool, bool, time.Duration) {
			if subject != b {
				return false, false, 0
			}
			if m.Kind == kindOfferReply {
				return false, false, time.Second
			}
			if m.Kind == kindGhost && slices.ContainsFunc(m.Ghost.Updates, func(s EntityState) bool { return s.ID == actorID(1) }) {
				return false, false, time.Second
			}
			return false, false, 0
		})
	})
}

func TestWorldDiagonalThroughFourCellCorner(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 2)
	scenarioDiagonal(t, startWorld(t, spec), contMaxMiss, contGap)
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
		w.waitView(1, travelWithin, "turn point", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10500
			}
			return v.Self.X <= 9500
		})
	}
	w.move(1, 0, 0)
	sid, epoch := w.waitSettled(1, settleWithin)
	// 丢包只在格与格之间（Cell ↔ Base 不丢）：同格间隔与两端照常；交接靠重传收敛，消息段取原约定的 2s。
	cont := checkContinuity(t, w.viewsOf(1), npcs, 3, gapRule{beats: contGap.beats, edge: contGap.edge, transit: 2 * time.Second}, w)
	t.Logf("views=%d %v owner=%d epoch=%d", len(w.viewsOf(1)), cont, sid, epoch)
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
