package cellworld

import (
	"context"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 原型测量（ROOST_CELLWORLD_PERF=1 才跑），与 v1 同一组负载以便对照：2×2 网格、每格一个 OS 进程（v2：每个都是带单实例锁
// 的 App）、真实 nats-server；每格 N 个随机游走的 NPC（3 m/s，碰世界边缘反弹，跨格就交接），父进程 Base 上 40 个玩家
// 随机游走（5 m/s，每 2s 换方向）。v2 的 NPC / 玩家都是 Nest 实体、各自一个 50ms 心跳，区域表在 Scene 实体上；Base 进程
// 多了 Sync mirror（witness 喂给 Manager、推给客户端替身）。预热后测一个窗口，子进程报 CPU、区域心跳耗时、消息量、Ghost
// 延迟、交接延迟，父进程报 witness 延迟。档位 ROOST_CELLWORLD_PERF_NPCS（缺省 "1000,5000"），窗口
// ROOST_CELLWORLD_PERF_SECONDS（缺省 20）。D1：ROOST_CELLWORLD_PERF_IDLE_MS（缺省 0 = 不降档，与 v2 相同；1000 = 无人
// 在附近的 NPC 降到 1s 档），ROOST_CELLWORLD_PERF_PLAYERS=spread（缺省，玩家随机散在整个世界）| cell（全部集中在
// 格 (0,0) 内）。只是原型量级：单机 macOS、Cell 与 nats-server 同机。

func TestCellWorldMeasure(t *testing.T) {
	if os.Getenv("ROOST_CELLWORLD_PERF") != "1" {
		t.Skip("set ROOST_CELLWORLD_PERF=1 to run the prototype measurement")
	}
	levels := []int{1000, 5000}
	if raw := os.Getenv("ROOST_CELLWORLD_PERF_NPCS"); raw != "" {
		levels = nil
		for _, f := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(f)
			if err != nil || n <= 0 {
				t.Fatalf("bad ROOST_CELLWORLD_PERF_NPCS %q", raw)
			}
			levels = append(levels, n)
		}
	}
	window := 20 * time.Second
	if raw := os.Getenv("ROOST_CELLWORLD_PERF_SECONDS"); raw != "" {
		s, err := strconv.Atoi(raw)
		if err != nil || s <= 0 {
			t.Fatalf("bad ROOST_CELLWORLD_PERF_SECONDS %q", raw)
		}
		window = time.Duration(s) * time.Second
	}
	idleMS := 0
	if raw := os.Getenv("ROOST_CELLWORLD_PERF_IDLE_MS"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			t.Fatalf("bad ROOST_CELLWORLD_PERF_IDLE_MS %q", raw)
		}
		idleMS = v
	}
	concentrated := false
	switch raw := os.Getenv("ROOST_CELLWORLD_PERF_PLAYERS"); raw {
	case "", "spread":
	case "cell":
		concentrated = true
	default:
		t.Fatalf("bad ROOST_CELLWORLD_PERF_PLAYERS %q", raw)
	}
	for _, n := range levels {
		t.Run(strconv.Itoa(n), func(t *testing.T) { measureLevel(t, n, window, idleMS, concentrated) })
	}
}

