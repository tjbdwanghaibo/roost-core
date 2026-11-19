// level-drill 是三进程联机演练的客户端：以 physics-slot-N 经真实 Gate 登录 physicsroom-local，
// 只发输入、只看收到的包，在示例关卡（gameplay/physics.ObstacleLevel）上走完
// “过检查点 A → 走出地面掉落 → 重生到检查点 A → 到终点结算”，逐项核对后退出。由 scripts/physics/verify-level.sh 调用。
//
// 路线按示例关卡写死；关卡内容本身取自握手（校验摘要后解码），出生点坐标与区域范围以它为准。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	gamephysics "github.com/tjbdwanghaibo/roost-core/gameplay/physics"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	frameMessageID   uint32 = 0x50485901
	inputMessageID   uint32 = 0x50485902
	contentMessageID uint32 = 0x50485903
	levelMessageID   uint32 = 0x50485904
)

func main() {
	addr := flag.String("addr", "127.0.0.1:19093", "Gate TCP address")
	slot := flag.Int("slot", 1, "player slot: the ticket is physics-slot-N")
	limit := flag.Duration("timeout", 2*time.Minute, "overall limit")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), *limit)
	defer cancel()
	if err := run(ctx, *addr, *slot); err != nil {
		fmt.Println("LEVEL_DRILL_FAIL", err)
		os.Exit(1)
	}
}

// steer 是读循环写、输入协程读的目标点；没有目标时输入为零。
type steer struct {
	mu       sync.Mutex
	active   bool
	x, z     float32
	position physx.Vec3
	velocity physx.Vec3
}

func (s *steer) to(x, z float32) {
	s.mu.Lock()
	s.active, s.x, s.z = true, x, z
	s.mu.Unlock()
}

func (s *steer) observe(state physx.State) {
	s.mu.Lock()
	s.position, s.velocity = state.Position, state.Velocity
	s.mu.Unlock()
}

// acceleration 朝目标点加速并按当前速度阻尼，水平合加速度不超过 10 m/s²（服务端上限 20）。
func (s *steer) acceleration() (float32, float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return 0, 0
	}
	x := 3*(s.x-s.position[0]) - 2.5*s.velocity[0]
	z := 3*(s.z-s.position[2]) - 2.5*s.velocity[2]
	if norm := float32(math.Hypot(float64(x), float64(z))); norm > 10 {
		x, z = x*10/norm, z*10/norm
	}
	return x, z
}

