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
	"runtime"
	"slices"
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

func runRoomLoad(t *testing.T, scene string, roomCount int, mechanisms bool) {
	if err := entity.RegisterEntityKindCategory(240, 1); err != nil {
		t.Fatal(err)
	}
	host, err := NewEntityHost(240, 1, roomCount, nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 256}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}))
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	var mu sync.Mutex
	recording := false
	samples := make(map[domain.Ref][]loadSample)
	cfg := domain.DefaultConfig(func(ctx context.Context, ref domain.Ref, f gamephysics.Frame) error {
		start := time.Now()
		err := host.Commit(ctx, ref, f)
		if err == nil {
			mu.Lock()
			if recording {
				samples[ref] = append(samples[ref], loadSample{f.Tick, f.StepTime, time.Since(start), time.Now()})
			}
			mu.Unlock()
		}
		return err
	})
	cfg.MaxRooms = roomCount
	cfg.Closed = host.Forget
	rooms, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer rooms.Shutdown(context.Background())
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
	nonstatic := gamephysics.CrowdBodies
	for roomID := 0; roomID < roomCount; roomID++ {
		spec := domain.Spec{Instance: instance, ID: fmt.Sprintf("load-%s-%d", scene, roomID), Bodies: gamephysics.CrowdScene(), LifetimeSeconds: 30}
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
		nonstatic = gamephysics.CrowdBodies
		if mechanisms {
			nonstatic += 12
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
	// 每个逻辑客户端独立调度，和 Relay 一样从响应后等 50ms；不使用串行全体轮询。
	clientCtx, stopClients := context.WithCancel(ctx)
	var clients sync.WaitGroup
	failures := make(chan error, len(observers))
	var payload, frames, empty uint64
	var polls []time.Duration
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
						failures <- err
					}
					return
				}
				began := time.Now()
				d, err := rooms.Poll(clientCtx, o.member, o.ack)
				duration := time.Since(began)
				timer.Reset(50 * time.Millisecond)
				if err != nil {
					if clientCtx.Err() == nil {
						failures <- err
					}
					return
				}
				mu.Lock()
				if recording {
					polls = append(polls, duration)
					payload += uint64(len(d.Payload))
					if len(d.Payload) > 0 {
						frames++
					} else {
						empty++
					}
				}
				mu.Unlock()
				if len(d.Payload) > 0 {
					if len(d.Payload) > cfg.MaxFrameBytes {
						failures <- errors.New("frame exceeded budget")
						return
					}
					if o.ack == 0 {
						var replica gamephysics.RoomReplica
						if err := replica.Apply(d.Payload); err != nil {
							failures <- err
							return
						}
						if _, ok := replica.States[uint64(51+i%20)]; !ok {
							failures <- errors.New("self missing")
							return
						}
					}
					o.ack = d.Sequence
				}
			}
		}(i)
	}
	defer func() { stopClients(); clients.Wait() }()
	time.Sleep(time.Second)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	mu.Lock()
	recording = true
	start := time.Now()
	mu.Unlock()
	time.Sleep(5 * time.Second)
	mu.Lock()
	recording = false
	elapsed := time.Since(start)
	mu.Unlock()
	runtime.ReadMemStats(&after)
	stopClients()
	clients.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	var steps, commits, intervals []time.Duration
	var ticks uint64
	mu.Lock()
	captured := make(map[domain.Ref][]loadSample, len(samples))
	for ref, values := range samples {
		captured[ref] = slices.Clone(values)
	}
	mu.Unlock()
	for _, roomSamples := range captured {
		if len(roomSamples) < 2 {
			t.Fatal("room stopped producing frames")
		}
		ticks += uint64(len(roomSamples))
		for i, s := range roomSamples {
			steps = append(steps, s.step)
			commits = append(commits, s.commit)
			if i > 0 {
				if s.tick != roomSamples[i-1].tick+1 {
					t.Fatal("missing committed tick")
				}
				intervals = append(intervals, s.at.Sub(roomSamples[i-1].at))
			}
		}
	}
	measuredRooms := len(captured)
	if measuredRooms != roomCount {
		t.Fatal("not every room produced committed samples")
	}
	percentile := func(values []time.Duration, q float64) float64 {
		sorted := slices.Clone(values)
		slices.Sort(sorted)
		if len(sorted) == 0 {
			return 0
		}
		return float64(sorted[int(float64(len(sorted)-1)*q)].Microseconds()) / 1000
	}
	mallocs := after.Mallocs - before.Mallocs
	report := struct {
		Scene                                                                                                                                                            string
		Rooms, Clients, NonstaticPerRoom, GOMAXPROCS                                                                                                                     int
		Seconds, MeanPhysicsHz, DeliveredHzPerClient, PayloadKBpsPerClient, StepP95ms, StepP99ms, CommitP99ms, TickIntervalP99ms, PollP99ms, AllocatedMB, AllocationMBps float64
		Frames, EmptyPolls, GCs, PauseNS                                                                                                                                 uint64
		// 进程级分配次数；按交付帧摊分只用于同口径前后对照，包含每 Tick 与驱动本身的分配。
		MallocsPerSecond, AllocKBPerFrame, AllocsPerFrame float64
	}{scene, roomCount, len(observers), nonstatic, runtime.GOMAXPROCS(0), elapsed.Seconds(), float64(ticks) / float64(roomCount) / elapsed.Seconds(), float64(frames) / float64(len(observers)) / elapsed.Seconds(), float64(payload) / 1000 / float64(len(observers)) / elapsed.Seconds(), percentile(steps, .95), percentile(steps, .99), percentile(commits, .99), percentile(intervals, .99), percentile(polls, .99), float64(after.TotalAlloc-before.TotalAlloc) / 1e6, float64(after.TotalAlloc-before.TotalAlloc) / 1e6 / elapsed.Seconds(), frames, empty, uint64(after.NumGC - before.NumGC), after.PauseTotalNs - before.PauseTotalNs, float64(mallocs) / elapsed.Seconds(), float64(after.TotalAlloc-before.TotalAlloc) / 1e3 / float64(max(frames, 1)), float64(mallocs) / float64(max(frames, 1))}
	encoded, _ := json.Marshal(report)
	t.Log("MULTI_ROOM_RESULT " + string(encoded))
}

// loadToken 给测量用的逻辑客户端生成唯一绑定标识。
func loadToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
