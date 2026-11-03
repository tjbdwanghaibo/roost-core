package cellworld

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// 多进程：每个 Cell 是测试二进制自 exec 出来的独立 OS 进程（TestCellProcessChild），而且是一个普通 roost App——
// app.New(...).Singleton(opener)，singleton.enabled=true，锁在测试自起的私有 redis-server 上（opener 是下面的 lockStore：
// 与 wiring/redis 的 SingletonStore 同样转到 core 的 redis.CompareAndSet / CompareAndDelete；core 层的包不能 import wiring）：
// 同 sid 的第二个 Cell 进程拿不到锁、启动失败；失锁 fail-stop（App 的统一保证，cellworld 不自己做）。进程之间经
// 真实 nats-server 通信。子进程把 Real 取得 / 失去、伤害结算等事件按行写到 stdout（JSON，带 pid 与系统时间），父进程
// 据此跨进程检查“同一 (实体, epoch) 只有一个进程取得 Real、Real 区间不重叠”。父进程关闭子进程 stdin 即让它退出并打印
// 统计；kill -9 模拟崩溃。Base 一般跑在父进程里（同一套 testProc），Base 消失的用例把它也放进子进程。

const childEnv = "ROOST_CELLWORLD_CHILD"

// childSpec 是父进程交给子进程的全部配置（JSON 放在环境变量里）。
type childSpec struct {
	URL        string        `json:"url"`
	Redis      string        `json:"redis"`
	LockPrefix string        `json:"lock"`
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
	// Writeback / Lease 覆盖 SceneActor 的回写间隔与租约（毫秒；0 取缺省）。
	WritebackMS int `json:"wb"`
	LeaseMS     int `json:"lease"`
	// BasePlayer 非零时子进程不是 Cell，而是 Base：让这个玩家在 BaseAt 进入并保持，收到第一份 witness 输出 "witness"。
	BasePlayer int64         `json:"bp"`
	BaseAt     spatial.Point `json:"bat"`
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
	HP    int32      `json:"hp,omitempty"`
	T     int64      `json:"t"`
	Stats *childStat `json:"stats,omitempty"`
}

// childStat 是子进程退出时的汇总（从收到 reset 起算）。
type childStat struct {
	Wall float64   `json:"wall_s"`
	CPU  float64   `json:"cpu_s"`
	Node NodeStats `json:"node"`
	// 以下均为毫秒（Tick 是区域心跳 handler 的耗时，Ghost 是发出到邻格应用，Handoff 是冻结到得到确认）。
	TickP50, TickP99, TickMx float64
	GhostP50, GhostP99       float64
	HandoffP50, HandoffP99   float64
	GhostSamples, Handoffs   int
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

// TestCellProcessChild 不是独立用例：只在父进程设置 childEnv 时作为 Cell / Base 子进程运行。
func TestCellProcessChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("helper process for the multi-process tests and TestCellWorldMeasure")
	}
	os.Exit(runChild(os.Getenv(childEnv)))
}

// childOut 是子进程的事件输出。
type childOut struct {
	mu   sync.Mutex
	enc  *json.Encoder
	pid  int
	cell CellID
}

func (o *childOut) emit(e childEvent) {
	e.PID, e.Cell, e.T = o.pid, o.cell, time.Now().UnixNano()
	o.mu.Lock()
	_ = o.enc.Encode(e)
	o.mu.Unlock()
}

