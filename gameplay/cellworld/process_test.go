package cellworld

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 多进程：每个 Cell 是测试二进制自 exec 出来的独立 OS 进程（TestCellProcessChild），经真实 nats-server 通信；
// 父进程只跑 Base。子进程把 Real 取得 / 失去、伤害结算等事件按行写到 stdout（JSON，带 pid 与系统时间），
// 父进程据此跨进程检查“同一 (实体, epoch) 只有一个进程取得 Real、Real 区间不重叠”。父进程关闭子进程 stdin
// 即让它退出并打印统计。

const childEnv = "ROOST_CELLWORLD_CHILD"

// childSpec 是父进程交给子进程的全部配置（JSON 放在环境变量里）。
type childSpec struct {
	URL        string        `json:"url"`
	Cols       int32         `json:"cols"`
	Rows       int32         `json:"rows"`
	CellSize   int64         `json:"size"`
	Band       int64         `json:"band"`
	Hysteresis int64         `json:"hyst"`
	ViewRadius int64         `json:"view"`
	Col        int32         `json:"col"`
	Row        int32         `json:"row"`
	Spawn      []EntityState `json:"spawn"`
	// RandomNPCs > 0 时在本格随机位置生成这么多 NPC（ID 从 IDBase 起），速度 NPCSpeed、方向随机。
	RandomNPCs int    `json:"npcs"`
	IDBase     int64  `json:"idbase"`
	NPCSpeed   int64  `json:"speed"`
	Seed       uint64 `json:"seed"`
	// Events 为 true 时逐条输出 Real / 伤害事件（功能用例）；测量时关闭，只输出汇总。
	Events bool `json:"events"`
}

func (s childSpec) grid() (*Grid, error) {
	owners := map[CellID]int32{}
	for c := range s.Cols {
		for r := range s.Rows {
			owners[CellID{c, r}] = 101 + r*s.Cols + c
		}
	}
	return NewGrid(GridConfig{CellSize: s.CellSize, Cols: s.Cols, Rows: s.Rows, GhostBand: s.Band, Hysteresis: s.Hysteresis, Owners: owners})
}

// childEvent 是子进程输出的一行。
type childEvent struct {
	Ev    string     `json:"ev"`
	PID   int        `json:"pid"`
	Cell  CellID     `json:"cell"`
	ID    int64      `json:"id,omitempty"`
	Epoch uint64     `json:"epoch,omitempty"`
	Acq   bool       `json:"acq,omitempty"`
	Hit   HitID      `json:"hit,omitzero"`
	T     int64      `json:"t"`
	Stats *childStat `json:"stats,omitempty"`
}

// childStat 是子进程退出时的汇总（从收到 reset 起算）。
type childStat struct {
	Wall float64   `json:"wall_s"`
	CPU  float64   `json:"cpu_s"`
	Cell CellStats `json:"cell"`
	// 以下均为毫秒（Tick 是每拍处理耗时，Ghost 是发出到邻格应用，Handoff 是冻结到得到确认）。
	TickP50, TickP99, TickMx float64
	GhostP50, GhostP99       float64
	HandoffP50, HandoffP99   float64
	GhostSamples, Handoffs   int
	RealsEnd, GhostsEnd      uint64
}

// sampler 是有界样本集：超过上限后按水库抽样替换，测量期内存不随时间增长。
type sampler struct {
	mu   sync.Mutex
	vals []float64
	seen int
	r    *rand.Rand
}

const samplerCap = 200000

func newSampler() *sampler { return &sampler{r: rand.New(rand.NewPCG(1, 2))} }

func (s *sampler) add(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	if len(s.vals) < samplerCap {
		s.vals = append(s.vals, v)
		return
	}
	if i := s.r.IntN(s.seen); i < samplerCap {
		s.vals[i] = v
	}
}

func (s *sampler) reset() {
	s.mu.Lock()
	s.vals, s.seen = s.vals[:0], 0
	s.mu.Unlock()
}

// quantiles 返回 p50、p99、最大值与样本数（毫秒）。
func (s *sampler) quantiles() (p50, p99, mx float64, n int) {
	s.mu.Lock()
	vals := slices.Clone(s.vals)
	s.mu.Unlock()
	if len(vals) == 0 {
		return 0, 0, 0, 0
	}
	sort.Float64s(vals)
	at := func(q float64) float64 { return vals[min(len(vals)-1, int(q*float64(len(vals))))] }
	return at(0.5), at(0.99), vals[len(vals)-1], len(vals)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func cpuSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime)
}

