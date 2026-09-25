// Command sync-aoi measures a shared world through Interest, never Group/Room.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/nest"
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

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy"
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
	Players            int    `json:"players"`
	Entities           int    `json:"entities"`
	Ticks              int    `json:"ticks"`
	Hz                 int    `json:"hz"`
	Dirty              int    `json:"dirty_percent"`
	Padding            int    `json:"padding_bytes"`
	Batches            int    `json:"event_batches_per_tick"`
	Visible            int    `json:"visible_target"`
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
	flag.BoolVar(&c.Profile, "profile", false, "capture server-only CPU and allocation profiles; do not compare profiled latency")
	flag.IntVar(&c.Players, "players", 1000, "observers, also subjects")
	flag.IntVar(&c.Entities, "entities", 10000, "total subjects including players")
	flag.IntVar(&c.Ticks, "ticks", 200, "measured ticks, warmup excluded")
	flag.IntVar(&c.Hz, "hz", 20, "replication frequency")
	flag.IntVar(&c.Dirty, "dirty", 1, "percent of entities changed per tick")
	flag.IntVar(&c.Padding, "padding", 64, "additional representative component bytes")
	flag.IntVar(&c.Batches, "batches", 10, "spread state changes across the interval")
	flag.IntVar(&c.Visible, "visible", 50, "spatial cap plus self")
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
	if c.Players < 1 || c.Entities < c.Players || c.Entities > 100000 || c.Ticks < 1 || c.Ticks > 10000 || c.Hz < 1 || c.Hz > 1000 || c.Dirty < 1 || c.Dirty > 100 || c.Padding < 0 || c.Padding > 4096 || c.Batches < 1 || c.Batches > 100 || c.Visible < 2 || c.Visible > 100 || c.Output == "" {
		return errors.New("invalid workload: check counts, hz, dirty, padding, visible and output")
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
func position(id int64, step int, c config) spatial.Point {
	id = workloadIndex(id, c)
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	phase := (step + int(id%31)) % 32
	if phase < 0 {
		phase += 32
	}
	if phase > 16 {
		phase = 32 - phase
	}
	return spatial.Point{X: 200 + (id-1)%side*30 + int64(phase)*3, Y: 200 + (id-1)/side*30}
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
	child := exec.Command(exe, "-role=client", "-address="+listener.Addr().String(), fmt.Sprintf("-players=%d", c.Players), fmt.Sprintf("-entities=%d", c.Entities), fmt.Sprintf("-padding=%d", c.Padding), "-output="+c.Output, fmt.Sprintf("-nest=%t", c.Nest), "-mode="+c.Mode, fmt.Sprintf("-trace-capacity=%d", c.TraceCapacity))
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
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Trace: trace, Mode: mode, Interval: time.Second / time.Duration(c.Hz), Transport: output, SnapshotBudget: entitysync.SnapshotBudget{MaxObjects: c.SnapshotObjects, MaxBytes: c.SnapshotBytes, PerSessionObjects: c.SnapshotPerSession}})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	}()
	side := int64(math.Ceil(math.Sqrt(float64(c.Entities))))
	interest, err := policy.NewInterest(policy.InterestConfig{Manager: manager, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: side*30 + 500, Y: side*30 + 500}}, BlockSize: 150, EnterRadius: 150, LeaveRadius: 180, MaxVisible: c.Visible - 1}})
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
			err = interest.Enter(id, at)
		} else {
			err = interest.Show(id, at)
		}
		if err != nil {
			return err
		}
	}
	if r := interest.Apply(); len(r) > 0 {
		return fmt.Errorf("initial subscription refusal: %v", r[0])
	}
	ctx := context.Background()
	var engine *nest.NestMgr
	if c.Nest {
		engine, err = newLoadNest(states, c, manager, interest)
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
		if startupTicks > c.Players*c.Visible+1 {
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
	mutate := func(step, start, end int, planned time.Time) error {
		for j := start; j < end; j++ {
			index := ((step+10)*count + j) * stride % c.Entities
			s := states[index]
			id := s.data.ID
			if engine != nil {
				_, err := engine.Request(ctx, loadHandler, id, []any{loadMutation{step: step, planned: planned, observer: expectedIDs[id]}})
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
			s.state.MarkDirty(1)
			if expectedIDs[id] {
				err = interest.Move(id, at)
			} else {
				err = interest.MoveShown(id, at)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	for step := -9; step <= 0; step++ {
		if err := mutate(step, 0, count, time.Now()); err != nil {
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
			if err := mutate(tick, batch*count/c.Batches, (batch+1)*count/c.Batches, planned); err != nil {
				return err
			}
			active += time.Since(begin)
		}
		begin := time.Now()
		if r := interest.Apply(); len(r) > 0 {
			return fmt.Errorf("subscription refusal: %v", r[0])
		}
		active += time.Since(begin)
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
						raw, err := json.Marshal(append(interest.Visible(id), id))
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
	}
	stats := manager.Stats()
	visible := make([]float64, 0, c.Players)
	for _, id := range observerIDs(c) {
		ids := append(interest.Visible(id), id)
		slices.Sort(ids)
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
