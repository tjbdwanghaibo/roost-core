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

// 原型测量（ROOST_CELLWORLD_PERF=1 才跑）：2×2 网格、每格一个 OS 进程、真实 nats-server；每格 N 个随机游走的
// NPC（3 m/s，碰世界边缘反弹，跨格就交接），父进程 Base 上 40 个玩家随机游走（5 m/s，每 2s 换方向）。
// 预热后测一个窗口，子进程报每拍耗时、CPU、消息量、Ghost 延迟、交接延迟，父进程报视野延迟。
// 档位用 ROOST_CELLWORLD_PERF_NPCS（逗号分隔，缺省 "1000,5000"），窗口 ROOST_CELLWORLD_PERF_SECONDS（缺省 20）。
// 只是原型量级数据：单机 macOS、Cell 与 nats-server 同机，结论不能外推到跨机部署。

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
	for _, n := range levels {
		t.Run(strconv.Itoa(n), func(t *testing.T) { measureLevel(t, n, window) })
	}
}

func measureLevel(t *testing.T, npcsPerCell int, window time.Duration) {
	url := startNATSServer(t)
	base := childSpec{URL: url, Cols: 2, Rows: 2, CellSize: 50000, Band: 3000, Hysteresis: 200, ViewRadius: 2500,
		RandomNPCs: npcsPerCell, NPCSpeed: 300, Seed: 42}
	grid, err := base.grid()
	if err != nil {
		t.Fatal(err)
	}
	var procs []*cellProcess
	for i, id := range grid.Cells() {
		spec := base
		spec.Col, spec.Row = id.Col, id.Row
		spec.IDBase = int64(1_000_000 * (i + 1))
		procs = append(procs, startCellProcess(t, spec))
	}
	viewLat := newSampler()
	b, err := NewBase(BaseConfig{World: "mp", SID: worldBaseSID, Grid: grid, Transport: NATSTransport(dialNATS(t, url)),
		OnView: func(v View) { viewLat.add(ms(v.At.Sub(v.SentAt))) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = b.Run(ctx) }()
	defer func() { cancel(); <-done }()
	<-b.Ready()
	const players = 40
	r := rand.New(rand.NewPCG(7, 7))
	bounds := grid.Bounds()
	for id := int64(1); id <= players; id++ {
		p := spatial.Point{X: bounds.Min.X + 1000 + r.Int64N(bounds.Max.X-bounds.Min.X-2000), Y: bounds.Min.Y + 1000 + r.Int64N(bounds.Max.Y-bounds.Min.Y-2000)}
		if err := b.Enter(id, p, 100); err != nil {
			t.Fatal(err)
		}
	}
	steer := func() {
		for id := int64(1); id <= players; id++ {
			_ = b.Move(id, r.Int64N(1001)-500, r.Int64N(1001)-500)
		}
	}
	steer()
	time.Sleep(3 * time.Second) // 预热
	viewLat.reset()
	for _, p := range procs {
		p.send("reset")
	}
	baseStart := b.Stats()
	end := time.Now().Add(window)
	for time.Now().Before(end) {
		time.Sleep(2 * time.Second)
		steer()
	}
	baseEnd := b.Stats()
	vp50, vp99, vmx, vn := viewLat.quantiles()
	t.Logf("level %d NPC/cell, %d players, window %v; base views accepted %d stale %d owner changes %d re-enters %d; view latency p50 %.2fms p99 %.2fms max %.2fms (n=%d)",
		npcsPerCell, players, window, baseEnd.ViewsAccepted-baseStart.ViewsAccepted, baseEnd.ViewsStale-baseStart.ViewsStale,
		baseEnd.OwnerChanges-baseStart.OwnerChanges, baseEnd.Reenters-baseStart.Reenters, vp50, vp99, vmx, vn)
	for i, p := range procs {
		var st *childStat
		for _, e := range p.stop(t) {
			if e.Ev == "stats" {
				st = e.Stats
			}
		}
		if st == nil {
			t.Fatalf("cell %v printed no stats", grid.Cells()[i])
		}
		c := st.Cell
		t.Logf("cell %v: reals %d ghosts %d | CPU %.1f%% of one core | tick p50 %.2fms p99 %.2fms max %.2fms | out %.0f msg/s %.0f KB/s, in %.0f msg/s %.0f KB/s | ghost latency p50 %.2fms p99 %.2fms (n=%d) | handoffs %d (p50 %.2fms p99 %.2fms), rejected %d aborted %d retransmits %d | inbox drops %d conflicts %d",
			grid.Cells()[i], st.RealsEnd, st.GhostsEnd, 100*st.CPU/st.Wall, st.TickP50, st.TickP99, st.TickMx,
			float64(c.MsgsOut)/st.Wall, float64(c.BytesOut)/1024/st.Wall, float64(c.MsgsIn)/st.Wall, float64(c.BytesIn)/1024/st.Wall,
			st.GhostP50, st.GhostP99, st.GhostSamples, st.Handoffs, st.HandoffP50, st.HandoffP99,
			c.HandoffsRejected, c.HandoffsAborted, c.OfferRetransmits, c.InboxDrops, c.Conflicts)
		if c.Conflicts != 0 || c.HandoffsAborted != 0 {
			t.Errorf("cell %v: conflicts %d aborted %d", grid.Cells()[i], c.Conflicts, c.HandoffsAborted)
		}
	}
}
