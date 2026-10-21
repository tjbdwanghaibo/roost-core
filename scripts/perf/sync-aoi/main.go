// Command sync-aoi measures a shared world through Interest, never Group/Room.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type config struct {
	TraceCapacity      int    `json:"trace_capacity"`
	Nest               bool   `json:"nest_dispatch"`
	Mode               string `json:"sync_mode"`
	SnapshotObjects    int    `json:"snapshot_objects_per_flush"`
	SnapshotBytes      int    `json:"snapshot_bytes_per_flush"`
	SnapshotPerSession int    `json:"snapshot_objects_per_session"`
	ReconnectTick      int    `json:"reconnect_tick"`
	ReconnectPlayers   int    `json:"reconnect_players"`
	Async              bool   `json:"async_transport"`
	Profile            bool   `json:"profile"`
	RuntimeTrace       bool   `json:"runtime_trace"`
	Players            int    `json:"players"`
	Entities           int    `json:"entities"`
	Ticks              int    `json:"ticks"`
	Hz                 int    `json:"hz"`
	Dirty              int    `json:"dirty_percent"`
	Padding            int    `json:"padding_bytes"`
	Batches            int    `json:"event_batches_per_tick"`
	Visible            int    `json:"visible_target"`
	ViewsPerPlayer     int    `json:"extra_views_per_player"`
	AllianceSize       int    `json:"alliance_size"`
	World              int64  `json:"world_size"`
	Block              int64  `json:"block_size"`
	EnterRadius        int64  `json:"enter_radius"`
	LeaveRadius        int64  `json:"leave_radius"`
	MoveAmplitude      int64  `json:"move_amplitude"`
	Role               string `json:"-"`
	Address            string `json:"-"`
	Output             string `json:"-"`
}