func run(ctx context.Context, addr string, slot int) error {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err = wire.Write(conn, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte(fmt.Sprintf("physics-slot-%d", slot))}}, 8192); err != nil {
		return err
	}
	if ack, err := wire.Read(conn, 8192); err != nil || ack.MsgID != 0 || ack.Seq != 1 || ack.Flags&wire.FlagPush != 0 {
		return fmt.Errorf("auth ack %+v: %v", ack, err)
	}
	if err = wire.Write(conn, []*wire.Packet{{MsgID: contentMessageID, Seq: 2}}, 8192); err != nil {
		return err
	}
	reply, err := wire.Read(conn, 1<<20)
	if err != nil || reply.MsgID != contentMessageID || reply.Seq != 2 {
		return fmt.Errorf("level handshake %+v: %v", reply, err)
	}
	course, body, err := content(reply.Payload)
	if err != nil {
		return err
	}
	if len(course.Flow) == 0 || len(course.Regions) == 0 || course.DefaultSpawn == "" {
		return errors.New("the room's level has no flow: start game with --level and an exported schema 2 level")
	}
	checkpoint, err := spawnPosition(course, "mid_a", slot)
	if err != nil {
		return err
	}

	var pilot steer
	inputCtx, stopInput := context.WithCancel(ctx)
	inputDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for sequence := uint64(1); ; sequence++ {
			x, z := pilot.acceleration()
			payload := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), sequence)
			payload = protowire.AppendFixed32(protowire.AppendTag(payload, 2, protowire.Fixed32Type), math.Float32bits(x))
			payload = protowire.AppendFixed32(protowire.AppendTag(payload, 3, protowire.Fixed32Type), math.Float32bits(z))
			if err := wire.Write(conn, []*wire.Packet{{MsgID: inputMessageID, Seq: uint32(sequence) + 2, Payload: payload}}, 8192); err != nil {
				inputDone <- err
				return
			}
			select {
			case <-inputCtx.Done():
				inputDone <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	defer stopInput()

	// 路线：先向 -x 让开正前方的小球与旋转横杆（半长 3），沿 x=-4.5 走到检查点 A 的纵深，再横向进入区域；
	// 然后朝 +z 走出地面（地面到 z=20）；重生后直奔终点。
	route := [][2]float32{{-4.5, -3}, {-4.5, 8}, {-1.5, 8}}
	const (
		toCheckpoint = iota
		toEdge
		falling
		toGoal
		settled
	)
	stage, leg := toCheckpoint, 0
	pilot.to(route[0][0], route[0][1])
	var replica gamephysics.RoomReplica
	var view domain.LevelView
	frames, views, teleports := 0, 0, 0
	var edgeTick, respawnTick, reachedTick, finishTick uint64
	var last physx.State // 重生之前最后看到的下落状态
	var lastTick uint64
	var entry float64 // 按 last 推算的进入掉落区的帧号
	var fallsAtRespawn level.Value
	for settledFrames := -1; stage != settled || frames == settledFrames; {
		packet, err := wire.Read(conn, 1<<20)
		if err != nil {
			return fmt.Errorf("stage %d after %d frames and %d views, body %+v, view %+v: %w", stage, frames, views, replica.States[body], view, err)
		}
		switch packet.MsgID {
		case inputMessageID: // 输入回执
		case levelMessageID:
			if packet.Flags != wire.FlagPush {
				return fmt.Errorf("level view flags=%d, want a protobuf push", packet.Flags)
			}
			next, err := domain.DecodeLevelView(packet.Payload)
			if err != nil {
				return fmt.Errorf("level view %x: %w", packet.Payload, err)
			}
			if views != 0 && next.Revision <= view.Revision {
				return fmt.Errorf("level view revision %d after %d", next.Revision, view.Revision)
			}
			view = next
			views++
			if view.Phase == level.Finishing && stage != settled {
				if stage != toGoal || view.Result != "clear" {
					return fmt.Errorf("flow finished in stage %d with %+v", stage, view)
				}
				stage, settledFrames, finishTick = settled, frames, replica.Tick
			}
		case frameMessageID:
			if packet.Flags != wire.FlagPush|wire.FlagSync {
				return fmt.Errorf("frame flags=%d, want a sync push", packet.Flags)
			}
			if views == 0 {
				return errors.New("the first frame arrived before the level view of a running flow")
			}
			if err = replica.Apply(packet.Payload); err != nil {
				return err
			}
			frames++
			state, ok := replica.States[body]
			if !ok {
				return errors.New("controlled body missing from a frame")
			}
			pilot.observe(state)
			teleported := replica.Profiles[body]&gamephysics.ViewTeleported != 0
			if teleported {
				teleports++
				if stage != falling {
					return fmt.Errorf("tick %d: teleported in stage %d at %v", replica.Tick, stage, state.Position)
				}
				// 带标记的这一帧里球已经在检查点 A 的出生点上，不在下落途中。传送时速度清零，但这一帧是传送之后
				// 1～3 个物理步才采样的（50Hz 模拟、20Hz 下发），重力与仍然生效的输入最多带来约 1.2 m/s；
				// 下落 4m 以上的速度超过 9 m/s，1.5 足以区分。
				if distance(state.Position, checkpoint) > .3 || speed(state.Velocity) > 1.5 {
					return fmt.Errorf("teleport frame at tick %d: body %+v, want it just placed at %v", replica.Tick, state, checkpoint)
				}
				// 进入掉落区到重生只有一两个物理步，比下发间隔（2.5 步）还短，客户端通常看不到“球在区域里”的那一帧。
				// 所以用重生前最后一帧按抛体推算球心到达区域顶面（y=-4）的时刻：重生不能早于它，也不该晚太多。
				entry = float64(lastTick) + fallTicks(float64(last.Position[1]), float64(last.Velocity[1]), -4)
				if after := float64(replica.Tick) - entry; after < -.5 || after > 10 {
					return fmt.Errorf("respawn frame at tick %d, but the ball last seen at tick %d (y=%.2f vy=%.2f) reaches the kill region at about tick %.1f",
						replica.Tick, lastTick, last.Position[1], last.Velocity[1], entry)
				}
				respawnTick, fallsAtRespawn = replica.Tick, view.Variables["global/falls"]
				stage = toGoal
				pilot.to(-14, 14)
				continue
			}
			switch stage {
			case toCheckpoint:
				if near(state.Position, route[leg], .6) {
					if leg++; leg == len(route) {
						reachedTick = replica.Tick
						stage = toEdge
						pilot.to(-1.5, 26)
					} else {
						pilot.to(route[leg][0], route[leg][1])
					}
				}
			case toEdge:
				// 地面顶面在 y=0，球半径 0.5：球心低于 0 就是已经离开地面在下落。
				if state.Position[1] < 0 {
					edgeTick, stage = replica.Tick, falling
					last, lastTick = state, replica.Tick
				}
			case falling:
				last, lastTick = state, replica.Tick
				if replica.Tick > edgeTick+250 {
					return fmt.Errorf("no respawn within 5s of leaving the ground (tick %d, now %d, body %v)", edgeTick, replica.Tick, state.Position)
				}
			}
		default:
			return fmt.Errorf("unexpected packet %+v", packet)
		}
	}
	stopInput()
	if err = <-inputDone; err != nil {
		return err
	}
	if teleports != 1 {
		return fmt.Errorf("%d teleport frames, want exactly one", teleports)
	}
	if fallsAtRespawn != level.Int(1) || view.Variables["global/falls"] != level.Int(1) {
		return fmt.Errorf("global/falls was %+v when the respawn frame arrived and %+v at the end, want 1", fallsAtRespawn, view.Variables["global/falls"])
	}
	final := replica.States[body].Position
	if final[0] < -16.5 || final[0] > -11.5 || final[2] < 11.5 || final[2] > 16.5 {
		return fmt.Errorf("finished with the body at %v, outside the goal", final)
	}
	fmt.Printf("LEVEL_DRILL_PASS slot=%d body=%d frames=%d views=%d checkpoint_tick=%d left_ground_tick=%d last_seen_falling_tick=%d last_seen_y=%.2f kill_entry_tick_estimate=%.1f respawn_frame_tick=%d ticks_from_entry_to_respawn_frame=%.1f finish_tick=%d phase=%s result=%s falls=%d revision=%d\n",
		slot, body, frames, views, reachedTick, edgeTick, lastTick, last.Position[1], entry, respawnTick, float64(respawnTick)-entry, finishTick, view.Phase, view.Result, view.Variables["global/falls"].Int, view.Revision)
	return nil
}