// TestCellProcessChild 不是独立用例：只在父进程设置 childEnv 时作为 Cell 子进程运行。
func TestCellProcessChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("helper process for TestCellsInSeparateProcesses / TestCellWorldMeasure")
	}
	os.Exit(runCellChild(os.Getenv(childEnv)))
}

func runCellChild(raw string) int {
	var spec childSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "child spec:", err)
		return 2
	}
	grid, err := spec.grid()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var outMu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	pid := os.Getpid()
	id := CellID{spec.Col, spec.Row}
	emit := func(e childEvent) {
		e.PID, e.Cell, e.T = pid, id, time.Now().UnixNano()
		outMu.Lock()
		_ = enc.Encode(e)
		outMu.Unlock()
	}
	tb := &childTB{}
	client := dialNATS(tb, spec.URL)
	defer tb.cleanup()
	ticks, ghosts, handoffs := newSampler(), newSampler(), newSampler()
	cfg := CellConfig{World: "mp", Grid: grid, Cell: id, Transport: NATSTransport(client), ViewRadius: spec.ViewRadius, Period: worldPeriod}
	cfg.Hooks = Hooks{
		TickDone:     func(_ CellID, d time.Duration, _, _ int) { ticks.add(ms(d)) },
		GhostApplied: func(_ CellID, d time.Duration, _ int) { ghosts.add(ms(d)) },
		HandoffDone: func(_ CellID, _ int64, _ uint64, accepted bool, d time.Duration) {
			if accepted {
				handoffs.add(ms(d))
			}
		},
	}
	if spec.Events {
		cfg.Hooks.RealChanged = func(_ CellID, eid int64, epoch uint64, acq bool) {
			emit(childEvent{Ev: "real", ID: eid, Epoch: epoch, Acq: acq})
		}
		cfg.Hooks.DamageApplied = func(_ CellID, target int64, hit HitID, _ int32) {
			emit(childEvent{Ev: "hit", ID: target, Hit: hit})
		}
	}
	cell, err := NewCell(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = cell.Run(ctx) }()
	defer func() { cancel(); <-done }()
	<-cell.Ready()
	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer spawnCancel()
	for _, st := range spec.Spawn {
		if err := cell.SpawnNPC(spawnCtx, st); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	if spec.RandomNPCs > 0 {
		r := rand.New(rand.NewPCG(spec.Seed, uint64(spec.Col)<<32|uint64(spec.Row)))
		rect := grid.Rect(id)
		for i := range spec.RandomNPCs {
			angle := r.Float64() * 6.283185307179586
			st := EntityState{ID: spec.IDBase + int64(i), HP: 100,
				X: rect.Min.X + r.Int64N(rect.Max.X-rect.Min.X), Y: rect.Min.Y + r.Int64N(rect.Max.Y-rect.Min.Y),
				VX: int64(float64(spec.NPCSpeed) * math.Cos(angle)), VY: int64(float64(spec.NPCSpeed) * math.Sin(angle))}
			if err := cell.SpawnNPC(spawnCtx, st); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
		}
	}
	emit(childEvent{Ev: "ready"})
	start, cpuStart, statStart := time.Now(), cpuSeconds(), cell.Stats()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() { // "reset" 开始测量窗口；EOF 结束
		if strings.TrimSpace(in.Text()) == "reset" {
			ticks.reset()
			ghosts.reset()
			handoffs.reset()
			start, cpuStart, statStart = time.Now(), cpuSeconds(), cell.Stats()
		}
	}
	end, cpuEnd, statEnd := time.Now(), cpuSeconds(), cell.Stats()
	tp50, tp99, tmx, _ := ticks.quantiles()
	gp50, gp99, _, gn := ghosts.quantiles()
	hp50, hp99, _, hn := handoffs.quantiles()
	emit(childEvent{Ev: "stats", Stats: &childStat{
		Wall: end.Sub(start).Seconds(), CPU: cpuEnd - cpuStart, Cell: diffStats(statEnd, statStart),
		TickP50: tp50, TickP99: tp99, TickMx: tmx, GhostP50: gp50, GhostP99: gp99, HandoffP50: hp50, HandoffP99: hp99,
		GhostSamples: gn, Handoffs: hn, RealsEnd: statEnd.Reals, GhostsEnd: statEnd.Ghosts,
	}})
	return 0
}

