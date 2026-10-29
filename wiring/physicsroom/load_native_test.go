//go:build physx && cgo

package physicsroom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
)

// 显式启用的有界多房间测量：真实 Room/PhysX/Nest，逻辑客户端直接 Poll。
// 不含 Gate/NATS/TCP，不把整个测试进程的分配或运行时间归为服务器独占成本。
func TestMultiRoomReplicationLoad(t *testing.T) {
	if os.Getenv("ROOST_PHYSICS_LOAD") != "1" {
		t.Skip("set ROOST_PHYSICS_LOAD=1 for bounded multi-room measurement")
	}
	for _, mechanisms := range []bool{false, true} {
		for _, count := range []int{1, 4, 8} {
			name := "crowd"
			if mechanisms {
				name = "mechanisms"
			}
			t.Run(fmt.Sprintf("%s/%d", name, count), func(t *testing.T) { runRoomLoad(t, name, count, mechanisms) })
		}
	}
}

type loadSample struct {
	tick         uint64
	step, commit time.Duration
	at           time.Time
}

// loadWindow 是一个采集窗口内的原始记录；take 整体换出，长时间运行时内存只与窗口长度有关。
type loadWindow struct {
	samples                map[domain.Ref][]loadSample
	polls                  []time.Duration
	payload, frames, empty uint64
}

// roomLoad 是多房间测量的共用夹具：真实 Room/PhysX/Nest，每房 20 个逻辑客户端
// 按 Relay 节奏（响应后等 50ms）独立提交输入并 Poll。只在 recording 打开时记录。
type roomLoad struct {
	rooms     *domain.Service
	maxFrame  int
	clients   int
	nonstatic int
	failures  chan error
	stop      func()

	mu        sync.Mutex
	recording bool
	window    loadWindow
}