func runChild(raw string) int {
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
	out := &childOut{enc: json.NewEncoder(os.Stdout), pid: os.Getpid(), cell: CellID{spec.Col, spec.Row}}
	if spec.BasePlayer != 0 {
		return runBaseChild(spec, grid, out)
	}
	svc := &cellService{spec: spec, grid: grid, out: out}
	sid := grid.SID(out.cell)
	// Cell 进程是普通 roost App：单实例锁在任何 Mod Init 之前获取，同 sid 已有进程持锁时等到 startup_wait 后失败。
	dir, err := os.MkdirTemp("", "cellworld-child")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(dir)
	config := filepath.Join(dir, "config.cell.yaml")
	yaml := fmt.Sprintf("singleton:\n  enabled: true\n  key_prefix: %q\n  ttl: 3s\n  renew_interval: 500ms\n  guard: 1s\n  startup_wait: 6s\n", spec.LockPrefix)
	if err := os.WriteFile(config, []byte(yaml), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	opener := func(*viper.Viper) (app.SingletonStore, error) { return newLockStore(spec.Redis) }
	a := app.New("cellworld-cell", "test").Singleton(opener).RegisterServer("cell", svc)
	a.RootCmd().SetArgs([]string{"cell", "--config", config, "--sid", fmt.Sprint(sid)})
	if err := a.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cell app:", err)
		return 4
	}
	return 0
}

// lockStore 是单实例锁的后端：与 wiring/redis 的 SingletonStore 相同的两个 CAS 原语加逐键 GET。
type lockStore struct{ client fredis.IRedis }

func newLockStore(addr string) (app.SingletonStore, error) {
	client, err := redisdriver.NewClient(&fredis.Config{Addr: addr, MaxRetries: -1})
	if err != nil {
		return nil, err
	}
	return &lockStore{client: client}, nil
}

func (s *lockStore) CompareAndSet(ctx context.Context, key string, expected, next []byte, ttl time.Duration) (bool, []byte, error) {
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{Key: key, Expected: expected, Next: next, TTL: ttl})
	if err != nil {
		return false, nil, err
	}
	return result.Applied, result.Current, nil
}

func (s *lockStore) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	result, err := fredis.CompareAndDelete(ctx, s.client, key, expected)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

func (s *lockStore) Get(ctx context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, key := range keys {
		value, err := s.client.Get(ctx, key)
		switch {
		case errors.Is(err, fredis.ErrNil):
		case err != nil:
			return nil, err
		default:
			out[i] = value
		}
	}
	return out, nil
}

func (s *lockStore) Close() error { return s.client.Close() }

// cellService 是 Cell 进程的 App 服务：Serve 起本格（startProc 同一套接线），stdin EOF 时打印统计并返回（App 停 Mod、
// 释放单实例锁）。
type cellService struct {
	spec childSpec
	grid *Grid
	out  *childOut
}

func (s *cellService) Name() app.ServiceName          { return "cell" }
func (s *cellService) Init(*app.Registry) error       { return nil }
func (s *cellService) Shutdown(context.Context) error { return nil }