func diffStats(a, b CellStats) CellStats {
	return CellStats{
		Ticks: a.Ticks - b.Ticks, MsgsOut: a.MsgsOut - b.MsgsOut, BytesOut: a.BytesOut - b.BytesOut,
		MsgsIn: a.MsgsIn - b.MsgsIn, BytesIn: a.BytesIn - b.BytesIn, InboxDrops: a.InboxDrops - b.InboxDrops,
		DecodeErrors: a.DecodeErrors - b.DecodeErrors, PublishErrors: a.PublishErrors - b.PublishErrors,
		StaleDrops: a.StaleDrops - b.StaleDrops, Conflicts: a.Conflicts - b.Conflicts,
		HandoffsStarted: a.HandoffsStarted - b.HandoffsStarted, HandoffsAccepted: a.HandoffsAccepted - b.HandoffsAccepted,
		HandoffsRejected: a.HandoffsRejected - b.HandoffsRejected, HandoffsAborted: a.HandoffsAborted - b.HandoffsAborted,
		OfferRetransmits: a.OfferRetransmits - b.OfferRetransmits,
	}
}

// childTB 让 dialNATS 在子进程里复用：只收集清理函数，失败直接退出。
type childTB struct {
	testing.TB
	cleanups []func()
}

func (c *childTB) Helper()           {}
func (c *childTB) Cleanup(f func())  { c.cleanups = append(c.cleanups, f) }
func (c *childTB) Fatal(args ...any) { fmt.Fprintln(os.Stderr, args...); os.Exit(2) }
func (c *childTB) Skip(args ...any)  { fmt.Fprintln(os.Stderr, args...); os.Exit(3) }
func (c *childTB) Fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(2)
}
func (c *childTB) cleanup() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
}

// cellProcess 是父进程这边对一个子进程的句柄。
type cellProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	mu     sync.Mutex
	events []childEvent
	ready  chan struct{}
	done   chan struct{}
	stderr strings.Builder
}