// startRoomLoad 建房、入房并启动逻辑客户端；关闭顺序（客户端、Service、EntityHost）登记在 t.Cleanup。
func startRoomLoad(t *testing.T, scene string, roomCount int, mechanisms bool, lifetimeSeconds int64) *roomLoad {
	t.Helper()
	if err := entity.RegisterEntityKindCategory(240, 1); err != nil {
		t.Fatal(err)
	}
	host, err := NewEntityHost(240, 1, roomCount, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 256}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(context.Background()) })
	l := &roomLoad{window: loadWindow{samples: make(map[domain.Ref][]loadSample)}}
	cfg := domain.DefaultConfig(func(ctx context.Context, ref domain.Ref, f gamephysics.Frame) error {
		start := time.Now()
		err := host.Commit(ctx, ref, f)
		if err == nil {
			l.mu.Lock()
			if l.recording {
				l.window.samples[ref] = append(l.window.samples[ref], loadSample{f.Tick, f.StepTime, time.Since(start), time.Now()})
			}
			l.mu.Unlock()
		}
		return err
	})
	cfg.MaxRooms = roomCount
	cfg.Closed = host.Forget
	l.maxFrame = cfg.MaxFrameBytes
	rooms, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l.rooms = rooms
	t.Cleanup(func() { rooms.Shutdown(context.Background()) })
	ctx := context.Background()
	instance, err := rooms.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type observer struct {
		member domain.Member
		ack    uint64
	}
	observers := make([]observer, 0, roomCount*20)
	l.nonstatic = gamephysics.CrowdBodies
	if mechanisms {
		l.nonstatic += 12
	}
	for roomID := 0; roomID < roomCount; roomID++ {
		spec := domain.Spec{Instance: instance, ID: fmt.Sprintf("load-%s-%d", scene, roomID), Bodies: gamephysics.CrowdScene(), LifetimeSeconds: lifetimeSeconds}
		if mechanisms {
			// 8 个转杆与 4 个铰链马达，位于 NPC 所在的内环；不增加网络玩家数。
			for i := 0; i < 8; i++ {
				a := float64(i) * 2 * math.Pi / 8
				id := uint64(20000 + i)
				spec.Bodies = append(spec.Bodies, physx.Body{ID: id, Shape: physx.Box, Motion: physx.Kinematic, Position: physx.Vec3{float32(9 * math.Cos(a)), 2, float32(9 * math.Sin(a))}, Dimensions: physx.Vec3{2, .2, .2}, Mass: 10})
				spec.Motions = append(spec.Motions, gamephysics.MotionTrack{ID: id, PeriodSeconds: 4, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1})
			}
			for i := 0; i < 4; i++ {
				id := uint64(21000 + i)
				p := physx.Vec3{float32(-6 + i*4), 4, 0}
				spec.Bodies = append(spec.Bodies, physx.Body{ID: id, Shape: physx.Box, Motion: physx.Dynamic, Position: p, Dimensions: physx.Vec3{.25, 1.5, .25}, Mass: 10})
				spec.Joints = append(spec.Joints, physx.Joint{ID: uint64(i + 1), Kind: physx.Hinge, A: id, AnchorA: physx.Vec3{0, 1.5, 0}, AnchorB: physx.Vec3{p[0], p[1] + 1.5, p[2]}, MotorSpeed: 1, MotorForce: 100, BreakForce: 10000})
			}
		}
		ref, err := rooms.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 20; i++ {
			binding := gateway.Binding{PlayerID: int64(i), SessionID: fmt.Sprintf("load-%d-%d", roomID, i), ConnectionNonce: loadToken(), BindID: loadToken(), Gate: gateway.ProcessIdentity{ServerID: 1, Incarnation: "0123456789abcdef0123456789abcdef"}, Game: gateway.ProcessIdentity{ServerID: 2, Incarnation: "1123456789abcdef0123456789abcdef"}}
			m, err := rooms.Join(ctx, ref, binding, uint64(50+i))
			if err != nil {
				t.Fatal(err)
			}
			observers = append(observers, observer{member: m})
		}
	}
	l.clients = len(observers)
	// 每个逻辑客户端独立调度，和 Relay 一样从响应后等 50ms；不使用串行全体轮询。
	clientCtx, stopClients := context.WithCancel(ctx)
	var clients sync.WaitGroup
	l.failures = make(chan error, len(observers))
	for i := range observers {
		clients.Add(1)
		go func(i int) {
			defer clients.Done()
			o := &observers[i]
			timer := time.NewTimer(0)
			defer timer.Stop()
			var sequence uint64
			for {
				select {
				case <-clientCtx.Done():
					return
				case <-timer.C:
				}
				sequence++
				a := float64(i%20)*2*math.Pi/20 + float64(sequence)/30
				if err := rooms.Submit(clientCtx, o.member, domain.Input{Sequence: sequence, Acceleration: physx.Vec3{float32(8 * math.Cos(a)), 0, float32(8 * math.Sin(a))}}); err != nil {
					if clientCtx.Err() == nil {
						l.failures <- err
					}
					return
				}
				began := time.Now()
				d, err := rooms.Poll(clientCtx, o.member, o.ack)
				duration := time.Since(began)
				timer.Reset(50 * time.Millisecond)
				if err != nil {
					if clientCtx.Err() == nil {
						l.failures <- err
					}
					return
				}
				l.mu.Lock()
				if l.recording {
					l.window.polls = append(l.window.polls, duration)
					l.window.payload += uint64(len(d.Payload))
					if len(d.Payload) > 0 {
						l.window.frames++
					} else {
						l.window.empty++
					}
				}
				l.mu.Unlock()
				if len(d.Payload) > 0 {
					if len(d.Payload) > l.maxFrame {
						l.failures <- errors.New("frame exceeded budget")
						return
					}
					if o.ack == 0 {
						var replica gamephysics.RoomReplica
						if err := replica.Apply(d.Payload); err != nil {
							l.failures <- err
							return
						}
						if _, ok := replica.States[uint64(51+i%20)]; !ok {
							l.failures <- errors.New("self missing")
							return
						}
					}
					o.ack = d.Sequence
				}
			}
		}(i)
	}
	l.stop = func() { stopClients(); clients.Wait() }
	t.Cleanup(l.stop)
	return l
}

// record 打开或关闭记录，返回切换时刻。
func (l *roomLoad) record(on bool) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recording = on
	return time.Now()
}

// take 换出当前窗口并返回换出时刻；记录状态不变。
func (l *roomLoad) take() (loadWindow, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.window
	l.window = loadWindow{samples: make(map[domain.Ref][]loadSample, len(w.samples))}
	return w, time.Now()
}

// failure 不阻塞地取一个客户端失败；没有失败返回 nil。
func (l *roomLoad) failure() error {
	select {
	case err := <-l.failures:
		return err
	default:
		return nil
	}
}

func loadPercentile(values []time.Duration, q float64) float64 {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	if len(sorted) == 0 {
		return 0
	}
	return float64(sorted[int(float64(len(sorted)-1)*q)].Microseconds()) / 1000
}

// windowStats 把一个窗口的逐 Tick 记录归并为频率与分位数；last 记录每房间上一窗口最后一个 tick，
// 用来跨窗口检查已提交 tick 连续。
type windowStats struct {
	ticks, minRoomTicks       uint64
	steps, commits, intervals []time.Duration
	rooms                     int
}

