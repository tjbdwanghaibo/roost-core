// comparison-host 是原生 PhysX 与 Unity PhysicsScene 的数值对照验收宿主（原 cmd/physics-viewer 的 comparison 场景）。
// 仅监听 loopback，无帐号、房间或业务；房间、输入、Gate 接入等运行链路属于正式服务（service/physicsroom、
// wiring/physicsroom），见 docs/framework/PHYSICS-ROOM-SERVICE.md。这里只保留正式服务不提供的对照能力：
// 单个确定初态的世界、固定输入带、首个订阅建立后才开始推进、整场景广播给只做旁观对照的 Unity。
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

const frameMessageID = 0x50485901

// getter 只持有这个验收宿主的瞬态世界目标，没有冷加载或持久化。
type worldEntity struct {
	*entity.EntityBase
	state gamephysics.StateDAO
}

func (w *worldEntity) Base() *entity.EntityBase { return w.EntityBase }

type getter struct{ world *worldEntity }

func (g getter) Get(_ context.Context, id int64, _ entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if id != g.world.ID() {
		return nil, errors.New("unknown physics world")
	}
	return g.world, nil
}
func (g getter) GetMany(ctx context.Context, ids []int64, cats []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		v, e := g.Get(ctx, id, 0)
		if e != nil {
			return nil, e
		}
		out[i] = v
	}
	return out, nil
}
func main() {
	if e := run(); e != nil && !errors.Is(e, context.Canceled) {
		log.Fatal(e)
	}
}
func run() error {
	addr := flag.String("listen", "127.0.0.1:19091", "local Unity viewer address")
	hz := flag.Int("send-hz", 20, "state send rate, 20 or 50")
	duration := flag.Duration("duration", 0, "optional run duration")
	flag.Parse()
	if *hz != 20 && *hz != 50 {
		return errors.New("send-hz must be 20 or 50")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("validation host only accepts a loopback listen address")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}
	bodies := gamephysics.ComparisonScene()
	expectedBodies := 0
	for _, body := range bodies {
		if body.Shape != physx.Box {
			expectedBodies++
		}
	}
	world, err := physx.New(-9.81, 1)
	if err != nil {
		return err
	}
	defer world.Close()
	for _, b := range bodies {
		if err = world.Add(b); err != nil {
			return err
		}
	}
	// 该独立进程只登记自己的 Kind，不占用库调用方的全局 Kind 编号。
	if err = entity.RegisterEntityKindCategory(240, 1); err != nil {
		return err
	}
	id, err := entity.BuildEntityID(1, 240)
	if err != nil {
		return err
	}
	target := &worldEntity{EntityBase: entity.NewEntityBase(id, 1, true, 240)}
	engine := nest.NewEngine(nest.NestOptionWithGetter(getter{target}), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 128}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}))
	name := nest.NewHandlerName("physics.accept_frame")
	// Guard 内将物理事实写入可回滚 DAO；返回成功后，执行器才向网络发布。
	err = engine.RegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		if len(args) != 1 {
			return nil, errors.New("physics frame argument required")
		}
		f, ok := args[0].(gamephysics.Frame)
		if !ok || f.Epoch == 0 || len(f.States) != expectedBodies {
			return nil, errors.New("invalid physics frame")
		}
		if err := target.state.Accept(f); err != nil {
			return nil, err
		}
		return target.state.Snapshot(), nil
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
	if err != nil {
		return err
	}
	if err = engine.Start(); err != nil {
		return err
	}
	defer engine.Shutdown(context.Background())
	var epochBytes [8]byte
	if _, err = rand.Read(epochBytes[:]); err != nil {
		return err
	}
	epoch := binary.LittleEndian.Uint64(epochBytes[:]) | 1
	var latest atomic.Pointer[gamephysics.SpatialFrame]
	initial, err := gamephysics.NewSpatialFrame(gamephysics.InitialFrame(epoch, bodies), 25, 10)
	if err != nil {
		return err
	}
	latest.Store(initial)
	ready := make(chan struct{})
	var readyOnce sync.Once
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	var clients sync.WaitGroup
	slots := make(chan struct{}, 64)
	go func() { <-ctx.Done(); listener.Close() }()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			select {
			case slots <- struct{}{}:
				clients.Add(1)
				go func() {
					defer clients.Done()
					defer func() { <-slots }()
					serve(ctx, conn, &latest, *hz, func() { readyOnce.Do(func() { close(ready) }) })
				}()
			default:
				conn.Close()
			}
		}
	}()
	log.Printf("physics viewer %s; %s; scene=%d physics=50Hz send=%dHz", *addr, physx.Version(), gamephysics.SceneVersion, *hz)
	var count uint64
	var total, maxStep time.Duration
	select {
	case <-ready:
	case <-ctx.Done():
		listener.Close()
		<-acceptDone
		clients.Wait()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil
		}
		return ctx.Err()
	}
	err = gamephysics.Run(ctx, world, gamephysics.RunConfig{Epoch: epoch, Step: 20 * time.Millisecond, Input: gamephysics.Commands}, func(ctx context.Context, f gamephysics.Frame) error {
		accepted, e := engine.Request(ctx, name, id, nest.NewParams(f))
		if e != nil {
			return e
		}
		frame := accepted.(gamephysics.Frame)
		snapshot, e := gamephysics.NewSpatialFrame(frame, 25, 10)
		if e != nil {
			return e
		}
		latest.Store(snapshot)
		count++
		total += f.StepTime
		if f.StepTime > maxStep {
			maxStep = f.StepTime
		}
		return nil
	})
	cancel()
	listener.Close()
	<-acceptDone
	clients.Wait()
	if count > 0 {
		fmt.Printf("ticks=%d physics_mean_us=%.2f physics_max_us=%.2f\n", count, float64(total.Microseconds())/float64(count), float64(maxStep.Microseconds()))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// serve 把最新已提交快照按 hz 整场景推给一个连接；首包写出后调用 ready，宿主才开始推进物理。
func serve(ctx context.Context, conn net.Conn, latest *atomic.Pointer[gamephysics.SpatialFrame], hz int, ready func()) {
	defer conn.Close()
	ticker := time.NewTicker(time.Second / time.Duration(hz))
	defer ticker.Stop()
	var cursor chunksync.Cursor
	lastTick := ^uint64(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		s := latest.Load()
		if s == nil || s.Snapshot.Tick == lastTick {
			continue
		}
		p, err := cursor.Plan(s.Snapshot)
		if err != nil {
			return
		}
		data, err := wire.Encode([]*wire.Packet{{Flags: wire.FlagSync | wire.FlagPush, MsgID: frameMessageID, Payload: p.Data}}, chunksync.MaxPayload)
		if err != nil {
			return
		}
		if err = conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return
		}
		for len(data) > 0 {
			n, e := conn.Write(data)
			if e != nil || n == 0 {
				return
			}
			data = data[n:]
		}
		if err = cursor.Commit(p); err != nil {
			return
		}
		lastTick = s.Snapshot.Tick
		ready()
	}
}