func startCellProcess(t *testing.T, spec childSpec) *cellProcess {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCellProcessChild$", "-test.count=1", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), childEnv+"="+string(raw))
	p := &cellProcess{cmd: cmd, ready: make(chan struct{}), done: make(chan struct{})}
	cmd.Stderr = &lockedWriter{w: &p.stderr, mu: &p.mu}
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		readyOnce := sync.Once{}
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 || line[0] != '{' {
				continue
			}
			var e childEvent
			if json.Unmarshal(line, &e) != nil {
				continue
			}
			p.mu.Lock()
			p.events = append(p.events, e)
			p.mu.Unlock()
			if e.Ev == "ready" {
				readyOnce.Do(func() { close(p.ready) })
			}
		}
	}()
	t.Cleanup(func() { // 失败路径也保证子进程退出
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	select {
	case <-p.ready:
	case <-p.done:
		t.Fatalf("cell process exited before ready: %s", p.stderrText())
	case <-time.After(60 * time.Second):
		t.Fatalf("cell process not ready: %s", p.stderrText())
	}
	return p
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func (p *cellProcess) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *cellProcess) send(line string) { _, _ = io.WriteString(p.stdin, line+"\n") }

// stop 关闭 stdin，等子进程打印统计并退出。
func (p *cellProcess) stop(t *testing.T) []childEvent {
	t.Helper()
	_ = p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("cell process did not exit: %s", p.stderrText())
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("cell process: %v: %s", err, p.stderrText())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

// checkCrossProcessReals：按系统时间排序全部进程的 Real 事件，检查同一 (实体, epoch) 只有一个进程取得，
// 任一时刻至多一个进程持有，epoch 严格递增。同一台机器上各进程的系统时间可比；冻结先于发 Offer、安装晚于收到，
// 中间隔着一次 NATS 往返，事件时间不会倒置。
func checkCrossProcessReals(t *testing.T, events []childEvent) map[int64]uint64 {
	t.Helper()
	var reals []childEvent
	for _, e := range events {
		if e.Ev == "real" {
			reals = append(reals, e)
		}
	}
	sort.SliceStable(reals, func(i, j int) bool { return reals[i].T < reals[j].T })
	holders := map[int64]map[int]uint64{}
	owner := map[[2]uint64]int{}
	last := map[int64]uint64{}
	for _, e := range reals {
		h := holders[e.ID]
		if h == nil {
			h = map[int]uint64{}
			holders[e.ID] = h
		}
		if !e.Acq {
			delete(h, e.PID)
			continue
		}
		if len(h) > 0 {
			t.Errorf("entity %d: pid %d acquired epoch %d at %d while %v still Real", e.ID, e.PID, e.Epoch, e.T, h)
		}
		key := [2]uint64{uint64(e.ID), e.Epoch}
		if prev, dup := owner[key]; dup {
			t.Errorf("entity %d epoch %d acquired by pid %d and pid %d", e.ID, e.Epoch, prev, e.PID)
		}
		owner[key] = e.PID
		if e.Epoch <= last[e.ID] {
			t.Errorf("entity %d epoch %d after %d", e.ID, e.Epoch, last[e.ID])
		}
		last[e.ID] = e.Epoch
		h[e.PID] = e.Epoch
	}
	return last
}

// 两个 Cell 各一个 OS 进程：玩家来回穿越边界（视野连续、epoch 单调、跨进程至多一个 Real），再跨界攻击 B 区
// 边界带里的 NPC（每个 HitID 只在一个进程里结算一次）。
func TestCellsInSeparateProcesses(t *testing.T) {
	url := startNATSServer(t)
	npcs := boundaryNPCs()
	npcs[7] = spatial.Point{X: 10200, Y: 5200}
	base := childSpec{URL: url, Cols: 2, Rows: 1, CellSize: 10000, Band: 1000, Hysteresis: 100, ViewRadius: worldViewRadius, Events: true}
	grid, err := base.grid()
	if err != nil {
		t.Fatal(err)
	}
	var procs []*cellProcess
	for _, id := range grid.Cells() {
		spec := base
		spec.Col, spec.Row = id.Col, id.Row
		for nid, p := range npcs {
			if c, _ := grid.CellAt(p); c == id {
				spec.Spawn = append(spec.Spawn, EntityState{ID: nid, X: p.X, Y: p.Y, HP: 100})
			}
		}
		procs = append(procs, startCellProcess(t, spec))
	}
	var mu sync.Mutex
	var views []View
	var results []AttackResult
	b, err := NewBase(BaseConfig{World: "mp", SID: worldBaseSID, Grid: grid, Transport: NATSTransport(dialNATS(t, url)),
		OnView:         func(v View) { mu.Lock(); views = append(views, v); mu.Unlock() },
		OnAttackResult: func(r AttackResult) { mu.Lock(); results = append(results, r); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	baseDone := make(chan struct{})
	go func() { defer close(baseDone); _ = b.Run(ctx) }()
	defer func() { cancel(); <-baseDone }()
	<-b.Ready()
	w := &world{t: t, grid: grid, base: b}
	w.enter(1, spatial.Point{X: 8800, Y: 5000})
	mu.Lock()
	views = nil
	mu.Unlock()
	_ = b.Move(1, 1000, 0)
	w.waitView(1, 10*time.Second, "east of boundary", func(v View) bool { return v.Self.X >= 11000 })
	_ = b.Move(1, -1000, 0)
	w.waitView(1, 10*time.Second, "back west", func(v View) bool { return v.Self.X <= 9600 })
	_ = b.Move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	mu.Lock()
	crossing := slices.Clone(views)
	mu.Unlock()
	stationary := boundaryNPCs()
	stationary[7] = npcs[7]
	miss, gap := checkContinuity(t, crossing, stationary, natsMaxMiss, natsMaxGap)
	sid, epoch, _ := b.Owner(1)
	if sid != grid.SID(CellID{0, 0}) || epoch != 3 {
		t.Fatalf("owner after there-and-back = sid %d epoch %d, want A epoch 3", sid, epoch)
	}
	var hits []HitID
	for range 10 {
		hit, err := b.Attack(1, 7, 3)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, hit)
		time.Sleep(60 * time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(results)
		mu.Unlock()
		if n >= len(hits) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var all []childEvent
	pids := map[int]bool{}
	for _, p := range procs {
		evs := p.stop(t)
		for _, e := range evs {
			pids[e.PID] = true
			if e.Ev == "stats" && e.Stats.Cell.Conflicts != 0 {
				t.Errorf("pid %d saw %d conflicts", e.PID, e.Stats.Cell.Conflicts)
			}
		}
		all = append(all, evs...)
	}
	if len(pids) != 2 || pids[os.Getpid()] {
		t.Fatalf("cell pids = %v (parent %d): want two separate processes", pids, os.Getpid())
	}
	last := checkCrossProcessReals(t, all)
	if last[1] != 3 {
		t.Fatalf("player last acquired epoch = %d, want 3", last[1])
	}
	applied := map[HitID]int{}
	for _, e := range all {
		if e.Ev == "hit" {
			applied[e.Hit]++
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) != len(hits) {
		t.Fatalf("results %d of %d", len(results), len(hits))
	}
	for _, r := range results {
		if r.Outcome != OutcomeApplied || applied[r.Hit] != 1 {
			t.Errorf("hit %v: outcome %v, applied %d times across processes", r.Hit, r.Outcome, applied[r.Hit])
		}
	}
	if final := results[len(results)-1].HP; final != 100-3*int32(len(hits)) {
		t.Errorf("final hp %d", final)
	}
	t.Logf("cell pids %v, parent %d; views %d, worst miss %d, worst gap %v; %d hits applied once each",
		pids, os.Getpid(), len(crossing), miss, gap, len(applied))
}
