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

// 单机多进程容量测量：真实 App / Redis 授权 / NATS，Base 经正式 Sync 编解码。
// 客户端传输与持久提交是测试替身；真实账号/TCP/存储联合验证走 test-seamless-demo.sh。
// ROOST_CELLWORLD_PERF_COUNT=1000、NPCS=2250 对应 1000 玩家 + 9000 NPC，20Hz。
// PLAYERS=spread|cell 控制分散/热点，SECONDS 控制测量时长；随机种子固定。

func TestCellWorldMeasure(t *testing.T) {
	if os.Getenv("ROOST_CELLWORLD_PERF") != "1" {
		t.Skip("set ROOST_CELLWORLD_PERF=1 to run the multiprocess capacity measurement")
	}
	levels := []int{2250}
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
	players := 1000
	if raw := os.Getenv("ROOST_CELLWORLD_PERF_COUNT"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 10000 {
			t.Fatalf("bad player count %q", raw)
		}
		players = v
	}
	for _, n := range levels {
		t.Run(strconv.Itoa(n), func(t *testing.T) { measureLevel(t, n, players, window, idleMS, concentrated) })
	}
}

func measureLevel(t *testing.T, npcsPerCell, players int, window time.Duration, idleMS int, concentrated bool) {
	active := 100
	if raw := os.Getenv("ROOST_CELLWORLD_PERF_ACTIVE"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 100 {
			t.Fatalf("bad active percent %q", raw)
		}
		active = v
	}
	t.Logf("moving entities=%d%%; short=8 queue=65536 io=8 queue=16; GOMAXPROCS inherited", active)

	viewLat, visible := newSampler(), newSampler()
	mp := startMPWorld(t, 2, 2, func(s *childSpec) {
		s.CellSize, s.Band, s.Hysteresis, s.ViewRadius = 7500, 900, 100, 800
		s.RandomNPCs, s.NPCSpeed, s.Seed, s.Events = npcsPerCell, 300, 42, false
		s.IdleMS, s.MaxView, s.ActivePercent = idleMS, 64, active
	})
	// Witness 覆盖 Base 的 600/700 AOI 半径；实际可见数单独报告。
	w := mp.world
	w.base.stop()
	w.base = startProc(t, procSpec{sid: worldBaseSID, base: true, grid: w.grid, transport: NATSTransport(dialNATS(t, mp.url)), mod: func(cfg *NodeConfig) {
		cfg.World, cfg.ViewRadius = "mp", 800
		cfg.Hooks.Witness = func(v View) { viewLat.add(ms(v.At.Sub(v.SentAt))); visible.add(float64(len(v.Entities))) }
	}})
	t.Cleanup(w.base.stop)
	for i, id := range w.grid.Cells() {
		spec := mp.cellSpec(id, nil)
		spec.IDBase = int64(1_000_000 * (i + 1))
		mp.procs[id] = startCellProcess(t, spec)
	}
	r := rand.New(rand.NewPCG(7, 7))
	bounds := w.grid.Bounds()
	if concentrated {
		bounds = w.grid.Rect(CellID{0, 0})
	}
	ctx, cancel := context.WithTimeout(context.Background(), window+time.Minute)
	defer cancel()
	for id := int64(1); id <= int64(players); id++ {
		p := spatial.Point{X: bounds.Min.X + 1000 + r.Int64N(bounds.Max.X-bounds.Min.X-2000), Y: bounds.Min.Y + 1000 + r.Int64N(bounds.Max.Y-bounds.Min.Y-2000)}
		pl := w.base.player(id)
		if err := w.base.node.Enter(ctx, pl.ID(), actorID(id), p, 100); err != nil {
			t.Fatal(err)
		}
	}
	steer := func() {
		for id := int64(1); id <= int64(players); id++ {
			if int(id)%100 >= active {
				continue
			}
			if err := w.base.node.Move(ctx, playerEntityID(id), r.Int64N(1001)-500, r.Int64N(1001)-500); err != nil {
				t.Fatal(err)
			}
		}
	}
	steer()
	time.Sleep(6 * time.Second) // 预热（D1：降档滞后 IdleAfter 缺省 2s，预热要盖过它）
	viewLat.reset()
	visible.reset()
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
	if vn == 0 {
		t.Error("no witness samples during measurement")
	}
	if vp99 > 50 {
		t.Errorf("witness p99 %.2fms exceeds 50ms", vp99)
	}
	if baseEnd.ReplicaGaps > baseStart.ReplicaGaps {
		t.Errorf("replica gaps during measurement: %d", baseEnd.ReplicaGaps-baseStart.ReplicaGaps)
	}
	visible50, visible99, visibleMax, _ := visible.quantiles()
	t.Logf("witness entity counts p50=%.0f p99=%.0f max=%.0f (Base AOI can further filter)", visible50, visible99, visibleMax)
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
		t.Logf("cell %v: replica full %d delta %d gaps %d; coalesced %d control exhausted %d max queue age %.2fms pending %d; owner errors %d full %d", id, c.ReplicaFull, c.ReplicaDelta, c.ReplicaGaps, c.StatesCoalesced, c.ControlExhausted, float64(c.QueueAgeMaxNS)/1e6, c.OutboundPending, c.OwnershipErrors, c.OwnershipQueueFull)
		if c.OutboxDrops+c.DispatchErrors+c.ControlExhausted+c.DecodeErrors != 0 {
			t.Errorf("cell %v lost work: %+v", id, c)
		}
		if c.Conflicts != 0 || c.HandoffsAborted != 0 {
			t.Errorf("cell %v: conflicts %d aborted %d", id, c.Conflicts, c.HandoffsAborted)
		}
	}
}