func (s *cellService) Serve(ctx context.Context) error {
	spec, id, out := s.spec, s.out.cell, s.out
	tb := &childTB{}
	defer tb.cleanup()
	ticks, ghosts, handoffs := newSampler(), newSampler(), newSampler()
	p := startProc(tb, procSpec{sid: s.grid.SID(id), cell: &id, grid: s.grid, transport: NATSTransport(dialNATS(tb, spec.URL)), mod: func(cfg *NodeConfig) {
		cfg.World, cfg.ViewRadius = "mp", spec.ViewRadius
		if spec.WritebackMS > 0 {
			cfg.WritebackEvery = time.Duration(spec.WritebackMS) * time.Millisecond
		}
		if spec.LeaseMS > 0 {
			cfg.LeaseTimeout = time.Duration(spec.LeaseMS) * time.Millisecond
		}
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
				out.emit(childEvent{Ev: "real", ID: eid, Epoch: epoch, Acq: acq})
			}
			cfg.Hooks.DamageApplied = func(_ CellID, target int64, hit HitID, hp int32) {
				out.emit(childEvent{Ev: "hit", ID: target, Hit: hit, HP: hp})
			}
		}
	}})
	defer p.stop()
	spawnCtx, spawnCancel := context.WithTimeout(ctx, 60*time.Second)
	defer spawnCancel()
	for _, st := range spec.Spawn {
		if err := p.node.Spawn(spawnCtx, st); err != nil {
			return err
		}
	}
	if spec.RandomNPCs > 0 {
		r := rand.New(rand.NewPCG(spec.Seed, uint64(spec.Col)<<32|uint64(spec.Row)))
		rect := s.grid.Rect(id)
		for i := range spec.RandomNPCs {
			angle := r.Float64() * 2 * math.Pi
			st := EntityState{ID: npcID(spec.IDBase + int64(i)), HP: 100,
				X: rect.Min.X + r.Int64N(rect.Max.X-rect.Min.X), Y: rect.Min.Y + r.Int64N(rect.Max.Y-rect.Min.Y),
				VX: int64(float64(spec.NPCSpeed) * math.Cos(angle)), VY: int64(float64(spec.NPCSpeed) * math.Sin(angle))}
			if err := p.node.Spawn(spawnCtx, st); err != nil {
				return err
			}
		}
	}
	out.emit(childEvent{Ev: "ready"})
	start, cpuStart, statStart := time.Now(), cpuSeconds(), p.node.Stats()
	lines := make(chan string)
	go func() {
		defer close(lines)
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			lines <- strings.TrimSpace(in.Text())
		}
	}()
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case line, ok := <-lines:
			if !ok {
				done = true
				break
			}
			if line == "reset" { // 开始测量窗口
				ticks.reset()
				ghosts.reset()
				handoffs.reset()
				start, cpuStart, statStart = time.Now(), cpuSeconds(), p.node.Stats()
			}
		}
	}
	end, cpuEnd, statEnd := time.Now(), cpuSeconds(), p.node.Stats()
	tp50, tp99, tmx, _ := ticks.quantiles()
	gp50, gp99, _, gn := ghosts.quantiles()
	hp50, hp99, _, hn := handoffs.quantiles()
	out.emit(childEvent{Ev: "stats", Stats: &childStat{
		Wall: end.Sub(start).Seconds(), CPU: cpuEnd - cpuStart, Node: diffStats(statEnd, statStart),
		TickP50: tp50, TickP99: tp99, TickMx: tmx, GhostP50: gp50, GhostP99: gp99, HandoffP50: hp50, HandoffP99: hp99,
		GhostSamples: gn, Handoffs: hn,
	}})
	return nil
}