func (w loadWindow) stats(t *testing.T, last map[domain.Ref]loadSample) windowStats {
	t.Helper()
	var s windowStats
	s.minRoomTicks = math.MaxUint64
	for ref, roomSamples := range w.samples {
		s.rooms++
		s.ticks += uint64(len(roomSamples))
		s.minRoomTicks = min(s.minRoomTicks, uint64(len(roomSamples)))
		prev, havePrev := last[ref]
		for _, sample := range roomSamples {
			s.steps = append(s.steps, sample.step)
			s.commits = append(s.commits, sample.commit)
			if havePrev {
				if sample.tick != prev.tick+1 {
					t.Fatal("missing committed tick")
				}
				s.intervals = append(s.intervals, sample.at.Sub(prev.at))
			}
			prev, havePrev = sample, true
		}
		if last != nil && havePrev {
			last[ref] = prev
		}
	}
	return s
}

func runRoomLoad(t *testing.T, scene string, roomCount int, mechanisms bool) {
	l := startRoomLoad(t, scene, roomCount, mechanisms, 30)
	time.Sleep(time.Second)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := l.record(true)
	time.Sleep(5 * time.Second)
	end := l.record(false)
	elapsed := end.Sub(start)
	runtime.ReadMemStats(&after)
	l.stop()
	close(l.failures)
	for err := range l.failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	w, _ := l.take()
	for _, roomSamples := range w.samples {
		if len(roomSamples) < 2 {
			t.Fatal("room stopped producing frames")
		}
	}
	s := w.stats(t, make(map[domain.Ref]loadSample, roomCount))
	if s.rooms != roomCount {
		t.Fatal("not every room produced committed samples")
	}
	mallocs := after.Mallocs - before.Mallocs
	report := struct {
		Scene                                                                                                                                                            string
		Rooms, Clients, NonstaticPerRoom, GOMAXPROCS                                                                                                                     int
		Seconds, MeanPhysicsHz, DeliveredHzPerClient, PayloadKBpsPerClient, StepP95ms, StepP99ms, CommitP99ms, TickIntervalP99ms, PollP99ms, AllocatedMB, AllocationMBps float64
		Frames, EmptyPolls, GCs, PauseNS                                                                                                                                 uint64
		// 进程级分配次数；按交付帧摊分只用于同口径前后对照，包含每 Tick 与驱动本身的分配。
		MallocsPerSecond, AllocKBPerFrame, AllocsPerFrame float64
	}{scene, roomCount, l.clients, l.nonstatic, runtime.GOMAXPROCS(0), elapsed.Seconds(), float64(s.ticks) / float64(roomCount) / elapsed.Seconds(), float64(w.frames) / float64(l.clients) / elapsed.Seconds(), float64(w.payload) / 1000 / float64(l.clients) / elapsed.Seconds(), loadPercentile(s.steps, .95), loadPercentile(s.steps, .99), loadPercentile(s.commits, .99), loadPercentile(s.intervals, .99), loadPercentile(w.polls, .99), float64(after.TotalAlloc-before.TotalAlloc) / 1e6, float64(after.TotalAlloc-before.TotalAlloc) / 1e6 / elapsed.Seconds(), w.frames, w.empty, uint64(after.NumGC - before.NumGC), after.PauseTotalNs - before.PauseTotalNs, float64(mallocs) / elapsed.Seconds(), float64(after.TotalAlloc-before.TotalAlloc) / 1e3 / float64(max(w.frames, 1)), float64(mallocs) / float64(max(w.frames, 1))}
	encoded, _ := json.Marshal(report)
	t.Log("MULTI_ROOM_RESULT " + string(encoded))
}

// soakMetrics 是长稳测量每行读取的 Go 运行时指标。
var soakMetrics = []string{
	"/memory/classes/heap/objects:bytes", // 存活与未回收对象占用（heap in-use）
	"/gc/heap/objects:objects",
	"/gc/heap/allocs:bytes", // 累计分配
	"/gc/cycles/total:gc-cycles",
	"/sched/goroutines:goroutines",
	"/memory/classes/total:bytes",         // Go 运行时映射的全部内存
	"/memory/classes/heap/released:bytes", // 已归还操作系统、不计入 RSS 的部分
}