// content 解码内容握手的应答（Protobuf：1 关卡字节、2 SHA-256、3 受控刚体），核对摘要后解码关卡。
func content(payload []byte) (gamephysics.Level, uint64, error) {
	var data []byte
	var hash string
	var body uint64
	for len(payload) > 0 {
		field, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return gamephysics.Level{}, 0, errors.New("level handshake: malformed reply")
		}
		payload = payload[n:]
		switch {
		case field == 3 && typ == protowire.VarintType:
			body, n = protowire.ConsumeVarint(payload)
		case (field == 1 || field == 2) && typ == protowire.BytesType:
			var value []byte
			if value, n = protowire.ConsumeBytes(payload); field == 1 {
				data = value
			} else {
				hash = string(value)
			}
		default:
			return gamephysics.Level{}, 0, errors.New("level handshake: unknown field")
		}
		if n < 0 {
			return gamephysics.Level{}, 0, errors.New("level handshake: malformed reply")
		}
		payload = payload[n:]
	}
	sum := sha256.Sum256(data)
	if body == 0 || hex.EncodeToString(sum[:]) != hash {
		return gamephysics.Level{}, 0, errors.New("level handshake: content hash mismatch")
	}
	course, err := gamephysics.DecodeLevel(data)
	return course, body, err
}

func spawnPosition(course gamephysics.Level, id string, slot int) (physx.Vec3, error) {
	for _, spawn := range course.Spawns {
		if spawn.ID == id {
			return spawn.Positions[(slot-1)%len(spawn.Positions)], nil
		}
	}
	return physx.Vec3{}, fmt.Errorf("level has no spawn %q: this drill is written for the sample obstacle level", id)
}

func near(position physx.Vec3, target [2]float32, radius float64) bool {
	return math.Hypot(float64(position[0]-target[0]), float64(position[2]-target[1])) < radius
}

func distance(a, b physx.Vec3) float64 {
	return math.Sqrt(float64((a[0]-b[0])*(a[0]-b[0]) + (a[1]-b[1])*(a[1]-b[1]) + (a[2]-b[2])*(a[2]-b[2])))
}

func speed(v physx.Vec3) float64 { return distance(v, physx.Vec3{}) }

// fallTicks 是从高度 y、竖直速度 vy 自由下落到高度 floor 还要多少个 20ms 的物理步；已经在 floor 以下时为 0。
func fallTicks(y, vy, floor float64) float64 {
	const g = 9.81
	if y <= floor {
		return 0
	}
	return (vy + math.Sqrt(vy*vy+2*g*(y-floor))) / g / .02
}