// runBaseChild 是 Base 子进程：让一个玩家进入并保持，直到被杀或 stdin EOF。
func runBaseChild(spec childSpec, grid *Grid, out *childOut) int {
	tb := &childTB{}
	defer tb.cleanup()
	var once sync.Once
	p := startProc(tb, procSpec{sid: worldBaseSID, base: true, grid: grid, transport: NATSTransport(dialNATS(tb, spec.URL)), mod: func(cfg *NodeConfig) {
		cfg.World, cfg.ViewRadius = "mp", spec.ViewRadius
		cfg.Hooks.Witness = func(View) { once.Do(func() { out.emit(childEvent{Ev: "witness", ID: actorID(spec.BasePlayer)}) }) }
	}})
	defer p.stop()
	pl := p.player(spec.BasePlayer)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.node.Enter(ctx, pl.ID(), actorID(spec.BasePlayer), spec.BaseAt, 100); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	out.emit(childEvent{Ev: "ready"})
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func diffStats(a, b NodeStats) NodeStats {
	return NodeStats{
		Ticks: a.Ticks - b.Ticks, MsgsOut: a.MsgsOut - b.MsgsOut, BytesOut: a.BytesOut - b.BytesOut,
		MsgsIn: a.MsgsIn - b.MsgsIn, BytesIn: a.BytesIn - b.BytesIn, OutboxDrops: a.OutboxDrops - b.OutboxDrops,
		DispatchErrors: a.DispatchErrors - b.DispatchErrors, DecodeErrors: a.DecodeErrors - b.DecodeErrors,
		PublishErrors: a.PublishErrors - b.PublishErrors, StaleDrops: a.StaleDrops - b.StaleDrops, Conflicts: a.Conflicts - b.Conflicts,
		HandoffsStarted: a.HandoffsStarted - b.HandoffsStarted, HandoffsAccepted: a.HandoffsAccepted - b.HandoffsAccepted,
		HandoffsRejected: a.HandoffsRejected - b.HandoffsRejected, HandoffsAborted: a.HandoffsAborted - b.HandoffsAborted,
		OfferRetransmits: a.OfferRetransmits - b.OfferRetransmits, LeaseExpired: a.LeaseExpired - b.LeaseExpired,
		Reals: a.Reals, Ghosts: a.Ghosts,
	}
}

// childTB 让 startProc / dialNATS 在子进程里复用：只收集清理函数，失败直接退出。
type childTB struct {
	testing.TB
	cleanups []func()
}

func (c *childTB) Helper()           {}
func (c *childTB) Cleanup(f func())  { c.cleanups = append(c.cleanups, f) }
func (c *childTB) Fatal(args ...any) { fmt.Fprintln(os.Stderr, args...); os.Exit(2) }
func (c *childTB) Skip(args ...any)  { fmt.Fprintln(os.Stderr, args...); os.Exit(3) }
func (c *childTB) Errorf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
}
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
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	mu       sync.Mutex
	events   []childEvent
	ready    chan struct{}
	done     chan struct{}
	stderr   strings.Builder
	waitOnce sync.Once
	err      error
}

// launchChild 起子进程，不等它就绪。
func launchChild(t *testing.T, spec childSpec) *cellProcess {
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
		p.wait()
	})
	return p
}

func startCellProcess(t *testing.T, spec childSpec) *cellProcess {
	t.Helper()
	p := launchChild(t, spec)
	select {
	case <-p.ready:
	case <-p.done:
		t.Fatalf("child exited before ready: %s", p.stderrText())
	case <-time.After(60 * time.Second):
		t.Fatalf("child not ready: %s", p.stderrText())
	}
	return p
}

// wait 等子进程退出（只调一次 cmd.Wait），返回它的退出错误。
func (p *cellProcess) wait() error {
	p.waitOnce.Do(func() { p.err = p.cmd.Wait() })
	return p.err
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

func (p *cellProcess) eventsSnapshot() []childEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

func (p *cellProcess) send(line string) { _, _ = io.WriteString(p.stdin, line+"\n") }

// kill 模拟崩溃：SIGKILL，不给它打印统计或释放锁的机会。
func (p *cellProcess) kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.done
	_ = p.wait()
}

// stop 关闭 stdin，等子进程打印统计并退出。
func (p *cellProcess) stop(t *testing.T) []childEvent {
	t.Helper()
	_ = p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("child did not exit: %s", p.stderrText())
	}
	if err := p.wait(); err != nil {
		t.Fatalf("child: %v: %s", err, p.stderrText())
	}
	return p.eventsSnapshot()
}