// TestPhysicsRoomSoak 是显式启用的长稳测量：机关场景 8 房间 × 20 逻辑客户端连续运行
// ROOST_PHYSICS_SOAK_MINUTES 分钟（房间寿命上限 1 小时，故不超过 55），每
// ROOST_PHYSICS_SOAK_INTERVAL 秒（缺省 15）记录一行：Go 运行时指标、进程 RSS（ps）、
// 该窗口的物理 / 交付频率与分位数；设置 ROOST_PHYSICS_SOAK_TSV 时同时写入该文件。
// 测试只断言链路持续工作（无客户端失败、每房间每窗口有连续 tick）；是否无界增长按预热后的
// 稳定窗口离线拟合斜率判断，原生内存（PhysX 走 malloc）由进程外的 footprint / vmmap 采样。
func TestPhysicsRoomSoak(t *testing.T) {
	raw := os.Getenv("ROOST_PHYSICS_SOAK_MINUTES")
	if raw == "" {
		t.Skip("set ROOST_PHYSICS_SOAK_MINUTES for the physics room soak")
	}
	minutes, err := strconv.ParseFloat(raw, 64)
	if err != nil || minutes <= 0 || minutes > 55 {
		t.Fatalf("ROOST_PHYSICS_SOAK_MINUTES=%q: want (0, 55]", raw)
	}
	interval := 15 * time.Second
	if v := os.Getenv("ROOST_PHYSICS_SOAK_INTERVAL"); v != "" {
		seconds, err := strconv.Atoi(v)
		if err != nil || seconds < 1 {
			t.Fatalf("ROOST_PHYSICS_SOAK_INTERVAL=%q", v)
		}
		interval = time.Duration(seconds) * time.Second
	}
	var out *os.File
	if path := os.Getenv("ROOST_PHYSICS_SOAK_TSV"); path != "" {
		if out, err = os.Create(path); err != nil {
			t.Fatal(err)
		}
		defer out.Close()
	}
	const roomCount = 8
	duration := time.Duration(minutes * float64(time.Minute))
	l := startRoomLoad(t, "mechanisms", roomCount, true, int64(duration/time.Second)+120)
	header := "unix_s\telapsed_s\trss_kb\theap_inuse_bytes\theap_objects\ttotal_alloc_bytes\tgc_cycles\tgoroutines\tgo_mapped_bytes\tgo_released_bytes\tphysics_hz\tmin_room_hz\tdelivered_hz\tempty_polls\tstep_p99_ms\tcommit_p99_ms\ttick_interval_p99_ms\tpoll_p99_ms"
	t.Logf("SOAK pid=%d rooms=%d clients=%d interval=%s", os.Getpid(), roomCount, l.clients, interval)
	t.Log("SOAK " + header)
	if out != nil {
		fmt.Fprintln(out, header)
	}
	samples := make([]metrics.Sample, len(soakMetrics))
	for i, name := range soakMetrics {
		samples[i].Name = name
	}
	last := make(map[domain.Ref]loadSample, roomCount)
	begin := l.record(true)
	previous := begin
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for now := range ticker.C {
		if err := l.failure(); err != nil {
			t.Fatal(err)
		}
		w, at := l.take()
		s := w.stats(t, last)
		if s.rooms != roomCount || s.minRoomTicks == 0 {
			t.Fatalf("only %d rooms committed in the window", s.rooms)
		}
		seconds := at.Sub(previous).Seconds()
		previous = at
		metrics.Read(samples)
		row := []string{
			strconv.FormatInt(at.Unix(), 10),
			strconv.FormatFloat(at.Sub(begin).Seconds(), 'f', 1, 64),
			strconv.FormatInt(soakRSSKB(), 10),
		}
		for _, m := range samples {
			row = append(row, strconv.FormatUint(m.Value.Uint64(), 10))
		}
		row = append(row,
			strconv.FormatFloat(float64(s.ticks)/roomCount/seconds, 'f', 2, 64),
			strconv.FormatFloat(float64(s.minRoomTicks)/seconds, 'f', 2, 64),
			strconv.FormatFloat(float64(w.frames)/float64(l.clients)/seconds, 'f', 2, 64),
			strconv.FormatUint(w.empty, 10),
			strconv.FormatFloat(loadPercentile(s.steps, .99), 'f', 3, 64),
			strconv.FormatFloat(loadPercentile(s.commits, .99), 'f', 3, 64),
			strconv.FormatFloat(loadPercentile(s.intervals, .99), 'f', 3, 64),
			strconv.FormatFloat(loadPercentile(w.polls, .99), 'f', 3, 64),
		)
		line := strings.Join(row, "\t")
		t.Log("SOAK " + line)
		if out != nil {
			fmt.Fprintln(out, line)
		}
		if now.Sub(begin) >= duration {
			return
		}
	}
}

// soakRSSKB 用 ps 读本进程常驻内存（KB，macOS 与 Linux 同口径）；读不到返回 -1。
func soakRSSKB() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// loadToken 给测量用的逻辑客户端生成唯一绑定标识。
func loadToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