func parseConfig() config {
	var c config
	flag.IntVar(&c.TraceCapacity, "trace-capacity", 0, "bounded diagnostic events; 0 disabled (diagnostic runs affect timing)")
	flag.BoolVar(&c.Nest, "nest", true, "use formal Nest dispatch and sync commit boundary")
	flag.StringVar(&c.Mode, "mode", "periodic", "periodic or on_change")
	flag.IntVar(&c.SnapshotObjects, "snapshot-objects", 0, "cold object creation budget per flush (per interval in on_change mode); 0 unlimited")
	flag.IntVar(&c.SnapshotBytes, "snapshot-bytes", 0, "cold object creation byte soft budget; 0 unlimited")
	flag.IntVar(&c.SnapshotPerSession, "snapshot-per-session", 0, "cold object creation budget per session per flush (per interval in on_change mode); 0 unlimited")
	flag.IntVar(&c.ReconnectTick, "reconnect-tick", 0, "reset client baselines at this measured tick; 0 disabled")
	flag.IntVar(&c.ReconnectPlayers, "reconnect-players", 0, "number of sessions reset; 0 means all")
	flag.BoolVar(&c.Async, "async", false, "use production reliable queue before loopback TCP")
	flag.BoolVar(&c.Profile, "profile", false, "capture server-only CPU, allocation, mutex and block profiles; do not compare profiled latency")
	flag.BoolVar(&c.RuntimeTrace, "runtime-trace", false, "capture server Go execution trace in a separate diagnostic run")
	flag.IntVar(&c.Players, "players", 1000, "observers, also subjects")
	flag.IntVar(&c.Entities, "entities", 10000, "total subjects including players")
	flag.IntVar(&c.Ticks, "ticks", 200, "measured ticks, warmup excluded")
	flag.IntVar(&c.Hz, "hz", 20, "replication frequency")
	flag.IntVar(&c.Dirty, "dirty", 1, "percent of entities changed per tick")
	flag.IntVar(&c.Padding, "padding", 64, "additional representative component bytes")
	flag.IntVar(&c.Batches, "batches", 10, "spread state changes across the interval")
	flag.IntVar(&c.Visible, "visible", 50, "spatial cap plus self")
	flag.IntVar(&c.ViewsPerPlayer, "views-per-player", 0, "extra spatial views per player, each following another entity (D19 multi-view); 0 keeps the single main view")
	flag.IntVar(&c.AllianceSize, "alliance-size", 0, "group players into shared relation views of this many members, attached to every member (D19 shared view fan-out); 0 disabled")
	flag.Int64Var(&c.World, "world", 0, "square map side; 0 keeps the dense grid (spacing 30) sized from -entities, >0 spreads entities pseudo-uniformly over world×world")
	flag.Int64Var(&c.Block, "block", 150, "AOI block size")
	flag.Int64Var(&c.EnterRadius, "enter-radius", 150, "AOI enter radius (default view radius)")
	flag.Int64Var(&c.LeaveRadius, "leave-radius", 180, "AOI leave radius")
	flag.Int64Var(&c.MoveAmplitude, "move-amplitude", 48, "X span of the per-entity back-and-forth movement (16 phases; 48 = the original 3 per phase)")
	flag.StringVar(&c.Role, "role", "server", "server or client")
	flag.StringVar(&c.Address, "address", "", "client connects to this loopback endpoint")
	flag.StringVar(&c.Output, "output", "", "new result directory")
	flag.Parse()
	return c
}
func (c config) validate() error {
	mode, err := entitysync.ParseSyncMode(c.Mode)
	if err != nil {
		return err
	}
	if !c.Nest && mode == entitysync.ModeOnChange {
		return errors.New("on_change requires formal Nest producer")
	}
	if c.Players < 1 || c.Entities < c.Players || c.Entities > 100000 || c.Ticks < 1 || c.Ticks > 10000 || c.Hz < 1 || c.Hz > 1000 || c.Dirty < 1 || c.Dirty > 100 || c.Padding < 0 || c.Padding > 4096 || c.Batches < 1 || c.Batches > 100 || c.Visible < 2 || c.Visible > 100 || c.ViewsPerPlayer < 0 || c.ViewsPerPlayer > 1000 || c.AllianceSize < 0 || c.AllianceSize > c.Players || c.Output == "" {
		return errors.New("invalid workload: check counts, hz, dirty, padding, visible and output")
	}
	if c.World < 0 || (c.World > 0 && c.World < 4*c.MoveAmplitude+100) || c.Block < 1 || c.EnterRadius < 1 || c.LeaveRadius < c.EnterRadius || c.MoveAmplitude < 0 || c.MoveAmplitude > 1000 {
		return errors.New("invalid geometry: check world, block, radii and move amplitude")
	}
	if c.TraceCapacity < 0 || c.TraceCapacity > 1<<20 {
		return errors.New("invalid trace capacity")
	}
	if c.SnapshotObjects < 0 || c.SnapshotBytes < 0 || c.SnapshotPerSession < 0 || c.ReconnectTick < 0 || c.ReconnectTick > c.Ticks || c.ReconnectPlayers < 0 || c.ReconnectPlayers > c.Players {
		return errors.New("invalid recovery workload")
	}
	if c.Role != "server" && c.Role != "client" {
		return errors.New("role must be server or client")
	}
	return nil
}
func main() {
	c := parseConfig()
	if err := c.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var err error
	if c.Role == "client" {
		err = runClient(c)
	} else {
		err = runServer(c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// 测试组件明确记录输入计划与实际状态变更两个时刻，积压不会被延迟统计隐藏。
// 正式路径经过 Nest 内存提交与同步边界；不把内存提交测试视为 WAL 性能。
type component struct {
	ID        int64  `bson:"id"`
	Step      int64  `bson:"step"`
	X         int64  `bson:"x"`
	Y         int64  `bson:"y"`
	HP        int64  `bson:"hp"`
	Planned   int64  `bson:"planned_ns"`
	Changed   int64  `bson:"changed_ns"`
	Committed int64  `bson:"committed_ns"`
	Extra     []byte `bson:"extra"`
}
type subject struct {
	*entity.EntityBase
	tracker dataengine.Tracker
	data    component
	state   *entity.SubjectSyncState
}

func (s *subject) pack() (entity.FrozenSyncPayload, error) {
	raw, err := bson.Marshal(s.data)
	return entity.TakeFrozenSyncPayload(1, raw), err
}
func (s *subject) PackSubjectSnapshot(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
	return s.pack()
}
func (s *subject) PackSubjectDelta(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
	return s.pack()
}

// position 是实体在某一步的位置（服务端与客户端各自计算、逐项核对）：基点加 X 方向的往返偏移，偏移按 32 步一周期
// 在 0..16 档之间来回，第 phase 档为 phase × MoveAmplitude / 16（缺省 48，即原来的每档 3）。
// 基点：-world 为 0 时是间距 30 的方阵（原负载）；否则按实体序号做 splitmix64 散列，在 world × world 内近似均匀分布，
// 两维都留出 MoveAmplitude 与 1 的边距。
func position(id int64, step int, c config) spatial.Point {
	id = workloadIndex(id, c)
	phase := (step + int(id%31)) % 32
	if phase < 0 {
		phase += 32
	}
	if phase > 16 {
		phase = 32 - phase
	}
	offset := int64(phase) * c.MoveAmplitude / 16
	if c.World > 0 {
		span := uint64(c.World - c.MoveAmplitude - 2)
		h := splitmix64(uint64(id))
		return spatial.Point{X: 1 + int64(h%span) + offset, Y: 1 + int64((h>>32)%span)}
	}
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	return spatial.Point{X: 200 + (id-1)%side*30 + offset, Y: 200 + (id-1)/side*30}
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// worldBounds：原负载按方阵边长加 500 的边距；-world 时就是 world × world。
func worldBounds(c config) spatial.Rect {
	if c.World > 0 {
		return spatial.Rect{Max: spatial.Point{X: c.World, Y: c.World}}
	}
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	return spatial.Rect{Max: spatial.Point{X: side*30 + 500, Y: side*30 + 500}}
}
func observerIDs(c config) []int64 {
	ids := make([]int64, c.Players)
	for i := range ids {
		ids[i] = workloadID(1+int64(i)*int64(c.Entities)/int64(c.Players), c)
	}
	return ids
}
func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}
func quantiles(values []float64) map[string]float64 {
	if len(values) == 0 {
		return nil
	}
	slices.Sort(values)
	out := map[string]float64{"min": values[0], "max": values[len(values)-1]}
	var sum float64
	for _, v := range values {
		sum += v
	}
	out["mean"] = sum / float64(len(values))
	for name, q := range map[string]float64{"p50": .5, "p95": .95, "p99": .99} {
		out[name] = values[int(math.Ceil(float64(len(values))*q))-1]
	}
	return out
}
func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

type serverTransport struct {
	trace         *entitysync.SyncTrace
	metaMu        sync.RWMutex
	conns         map[int64]net.Conn
	tick          int
	due           time.Time
	frames, bytes int64
}

func (l *serverTransport) Push(ctx context.Context, sid entitysync.SessionID, data []byte) error {
	conn := l.conns[int64(sid)]
	if conn == nil {
		return fmt.Errorf("missing session %d", sid)
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	tick, due := l.metadata()
	var sendStarted time.Time
	if l.trace != nil {
		sendStarted = time.Now()
	}
	if err := writePacket(conn, packetFrame, tick, due, data); err != nil {
		return err
	}
	l.traceSent(sid, data, sendStarted)
	if tick > 0 {
		l.frames++
		l.bytes += int64(len(data))
	}
	return nil
}
func runServer(c config) error {
	if _, err := os.Stat(c.Output); err == nil {
		return fmt.Errorf("output already exists: %s", c.Output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(c.Output, 0755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(c.Output, "config.json"), c); err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return err
	}
	defer listener.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(exe, "-role=client", "-address="+listener.Addr().String(), fmt.Sprintf("-players=%d", c.Players), fmt.Sprintf("-entities=%d", c.Entities), fmt.Sprintf("-padding=%d", c.Padding), "-output="+c.Output, fmt.Sprintf("-nest=%t", c.Nest), "-mode="+c.Mode, fmt.Sprintf("-trace-capacity=%d", c.TraceCapacity), fmt.Sprintf("-visible=%d", c.Visible), fmt.Sprintf("-views-per-player=%d", c.ViewsPerPlayer), fmt.Sprintf("-alliance-size=%d", c.AllianceSize), fmt.Sprintf("-world=%d", c.World), fmt.Sprintf("-block=%d", c.Block), fmt.Sprintf("-enter-radius=%d", c.EnterRadius), fmt.Sprintf("-leave-radius=%d", c.LeaveRadius), fmt.Sprintf("-move-amplitude=%d", c.MoveAmplitude))
	log, err := os.Create(filepath.Join(c.Output, "client.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	child.Stdout, child.Stderr = log, log
	if err := child.Start(); err != nil {
		return err
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	transport := &serverTransport{conns: make(map[int64]net.Conn)}
	defer func() {
		for _, conn := range transport.conns {
			_ = conn.Close()
		}
	}()
	expectedIDs := make(map[int64]bool)
	for _, id := range observerIDs(c) {
		expectedIDs[id] = true
	}
	_ = listener.SetDeadline(time.Now().Add(60 * time.Second))
	for range c.Players {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var header [8]byte
		if _, err := readFull(conn, header[:]); err != nil {
			_ = conn.Close()
			return err
		}
		id := int64(binary.LittleEndian.Uint64(header[:]))
		if !expectedIDs[id] || transport.conns[id] != nil {
			_ = conn.Close()
			return fmt.Errorf("unexpected observer %d", id)
		}
		transport.conns[id] = conn
	}
	var queued *asyncLoad
	var output entitysync.Transport = transport
	if c.Async {
		queued, err = newAsyncLoad(transport)
		if err != nil {
			return err
		}
		output = queued
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = queued.queue.Close(closeCtx)
		}()
	}
	trace, drainTrace, closeTrace, err := openSyncTrace(c)
	if err != nil {
		return err
	}
	defer closeTrace()
	transport.trace = trace
	mode, _ := entitysync.ParseSyncMode(c.Mode)
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Trace: trace, Mode: mode, Interval: time.Second / time.Duration(c.Hz), Transport: output, Limits: frameLimits(c), SnapshotBudget: entitysync.SnapshotBudget{MaxObjects: c.SnapshotObjects, MaxBytes: c.SnapshotBytes, PerSessionObjects: c.SnapshotPerSession}})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	}()
	// 模拟玩家的玩家 ID 与实体 ID 取同一个数（会话也按它打开），主视野跟随玩家自己的实体。
	interest, err := policy.NewInterest(policy.InterestConfig{Manager: manager, AOI: policy.AOIConfig{Bounds: worldBounds(c), BlockSize: c.Block, EnterRadius: c.EnterRadius, LeaveRadius: c.LeaveRadius, MaxVisible: c.Visible - 1},
		MaxViewsPerPlayer: max(policy.DefaultMaxViewsPerPlayer, c.ViewsPerPlayer+2), MaxPlayersPerView: max(policy.DefaultMaxPlayersPerView, c.AllianceSize)})
	if err != nil {
		return err
	}
	defer interest.Close()
	states := make([]*subject, c.Entities)
	for i := range states {
		id := workloadID(int64(i+1), c)
		at := position(id, 0, c)
		s := &subject{data: component{ID: id, X: at.X, Y: at.Y, HP: 100, Extra: make([]byte, c.Padding)}}
		for j := range s.data.Extra {
			s.data.Extra[j] = byte(id % 251)
		}
		s.state = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "aoi-load", Packer: s})
		if c.Nest {
			s.EntityBase = entity.NewEntityBase(id, entity.EntityCategory(4), true, loadEntityKind)
			s.SetSyncState(s.state)
		}
		states[i] = s
		if err := manager.Register(s.state); err != nil {
			return err
		}
		if expectedIDs[id] {
			if err := manager.OpenSession(entitysync.SessionID(id)); err != nil {
				return err
			}
			err = interest.Enter(id, id, at, policy.ObserverView{})
		} else {
			err = interest.Show(id, at)
		}
		if err != nil {
			return err
		}
	}
	if err := defineLoadViews(interest, states, c); err != nil {
		return err
	}
	if r := interest.Apply(); len(r) > 0 {
		return fmt.Errorf("initial subscription refusal: %v", r[0])
	}
	ctx := context.Background()
	var engine *nest.NestMgr
	split := &splitTimings{}
	if c.Nest {
		engine, err = newLoadNest(states, c, manager, interest, split)
		if err != nil {
			return err
		}
		defer engine.Shutdown(context.Background())
	}
	transport.setMetadata(0, time.Now())
	startupStart := time.Now()
	startupTicks := 0
	for {
		if err := manager.Flush(ctx); err != nil {
			return err
		}
		startupTicks++
		if err := drainTrace(); err != nil {
			return err
		}
		if manager.Stats().Pending == 0 {
			break
		}
		if startupTicks > c.Players*c.Visible*(1+c.ViewsPerPlayer)+c.AllianceSize+1 {
			return errors.New("initial snapshots did not converge")
		}
		if wait := time.Until(startupStart.Add(time.Duration(startupTicks) * time.Second / time.Duration(c.Hz))); wait > 0 {
			time.Sleep(wait)
		}
	}
	if err := queued.drain(); err != nil {
		return err
	}
	startupDuration := time.Since(startupStart)
	count := max(1, (c.Entities*c.Dirty+99)/100)
	// A coprime traversal changes entities throughout the map, not one hot room.
	stride := 7919
	for gcd(stride, c.Entities) != 1 {
		stride += 2
	}
	// begin 是本批次醒来开始发送的时刻；同一批里的状态变化由一个发生器串行发出，后面的要等前面的 Request 返回。
	// driverMove 累计直接模式下压测端线程里 interest.Move 的耗时（AOI 工作在这里），与政策阶段同口径对比。
	var driverMove time.Duration
	mutate := func(step, start, end int, planned, begin time.Time) error {
		for j := start; j < end; j++ {
			index := ((step+10)*count + j) * stride % c.Entities
			s := states[index]
			id := s.data.ID
			sent := time.Now()
			if step > 0 {
				split.sent(planned, begin, sent)
			}
			if engine != nil {
				_, err := engine.Request(ctx, loadHandler, id, []any{loadMutation{step: step, planned: planned, sent: sent}})
				if err != nil {
					return err
				}
				continue
			}
			at := position(id, step, c)
			s.data.Step = int64(step)
			s.data.X, s.data.Y = at.X, at.Y
			s.data.HP = 100 - int64((step+10000)%97)
			s.data.Planned = planned.UnixNano()
			s.data.Changed = time.Now().UnixNano()
			if step > 0 {
				split.changed(sent)
			}
			s.state.MarkDirty(1)
			moveStarted := time.Now()
			err := interest.Move(id, at)
			driverMove += time.Since(moveStarted)
			if err != nil {
				return err
			}
		}
		return nil
	}
	for step := -9; step <= 0; step++ {
		now := time.Now()
		if err := mutate(step, 0, count, now, now); err != nil {
			return err
		}
		if r := interest.Apply(); len(r) > 0 {
			return fmt.Errorf("warmup refusal: %v", r[0])
		}
		transport.setMetadata(step, time.Now())
		if err := manager.Flush(ctx); err != nil {
			return err
		}
	}
	if err := queued.drain(); err != nil {
		return err
	}
	if manager.Stats().Sessions != c.Players {
		return errors.New("session lost during warmup")
	}
	for _, id := range observerIDs(c) {
		conn := transport.conns[id]
		if err := writePacket(conn, packetBarrier, 0, 0, nil); err != nil {
			return err
		}
	}
	for _, id := range observerIDs(c) {
		conn := transport.conns[id]
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		var ack [1]byte
		if _, err := readFull(conn, ack[:]); err != nil {
			return err
		}
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	stopRuntimeTrace, err := openRuntimeTrace(c)
	if err != nil {
		return err
	}
	defer stopRuntimeTrace()
	var cpuFile *os.File
	if c.Profile {
		cpuFile, err = os.Create(filepath.Join(c.Output, "cpu.out"))
		if err != nil {
			return err
		}
		if err = pprof.StartCPUProfile(cpuFile); err != nil {
			_ = cpuFile.Close()
			return err
		}
		// 锁竞争与阻塞等待只在测量窗口内采样（诊断运行，不比较其延迟）。
		runtime.SetMutexProfileFraction(5)
		runtime.SetBlockProfileRate(int(100 * time.Microsecond))
		defer func() {
			if cpuFile != nil {
				pprof.StopCPUProfile()
				_ = cpuFile.Close()
			}
		}()
	}
	period := time.Second / time.Duration(c.Hz)
	if c.Nest {
		period = 50 * time.Millisecond
		if err := manager.Start(ctx); err != nil {
			return err
		}
	}
	start := time.Now()
	trace.Record(entitysync.SyncTraceEvent{Stage: "measurement_started", At: start.UnixNano()})
	work, flush, lag := make([]float64, 0, c.Ticks), make([]float64, 0, c.Ticks), make([]float64, 0, c.Ticks)
	// 同口径（每 tick）：policy 是 Manager 政策阶段的耗时增量（排队模式下 AOI 全在这里），driverInterest 是压测端线程里
	// interest.Move 与 interest.Apply 的耗时（直接模式下 AOI 在这里；排队模式下只剩 Apply，主要是等 Interest 的锁）。
	policyPerTick, driverInterest := make([]float64, 0, c.Ticks), make([]float64, 0, c.Ticks)
	policyBefore := manager.Counters().PolicyDuration
	driverMove = 0
	var overWork int
	var slow []map[string]any
	var recoverySetup time.Duration
	var recoveryStarted time.Time
	var recoveryCaughtUp time.Duration
	var recoverySamples []map[string]any
	for tick := 1; tick <= c.Ticks; tick++ {
		tickStart := start.Add(time.Duration(tick-1) * period)
		due := tickStart.Add(period)
		transport.setMetadata(tick, due)
		var active time.Duration
		if tick == c.ReconnectTick {
			resetStart := time.Now()
			recoveryStarted = resetStart
			trace.Record(entitysync.SyncTraceEvent{Stage: "recovery_started", At: resetStart.UnixNano()})
			ids := observerIDs(c)
			if c.ReconnectPlayers > 0 {
				ids = ids[:c.ReconnectPlayers]
			}
			for _, id := range ids {
				if err := manager.HoldSession(entitysync.SessionID(id)); err != nil {
					return err
				}
				if err := manager.ReadySession(entitysync.SessionID(id)); err != nil {
					return err
				}
			}
			recoverySetup = time.Since(resetStart)
			active += recoverySetup
		}
		for batch := 0; batch < c.Batches; batch++ {
			planned := tickStart.Add(time.Duration(batch) * period / time.Duration(c.Batches))
			if wait := time.Until(planned); wait > 0 {
				time.Sleep(wait)
			}
			begin := time.Now()
			if err := mutate(tick, batch*count/c.Batches, (batch+1)*count/c.Batches, planned, begin); err != nil {
				return err
			}
			active += time.Since(begin)
		}
		begin := time.Now()
		if r := interest.Apply(); len(r) > 0 {
			return fmt.Errorf("subscription refusal: %v", r[0])
		}
		applied := time.Since(begin)
		active += applied
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		begin = time.Now()
		if !c.Nest {
			if err := manager.Flush(ctx); err != nil {
				return err
			}
		}
		elapsed := time.Since(begin)
		active += elapsed
		if !recoveryStarted.IsZero() && recoveryCaughtUp == 0 {
			stats := manager.Stats()
			recoverySamples = append(recoverySamples, map[string]any{"elapsed_ms": milliseconds(time.Since(recoveryStarted)), "pending_snapshots": stats.PendingSnapshots, "waiting_subjects": stats.WaitingSnapshotSubjects, "oldest_wait_ms": milliseconds(stats.OldestSnapshotWait)})
			if stats.PendingSnapshots == 0 {
				recoveryCaughtUp = time.Since(recoveryStarted)
				if c.TraceCapacity > 0 {
					// 只用于诊断：暂停本驱动的下一批输入，排空后给客户端一份当前
					// Interest 集合检查点。客户端实际逐项核验，而非拿准入完成冒充到齐。
					if err := manager.Flush(ctx); err != nil {
						return err
					}
					if err := queued.drain(); err != nil {
						return err
					}
					ids := observerIDs(c)
					if c.ReconnectPlayers > 0 {
						ids = ids[:c.ReconnectPlayers]
					}
					for _, id := range ids {
						raw, err := json.Marshal(interest.Seen(id))
						if err != nil {
							return err
						}
						if err := writePacket(transport.conns[id], packetRecovery, tick, recoveryStarted.UnixNano(), raw); err != nil {
							return err
						}
					}
				}
			}
		}
		if err := drainTrace(); err != nil {
			return err
		}
		work = append(work, milliseconds(active))
		policyNow := manager.Counters().PolicyDuration
		policyPerTick = append(policyPerTick, milliseconds(policyNow-policyBefore))
		policyBefore = policyNow
		driverInterest = append(driverInterest, milliseconds(driverMove+applied))
		driverMove = 0
		flush = append(flush, milliseconds(elapsed))
		lag = append(lag, milliseconds(begin.Sub(due)))
		if active > 50*time.Millisecond {
			overWork++
		}
		if active > 50*time.Millisecond || begin.Sub(due) > 50*time.Millisecond {
			slow = append(slow, map[string]any{"tick": tick, "work_ms": milliseconds(active), "flush_ms": milliseconds(elapsed), "flush_start_lag_ms": milliseconds(begin.Sub(due))})
		}
	}
	failuresBeforeStop := manager.Counters().FlushFailures
	if c.Nest {
		if err := engine.Shutdown(ctx); err != nil {
			return err
		}
		if err := manager.Stop(ctx); err != nil {
			return err
		}
		if err := manager.Drain(ctx); err != nil {
			return err
		}
	}
	if err := queued.drain(); err != nil {
		return err
	}
	if err := drainTrace(); err != nil {
		return err
	}
	duration := time.Since(start)
	runtime.ReadMemStats(&after)
	if cpuFile != nil {
		pprof.StopCPUProfile()
		_ = cpuFile.Close()
		cpuFile = nil
		allocFile, err := os.Create(filepath.Join(c.Output, "allocs.out"))
		if err != nil {
			return err
		}
		err = pprof.Lookup("allocs").WriteTo(allocFile, 0)
		_ = allocFile.Close()
		if err != nil {
			return err
		}
		runtime.SetMutexProfileFraction(0)
		runtime.SetBlockProfileRate(0)
		for _, name := range []string{"mutex", "block"} {
			file, err := os.Create(filepath.Join(c.Output, name+".out"))
			if err != nil {
				return err
			}
			err = pprof.Lookup(name).WriteTo(file, 0)
			_ = file.Close()
			if err != nil {
				return err
			}
		}
	}
	stats := manager.Stats()
	visible := make([]float64, 0, c.Players)
	for _, id := range observerIDs(c) {
		ids := interest.Seen(id)
		visible = append(visible, float64(len(ids)))
		raw, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		if err := writePacket(transport.conns[id], packetFinish, 0, 0, raw); err != nil {
			return err
		}
	}
	// The child decodes and checks final visible sets before it writes client.json.
	wait := make(chan error, 1)
	go func() { wait <- child.Wait() }()
	select {
	case err = <-wait:
	case <-time.After(30 * time.Second):
		_ = child.Process.Kill()
		err = <-wait
		if err == nil {
			err = errors.New("client drain timed out")
		}
	}
	report := map[string]any{"recovery_admission_caught_up_ms": milliseconds(recoveryCaughtUp), "recovery_samples": recoverySamples, "startup_ticks": startupTicks, "startup_ms": milliseconds(startupDuration), "recovery_setup_ms": milliseconds(recoverySetup), "config": c, "server_gomaxprocs": runtime.GOMAXPROCS(0), "elapsed_seconds": duration.Seconds(), "server_active_work_ms": quantiles(work), "flush_ms": quantiles(flush), "flush_start_lag_ms": quantiles(lag), "server_work_over_50ms_ticks": overWork, "slow_ticks": slow, "visible_final": quantiles(visible), "manager_stats": stats, "manager_counters": manager.Counters(), "frames": transport.frames, "sync_bytes": transport.bytes, "server_alloc_bytes": after.TotalAlloc - before.TotalAlloc, "server_gc_cycles": after.NumGC - before.NumGC, "server_gc_pause_ms": float64(after.PauseTotalNs-before.PauseTotalNs) / 1e6, "server_heap_alloc_end": after.HeapAlloc, "go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH}
	report["flush_failures_before_stop"] = failuresBeforeStop
	report["same_basis"] = map[string]any{"policy_phase_per_tick_ms": quantiles(policyPerTick), "driver_interest_per_tick_ms": quantiles(driverInterest)}
	report["harness"], report["server"] = split.report()
	if last := manager.LastError(); last != nil {
		report["manager_last_error"] = last.Error()
	}
	if queued != nil {
		report["async_stats"] = queued.queue.Stats()
		report["async_counters"] = queued.queue.Counters()
	}
	raw, _ := bson.Marshal(states[0].data)
	report["example_component_bytes"] = len(raw)
	if err != nil {
		report["client_error"] = err.Error()
	}
	if writeErr := writeJSON(filepath.Join(c.Output, "server.json"), report); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return fmt.Errorf("client failed: %w; inspect client.log", err)
	}
	var received clientReport
	raw, err = os.ReadFile(filepath.Join(c.Output, "client.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &received); err != nil {
		return err
	}
	if received.Frames != transport.frames || received.Bytes != transport.bytes || stats.SessionsLost != 0 || stats.Sessions != c.Players {
		return fmt.Errorf("delivery mismatch or session loss: sent=%d received=%d lost=%d", transport.frames, received.Frames, stats.SessionsLost)
	}
	if received.Samples == 0 {
		return errors.New("no latency samples")
	}
	if failuresBeforeStop != 0 {
		return fmt.Errorf("%d flush failures before shutdown; reports retained", failuresBeforeStop)
	}
	fmt.Printf("players=%d entities=%d visible_mean=%.2f planned_p99=%.2fms change_p99=%.2fms above50ms=%d/%d\n", c.Players, c.Entities, report["visible_final"].(map[string]float64)["mean"], received.Planned["p99"], received.Changed["p99"], received.PlannedOver, received.Samples)
	if received.PlannedOver > 0 || received.ChangedOver > 0 {
		return errors.New("50ms latency gate failed; reports retained")
	}
	return nil
}
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (l *serverTransport) setMetadata(tick int, due time.Time) {
	l.metaMu.Lock()
	l.tick, l.due = tick, due
	l.metaMu.Unlock()
}
func (l *serverTransport) metadata() (int, int64) {
	l.metaMu.RLock()
	defer l.metaMu.RUnlock()
	return l.tick, time.Now().UnixNano()
}

// defineLoadViews 按 -views-per-player 与 -alliance-size 给模拟玩家加视野（D19）：每个额外空间视野跟随一个确定挑选的
// 其他实体（召唤物、眼位），随它的事务移动一起移动；联盟是一个共享关系视野，成员是同组玩家的实体，挂给同组每个玩家。
// 两项都为 0 时什么也不加，负载与只有主视野时相同。
func defineLoadViews(interest *policy.Interest, states []*subject, c config) error {
	players := observerIDs(c)
	next := policy.ViewID(1)
	for index, playerID := range players {
		for k := range c.ViewsPerPlayer {
			followed := states[(index*c.ViewsPerPlayer*7919+k*104729+1)%len(states)].data
			at := spatial.Point{X: followed.X, Y: followed.Y}
			if err := interest.DefineSpatialView(next, at, policy.ObserverView{}, followed.ID); err != nil {
				return err
			}
			if err := interest.AttachView(playerID, next); err != nil {
				return err
			}
			next++
		}
	}
	if c.AllianceSize == 0 {
		return nil
	}
	for start := 0; start < len(players); start += c.AllianceSize {
		group := players[start:min(start+c.AllianceSize, len(players))]
		if err := interest.DefineRelationView(next, entity.SyncProfile{}); err != nil {
			return err
		}
		if err := interest.SetViewMembers(next, group); err != nil {
			return err
		}
		for _, playerID := range group {
			if err := interest.AttachView(playerID, next); err != nil {
				return err
			}
		}
		next++
	}
	return nil
}

// frameLimits：多视野与联盟让一个会话持有的目标超过缺省的每帧 / 每会话 100 个对象，服务端与客户端按同一上限放宽。
// 不加视野时保持缺省，负载与之前相同。
func frameLimits(c config) frame.Limits {
	limits := frame.DefaultLimits()
	limits.MaxObjects = max(limits.MaxObjects, c.Visible*(1+c.ViewsPerPlayer)+c.AllianceSize)
	return limits
}
