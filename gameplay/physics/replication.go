package physics

import (
	"bytes"
	"errors"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// RP2 的状态字节 bit0 是休眠，bit1 表示展示档（速度省略），bit2 表示轨迹档。
const ViewDisplayOnly byte = 2
const ViewTrajectory byte = 4

// ReplicationPolicy 只裁剪接收者网络视图，不改变物理步长或 AOI。
// 零值禁用；距离按包围球表面计算，另预留相对速度 * 0.2s 的接近距离。
type ReplicationPolicy struct {
	Enabled                            bool
	Near, Middle                       float64
	MiddleTicks, FarTicks, MotionTicks uint64
}

func DefaultReplicationPolicy() ReplicationPolicy {
	return ReplicationPolicy{Enabled: true, Near: 5, Middle: 9, MiddleTicks: 5, FarTicks: 10, MotionTicks: 50}
}
func (p ReplicationPolicy) Validate() error {
	if !p.Enabled {
		return nil
	}
	if !bounded(p.Near, 100) || !bounded(p.Middle, 100) || p.Near < 2.5 || p.Middle < p.Near || p.MiddleTicks < 3 || p.FarTicks < p.MiddleTicks || p.FarTicks > 10 || p.MotionTicks < p.FarTicks || p.MotionTicks > 50 {
		return errors.New("physics: invalid replication policy")
	}
	return nil
}

type ViewEntry struct {
	Object chunksync.Object
	Tick   uint64
	Class  string
}
type ViewPlan struct {
	Snapshot          chunksync.Snapshot
	Entries           map[uint64]ViewEntry
	Samples, Deferred map[string]int
}

// PlanView 基线必须来自已 ACK 的计划；调用方在 ACK 时才替换 Entries。
// 暂缓更新保留旧对象而不是删除；新可见/跨区/升档立即刷新。退出视野不延迟。
func PlanView(snapshot chunksync.Snapshot, states []physx.State, bodies []physx.Body, tracks []MotionTrack, observer uint64, grabs map[uint64]uint64, previous map[uint64]ViewEntry, policy ReplicationPolicy) (ViewPlan, error) {
	return NewReplicationScene(bodies, tracks).Frame(states).Plan(snapshot, observer, grabs, previous, policy)
}

// ReplicationScene 拥有建房时固定的形状和轨迹索引；不保留调用方切片。
type ReplicationScene struct {
	radii   map[uint64]float64
	motions map[uint64]bool
}

func NewReplicationScene(bodies []physx.Body, tracks []MotionTrack) *ReplicationScene {
	scene := &ReplicationScene{radii: make(map[uint64]float64, len(bodies)), motions: make(map[uint64]bool, len(tracks))}
	for _, b := range bodies {
		scene.radii[b.ID] = bodyRadius(b)
	}
	for _, m := range tracks {
		scene.motions[m.ID] = true
	}
	return scene
}

// ReplicationFrame 每个已提交帧只创建一次；所有接收者共享只读索引。
type ReplicationFrame struct {
	scene *ReplicationScene
	poses map[uint64]physx.State
}

func (s *ReplicationScene) Frame(states []physx.State) *ReplicationFrame {
	f := &ReplicationFrame{scene: s, poses: make(map[uint64]physx.State, len(states))}
	for _, state := range states {
		f.poses[state.ID] = state
	}
	return f
}
func (f *ReplicationFrame) State(id uint64) (physx.State, bool) { s, ok := f.poses[id]; return s, ok }
func (f *ReplicationFrame) Plan(snapshot chunksync.Snapshot, observer uint64, grabs map[uint64]uint64, previous map[uint64]ViewEntry, policy ReplicationPolicy) (ViewPlan, error) {
	out := ViewPlan{Snapshot: chunksync.Snapshot{Epoch: snapshot.Epoch, Tick: snapshot.Tick}, Entries: make(map[uint64]ViewEntry, len(snapshot.Objects)), Samples: make(map[string]int), Deferred: make(map[string]int)}
	if err := policy.Validate(); err != nil {
		return out, err
	}
	poses, radii, motions := f.poses, f.scene.radii, f.scene.motions
	self, ok := poses[observer]
	if !ok {
		return out, errors.New("physics: missing view observer")
	}
	critical := map[uint64]bool{observer: true}
	for a, b := range grabs {
		if a == observer || b == observer {
			critical[a] = true
			critical[b] = true
		}
	}
	for _, object := range snapshot.Objects {
		s, ok := poses[object.ID]
		if !ok || len(object.Data) != QuantizedBodyBytes {
			return out, errors.New("physics: invalid view object")
		}
		class := "near"
		period := uint64(0)
		if policy.Enabled && !critical[object.ID] {
			distance, speed := float64(0), float64(0)
			for i := range 3 {
				d := float64(s.Position[i] - self.Position[i])
				distance += d * d
				v := float64(s.Velocity[i] - self.Velocity[i])
				speed += v * v
			}
			gap := math.Sqrt(distance) - radii[observer] - radii[object.ID] - math.Sqrt(speed)*.2
			old, exists := previous[object.ID]
			// 降档增加 1m 滞回；升档以当前权威距离立即发生。
			near, middle := policy.Near, policy.Middle
			if exists && old.Class == "near" {
				near++
			}
			if exists && old.Class == "middle" {
				middle++
			}
			if gap > near {
				class = "middle"
				period = policy.MiddleTicks
				if gap > middle {
					class = "far"
					period = policy.FarTicks
				}
				if motions[object.ID] {
					class = "motion"
					period = policy.MotionTicks
				}
			}
		}
		old, exists := previous[object.ID]
		rank := func(c string) int {
			switch c {
			case "near":
				return 0
			case "middle":
				return 1
			case "far":
				return 2
			default:
				return 3
			}
		}
		refresh := !exists || old.Object.Chunk != object.Chunk || rank(class) < rank(old.Class) || snapshot.Tick < old.Tick || snapshot.Tick-old.Tick >= period
		if !refresh {
			out.Entries[object.ID] = old
			out.Snapshot.Objects = append(out.Snapshot.Objects, old.Object)
			out.Deferred[class]++
			continue
		}
		object.Data = slices.Clone(object.Data)
		if class != "near" {
			clear(object.Data[15:33])
			object.Data[33] |= ViewDisplayOnly
			if class == "motion" {
				object.Data[33] |= ViewTrajectory
			}
		} // 展示档显式清零速度，升档重新携带当前权威速度。
		entry := ViewEntry{Object: object, Tick: snapshot.Tick, Class: class}
		out.Entries[object.ID] = entry
		out.Snapshot.Objects = append(out.Snapshot.Objects, object)
		if !exists || old.Object.Chunk != object.Chunk || !bytes.Equal(old.Object.Data, object.Data) {
			out.Samples[class]++
		}
	}
	return out, nil
}
func bodyRadius(b physx.Body) float64 {
	switch b.Shape {
	case physx.Box:
		return math.Sqrt(float64(b.Dimensions[0])*float64(b.Dimensions[0]) + float64(b.Dimensions[1])*float64(b.Dimensions[1]) + float64(b.Dimensions[2])*float64(b.Dimensions[2]))
	case physx.Capsule:
		return float64(b.Dimensions[0] + b.Dimensions[1])
	default:
		return float64(b.Dimensions[0])
	}
}
