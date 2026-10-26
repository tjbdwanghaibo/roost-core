// Package physics 负责固定步长原生模拟与已提交结果交接。
// 模拟器是物理状态的唯一权威；Nest 提交失败后 Run 停止，不假装能回滚原生求解器。
package physics

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

const StepSeconds float32 = .02
const SceneVersion uint32 = 1

type Frame struct {
	Epoch, Tick uint64
	States      []physx.State
	StepTime    time.Duration
}
type Commit func(context.Context, Frame) error

// RunConfig 由业务选择输入与固定步长；输入回调在物理执行器上运行，不能直接修改 Entity。
type RunConfig struct {
	Epoch      uint64
	Step       time.Duration
	Input      func(uint64) []physx.Command
	BeforeStep func(uint64) error // 物理执行器上的可信机制维护；失败停止房间。
}

// ComparisonScene 创建用户指定的对照场景。通用后端本身不限制胶囊数量或场景大小。
func ComparisonScene() []physx.Body {
	bodies := []physx.Body{
		{ID: 1001, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{25, .5, 25}},
		{ID: 1002, Shape: physx.Box, Position: physx.Vec3{-25, 2, 0}, Dimensions: physx.Vec3{.5, 2, 25}},
		{ID: 1003, Shape: physx.Box, Position: physx.Vec3{25, 2, 0}, Dimensions: physx.Vec3{.5, 2, 25}},
		{ID: 1004, Shape: physx.Box, Position: physx.Vec3{0, 2, -25}, Dimensions: physx.Vec3{25, 2, .5}},
		{ID: 1005, Shape: physx.Box, Position: physx.Vec3{0, 2, 25}, Dimensions: physx.Vec3{25, 2, .5}},
		{ID: 21, Shape: physx.Sphere, Position: physx.Vec3{0, 2.1, 0}, Dimensions: physx.Vec3{2}, Mass: 80},
	}
	for i := range 20 {
		p := spawn(i)
		bodies = append(bodies, physx.Body{ID: uint64(i + 1), Shape: physx.Capsule, Position: p, Dimensions: physx.Vec3{.4, .6, 0}, Mass: 10})
	}
	return bodies
}
func spawn(i int) physx.Vec3 {
	a := float64(i) * 2 * math.Pi / 20
	return physx.Vec3{float32(12 * math.Cos(a)), 1.1, float32(12 * math.Sin(a))}
}

// Commands 是与位置无关的输入带。相同 Tick 两端收到相同加速度，不因避障分歧引入不同输入。
// 朝中心、反向、切向交替，使球被推动并观察跨块迁移。
func Commands(tick uint64) []physx.Command {
	out := make([]physx.Command, 20)
	phase := ((tick - 1) / 150) % 4
	for i := range 20 {
		p := spawn(i)
		x, z := -p[0]/12, -p[2]/12
		switch phase {
		case 1:
			x, z = -z, x
		case 2:
			x, z = -x, -z
		case 3:
			x, z = z, -x
		}
		out[i] = physx.Command{ID: uint64(i + 1), Acceleration: physx.Vec3{x * float32(8+i%3*2), 0, z * float32(8+i%3*2)}}
	}
	return out
}

// Run 只从物理执行 goroutine 调用。等待 Nest 提交在此发生，业务 worker 不等待 PhysX。
// 不追赶无限积压；宿主调度落后时模拟时间放慢，Tick 与固定 dt 仍保持一致。
func Run(ctx context.Context, w *physx.World, config RunConfig, commit Commit) error {
	if config.Step <= 0 || config.Step > 100*time.Millisecond {
		return errors.New("physics: invalid fixed step")
	}
	if config.Epoch == 0 || commit == nil {
		return errors.New("physics: epoch and commit required")
	}
	ticker := time.NewTicker(config.Step)
	defer ticker.Stop()
	for tick := uint64(1); ; tick++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		start := time.Now()
		if config.BeforeStep != nil {
			if err := config.BeforeStep(tick); err != nil {
				return err
			}
		}
		var commands []physx.Command
		if config.Input != nil {
			commands = config.Input(tick)
		}
		states, err := w.Step(float32(config.Step.Seconds()), commands)
		if err != nil {
			return err
		}
		if err = commit(ctx, Frame{Epoch: config.Epoch, Tick: tick, States: states, StepTime: time.Since(start)}); err != nil {
			return err
		}
	}
}

// Snapshot 使用块内 float32 坐标，先建立无量化误差的基准。
// 70 字节对象载荷：位置3、旋转4、线速度3、角速度3个 float32，再加休眠、实际采样输入序列与输入有效标志。
func Snapshot(f Frame, chunkSize float32) (chunksync.Snapshot, error) {
	if chunkSize <= 0 || math.IsNaN(float64(chunkSize)) || math.IsInf(float64(chunkSize), 0) {
		return chunksync.Snapshot{}, errors.New("physics: invalid chunk size")
	}
	s := chunksync.Snapshot{Epoch: f.Epoch, Tick: f.Tick, Objects: make([]chunksync.Object, len(f.States))}
	for i, b := range f.States {
		x, z := math.Floor(float64(b.Position[0])/float64(chunkSize)), math.Floor(float64(b.Position[2])/float64(chunkSize))
		if math.IsNaN(x) || math.IsNaN(z) || x < math.MinInt32 || x > math.MaxInt32 || z < math.MinInt32 || z > math.MaxInt32 {
			return chunksync.Snapshot{}, errors.New("physics: chunk coordinate out of range")
		}
		k := chunksync.Key{X: int32(x), Z: int32(z)}
		data := make([]byte, 70)
		offset := 0
		put := func(v float32) { binary.LittleEndian.PutUint32(data[offset:], math.Float32bits(v)); offset += 4 }
		put(b.Position[0] - float32(k.X)*chunkSize)
		put(b.Position[1])
		put(b.Position[2] - float32(k.Z)*chunkSize)
		for _, v := range b.Rotation {
			put(v)
		}
		for _, v := range b.Velocity {
			put(v)
		}
		for _, v := range b.AngularVelocity {
			put(v)
		}
		if b.Sleeping {
			data[52] = 1
		}
		binary.LittleEndian.PutUint64(data[53:], b.InputSequence)
		binary.LittleEndian.PutUint64(data[62:], b.InputGeneration)
		if b.InputActive {
			data[61] = 1
		}
		s.Objects[i] = chunksync.Object{ID: b.ID, Chunk: k, Data: data}
	}
	return s, nil
}