// startRedisServer 在私有端口起一个不落盘的 redis-server 给单实例锁用，用完杀掉；PATH 里没有或 -short 时跳过。
func startRedisServer(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real redis-server suite skipped in -short")
	}
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server executable unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	_ = listener.Close()
	cmd := exec.Command(bin, "--port", fmt.Sprint(addr.Port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return addr.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis-server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkCrossProcessReals：按系统时间排序全部进程的 Real 事件，检查同一 (实体, epoch) 只有一个进程取得，任一时刻至多
// 一个进程持有，epoch 严格递增。被 kill 的进程持有的 Real 在 killedAt 之后不再算（它已不存在）。同一台机器上各进程的
// 系统时间可比；冻结的提交后回调先于发 Offer、安装晚于收到，中间隔着一次 NATS 往返，事件时间不会倒置。
func checkCrossProcessReals(t *testing.T, events []childEvent, killed map[int]int64) map[int64]uint64 {
	t.Helper()
	var reals []childEvent
	for _, e := range events {
		if e.Ev == "real" {
			reals = append(reals, e)
		}
	}
	for pid, at := range killed {
		reals = append(reals, childEvent{Ev: "kill", PID: pid, T: at})
	}
	sort.SliceStable(reals, func(i, j int) bool { return reals[i].T < reals[j].T })
	holders := map[int64]map[int]uint64{}
	owner := map[[2]uint64]int{}
	last := map[int64]uint64{}
	for _, e := range reals {
		if e.Ev == "kill" {
			for _, h := range holders {
				delete(h, e.PID)
			}
			continue
		}
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

// mpWorld 是父进程这边的 Base（同一套 testProc，NATS 传输）加上若干 Cell 子进程。
type mpWorld struct {
	*world
	url, redis, lock string
	spec             childSpec
	procs            map[CellID]*cellProcess
}

func startMPWorld(t *testing.T, cols, rows int32, mod func(*childSpec)) *mpWorld {
	url := startNATSServer(t)
	redis := startRedisServer(t)
	base := childSpec{URL: url, Redis: redis, LockPrefix: fmt.Sprintf("cwtest-%d-%d", os.Getpid(), time.Now().UnixNano()),
		Cols: cols, Rows: rows, CellSize: 10000, Band: 1000, Hysteresis: 100, ViewRadius: worldViewRadius, Events: true}
	if mod != nil {
		mod(&base)
	}
	grid, err := base.grid()
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, grid: grid, real: newRealChecker(t), cells: map[CellID]*testProc{},
		views: map[int64][]View{}, hits: map[HitID]int{}, writebacks: map[int64][]EntityState{}}
	mp := &mpWorld{world: w, url: url, redis: redis, lock: base.LockPrefix, spec: base, procs: map[CellID]*cellProcess{}}
	w.base = startProc(t, procSpec{sid: worldBaseSID, base: true, grid: grid, transport: NATSTransport(dialNATS(t, url)), mod: func(cfg *NodeConfig) {
		cfg.World = "mp"
		cfg.Hooks.Witness = func(v View) { w.mu.Lock(); w.views[v.Player] = append(w.views[v.Player], v); w.mu.Unlock() }
		cfg.Hooks.AttackResult = func(r AttackResult) { w.mu.Lock(); w.results = append(w.results, r); w.mu.Unlock() }
		cfg.Hooks.Writeback = func(id int64, s EntityState) {
			w.mu.Lock()
			w.writebacks[id] = append(w.writebacks[id], s)
			w.mu.Unlock()
		}
	}})
	t.Cleanup(w.base.stop)
	return mp
}

func (mp *mpWorld) cellSpec(id CellID, npcs map[int64]spatial.Point) childSpec {
	spec := mp.spec
	spec.Col, spec.Row = id.Col, id.Row
	for nid, p := range npcs {
		if c, _ := mp.grid.CellAt(p); c == id {
			spec.Spawn = append(spec.Spawn, EntityState{ID: nid, X: p.X, Y: p.Y, HP: 100})
		}
	}
	return spec
}

func (mp *mpWorld) startCells(npcs map[int64]spatial.Point) {
	for _, id := range mp.grid.Cells() {
		mp.procs[id] = startCellProcess(mp.t, mp.cellSpec(id, npcs))
	}
}

func (mp *mpWorld) stopAll() []childEvent {
	var all []childEvent
	for _, id := range mp.grid.Cells() {
		if p := mp.procs[id]; p != nil {
			all = append(all, p.stop(mp.t)...)
		}
	}
	return all
}

// 两个 Cell 各是一个 App 进程：玩家来回穿越边界（witness 与客户端视野连续、epoch 单调、跨进程至多一个 Real），再跨界攻击
// B 区边界带里的 NPC（每个 HitID 只在一个进程里结算一次）。
func TestCellsInSeparateProcesses(t *testing.T) {
	mp := startMPWorld(t, 2, 1, nil)
	npcs := boundaryNPCs()
	target := npcID(7)
	npcs[target] = spatial.Point{X: 10200, Y: 5200}
	mp.startCells(npcs)
	w := mp.world
	w.enter(1, spatial.Point{X: 8800, Y: 5000})
	w.resetViews(1)
	w.move(1, 1000, 0)
	w.waitView(1, 10*time.Second, "east of boundary", func(v View) bool { return v.Self.X >= 11000 })
	w.move(1, -1000, 0)
	w.waitView(1, 10*time.Second, "back west", func(v View) bool { return v.Self.X <= 9600 })
	w.move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	crossing := w.viewsOf(1)
	miss, gap := checkContinuity(t, crossing, npcs, natsMaxMiss, natsMaxGap)
	w.checkClient(1, npcs)
	if sid, epoch := w.owner(1); sid != w.grid.SID(CellID{0, 0}) || epoch != 3 {
		t.Fatalf("owner after there-and-back = sid %d epoch %d, want A epoch 3", sid, epoch)
	}
	var hits []HitID
	for range 10 {
		hits = append(hits, w.attack(1, target, 3))
		time.Sleep(60 * time.Millisecond)
	}
	results := w.waitResults(len(hits), 5*time.Second)
	all := mp.stopAll()
	pids := map[int]bool{}
	for _, e := range all {
		pids[e.PID] = true
		if e.Ev == "stats" && e.Stats.Node.Conflicts != 0 {
			t.Errorf("pid %d saw %d conflicts", e.PID, e.Stats.Node.Conflicts)
		}
	}
	if len(pids) != 2 || pids[os.Getpid()] {
		t.Fatalf("cell pids = %v (parent %d): want two separate processes", pids, os.Getpid())
	}
	if last := checkCrossProcessReals(t, all, nil); last[actorID(1)] != 3 {
		t.Fatalf("player last acquired epoch = %d, want 3", last[actorID(1)])
	}
	applied := map[HitID]int{}
	for _, e := range all {
		if e.Ev == "hit" {
			applied[e.Hit]++
		}
	}
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

// Cell 进程 kill -9：玩家在 B 挨打并回写（HP 90），B 进程被 SIGKILL（不释放单实例锁、内存全丢）；新起的 B' 进程等旧锁过期
// （TTL 3s）后拿到锁，Base 的看门狗重进，B' 按 Player 快照建 SceneActor：新进程、epoch 更大、HP 90。
func TestCellProcessKill9PlayerRestoredFromSnapshot(t *testing.T) {
	mp := startMPWorld(t, 2, 1, func(s *childSpec) { s.WritebackMS = 200 })
	mp.startCells(nil)
	w := mp.world
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 12000, Y: 5000})
	w.enter(2, spatial.Point{X: 12200, Y: 5000})
	time.Sleep(3 * worldPeriod)
	for range 5 {
		w.attack(2, actorID(1), 2)
	}
	w.waitResults(5, 3*time.Second)
	waitFor(t, 3*time.Second, "writeback of hp 90", func() bool { return w.baseState(1).HP == 90 })
	_, before := w.owner(1)
	old := mp.procs[b]
	oldPID := old.cmd.Process.Pid
	killedAt := time.Now().UnixNano()
	old.kill()
	restarted := startCellProcess(t, mp.cellSpec(b, nil))
	mp.procs[b] = restarted
	v := w.waitView(1, 15*time.Second, "view after restart", func(v View) bool { return v.Epoch > before })
	if v.Cell != w.grid.SID(b) || v.Self.X != 12000 || v.Self.HP != 90 {
		t.Fatalf("after restart: view from %d at (%d,%d) hp %d, want B at (12000,5000) hp 90", v.Cell, v.Self.X, v.Self.Y, v.Self.HP)
	}
	newPID := restarted.cmd.Process.Pid
	all := append(old.eventsSnapshot(), mp.stopAll()...)
	checkCrossProcessReals(t, all, map[int]int64{oldPID: killedAt})
	if newPID == oldPID {
		t.Fatalf("restarted cell has the old pid %d", oldPID)
	}
	t.Logf("killed pid %d, restarted pid %d; epoch %d -> %d, hp restored %d", oldPID, newPID, before, v.Epoch, v.Self.HP)
}

// 单实例锁：同一个 sid 的第二个 Cell 进程在 startup_wait（6s）内拿不到锁，以 ErrSingletonHeld 启动失败；第一个进程照常服务。
func TestSecondCellProcessForSameSIDIsRefused(t *testing.T) {
	mp := startMPWorld(t, 2, 1, nil)
	mp.startCells(nil)
	w := mp.world
	a := CellID{0, 0}
	w.enter(1, spatial.Point{X: 5000, Y: 5000})
	second := launchChild(t, mp.cellSpec(a, nil))
	select {
	case <-second.ready:
		t.Fatal("a second process for the same sid became ready")
	case <-second.done:
	case <-time.After(20 * time.Second):
		t.Fatal("second process neither failed nor became ready")
	}
	err := second.wait()
	if err == nil || !strings.Contains(second.stderrText(), app.ErrSingletonHeld.Error()) {
		t.Fatalf("second process: err %v, stderr %q; want a refusal naming %q", err, second.stderrText(), app.ErrSingletonHeld)
	}
	w.resetViews(1)
	w.waitView(1, 3*time.Second, "first process still serving", func(v View) bool { return v.Cell == w.grid.SID(a) })
	mp.stopAll()
}

// Base 进程消失：Base 是子进程，玩家在 A；Base 被 SIGKILL 后 SceneActor 收不到续租，LeaseTimeout（1s）内离场。
func TestBaseProcessGoneSceneActorCleanedUp(t *testing.T) {
	mp := startMPWorld(t, 2, 1, func(s *childSpec) { s.LeaseMS = 1000 })
	mp.startCells(nil)
	spec := mp.spec
	spec.BasePlayer, spec.BaseAt = 5, spatial.Point{X: 5000, Y: 5000}
	base := startCellProcess(t, spec)
	a := mp.procs[CellID{0, 0}]
	waitFor(t, 5*time.Second, "witness at the base child", func() bool {
		return slices.ContainsFunc(base.eventsSnapshot(), func(e childEvent) bool { return e.Ev == "witness" })
	})
	time.Sleep(1500 * time.Millisecond) // 超过租约：Base 在续租，SceneActor 必须还在
	released := func() bool {
		return slices.ContainsFunc(a.eventsSnapshot(), func(e childEvent) bool { return e.Ev == "real" && e.ID == actorID(5) && !e.Acq })
	}
	if released() {
		t.Fatal("SceneActor left although its base renews the lease")
	}
	killedAt := time.Now()
	base.kill()
	waitFor(t, 4*time.Second, "SceneActor released after the base died", released)
	var at int64
	for _, e := range a.eventsSnapshot() {
		if e.Ev == "real" && e.ID == actorID(5) && !e.Acq {
			at = e.T
		}
	}
	if gone := time.Unix(0, at).Sub(killedAt); gone > 2500*time.Millisecond {
		t.Errorf("SceneActor left %v after the base died, want within the 1s lease plus a beat", gone)
	} else {
		t.Logf("SceneActor left %v after the base process was killed", gone)
	}
	var leases uint64
	for _, e := range mp.stopAll() {
		if e.Ev == "stats" {
			leases += e.Stats.Node.LeaseExpired
		}
	}
	if leases != 1 {
		t.Fatalf("lease expirations %d, want 1", leases)
	}
}