func measureLevel(t *testing.T, npcsPerCell int, window time.Duration, idleMS int, concentrated bool) {
	viewLat := newSampler()
	mp := startMPWorld(t, 2, 2, func(s *childSpec) {
		s.CellSize, s.Band, s.Hysteresis, s.ViewRadius = 50000, 3000, 200, 2500
		s.RandomNPCs, s.NPCSpeed, s.Seed, s.Events = npcsPerCell, 300, 42, false
		s.IdleMS = idleMS
	})
	// 测量的 Base 用与 v1 相同的视野半径；Base 的 witness 观察点只记延迟。
	w := mp.world
	w.base.stop()
	w.base = startProc(t, procSpec{sid: worldBaseSID, base: true, grid: w.grid, transport: NATSTransport(dialNATS(t, mp.url)), mod: func(cfg *NodeConfig) {
		cfg.World, cfg.ViewRadius = "mp", 2500
		cfg.Hooks.Witness = func(v View) { viewLat.add(ms(v.At.Sub(v.SentAt))) }
	}})
	t.Cleanup(w.base.stop)
	for i, id := range w.grid.Cells() {
		spec := mp.cellSpec(id, nil)
		spec.IDBase = int64(1_000_000 * (i + 1))
		mp.procs[id] = startCellProcess(t, spec)
	}
	const players = 40
	r := rand.New(rand.NewPCG(7, 7))
	bounds := w.grid.Bounds()
	if concentrated {
		bounds = w.grid.Rect(CellID{0, 0})
	}
	ctx, cancel := context.WithTimeout(context.Background(), window+time.Minute)
	defer cancel()
	for id := int64(1); id <= players; id++ {
		p := spatial.Point{X: bounds.Min.X + 1000 + r.Int64N(bounds.Max.X-bounds.Min.X-2000), Y: bounds.Min.Y + 1000 + r.Int64N(bounds.Max.Y-bounds.Min.Y-2000)}
		pl := w.base.player(id)
		if err := w.base.node.Enter(ctx, pl.ID(), actorID(id), p, 100); err != nil {
			t.Fatal(err)
		}
	}
	steer := func() {
		for id := int64(1); id <= players; id++ {
			_ = w.base.node.Move(ctx, playerEntityID(id), r.Int64N(1001)-500, r.Int64N(1001)-500)
		}
	}
	steer()
	time.Sleep(6 * time.Second) // 预热（D1：降档滞后 IdleAfter 缺省 2s，预热要盖过它）
	viewLat.reset()
	for _, p := range mp.procs {
		p.send("reset")
	}
	baseStart, cpuStart, wallStart := w.base.node.Stats(), cpuSeconds(), time.Now()
	end := time.Now().Add(window)
	for time.Now().Before(end) {
		time.Sleep(2 * time.Second)
		steer()
	}
	baseEnd, cpuEnd, wall := w.base.node.Stats(), cpuSeconds(), time.Since(wallStart)
	vp50, vp99, vmx, vn := viewLat.quantiles()
	t.Logf("idle period %dms, players concentrated in one cell %v", idleMS, concentrated)
	t.Logf("level %d NPC/cell, %d players, window %v; base (parent process, incl. Sync mirrors and client decode) CPU %.1f%% of one core; views accepted %d stale %d owner changes %d re-enters %d; witness latency p50 %.2fms p99 %.2fms max %.2fms (n=%d); mirrors %d",
		npcsPerCell, players, window, 100*(cpuEnd-cpuStart)/wall.Seconds(), baseEnd.ViewsAccepted-baseStart.ViewsAccepted, baseEnd.ViewsStale-baseStart.ViewsStale,
		baseEnd.OwnerChanges-baseStart.OwnerChanges, baseEnd.Reenters-baseStart.Reenters, vp50, vp99, vmx, vn, w.base.mirrors.Len())
	for _, id := range w.grid.Cells() {
		var st *childStat
		for _, e := range mp.procs[id].stop(t) {
			if e.Ev == "stats" {
				st = e.Stats
			}
		}
		if st == nil {
			t.Fatalf("cell %v printed no stats", id)
		}
		c := st.Node
		t.Logf("cell %v: idle NPCs %d of %d reals, actor beats %.0f/s, tier changes idle %d fast %d", id, c.IdleNPCs, c.Reals,
			float64(c.ActorBeats)/st.Wall, c.TierIdle, c.TierFast)
		t.Logf("cell %v: reals %d ghosts %d | CPU %.1f%% of one core | region beat p50 %.2fms p99 %.2fms max %.2fms | out %.0f msg/s %.0f KB/s, in %.0f msg/s %.0f KB/s | ghost latency p50 %.2fms p99 %.2fms (n=%d) | handoffs %d (p50 %.2fms p99 %.2fms), rejected %d aborted %d retransmits %d | outbox drops %d dispatch errors %d conflicts %d",
			id, c.Reals, c.Ghosts, 100*st.CPU/st.Wall, st.TickP50, st.TickP99, st.TickMx,
			float64(c.MsgsOut)/st.Wall, float64(c.BytesOut)/1024/st.Wall, float64(c.MsgsIn)/st.Wall, float64(c.BytesIn)/1024/st.Wall,
			st.GhostP50, st.GhostP99, st.GhostSamples, st.Handoffs, st.HandoffP50, st.HandoffP99,
			c.HandoffsRejected, c.HandoffsAborted, c.OfferRetransmits, c.OutboxDrops, c.DispatchErrors, c.Conflicts)
		if c.Conflicts != 0 || c.HandoffsAborted != 0 {
			t.Errorf("cell %v: conflicts %d aborted %d", id, c.Conflicts, c.HandoffsAborted)
		}
	}
}
