package physics

import (
	"errors"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// StateDAO 保存已接受的瞬态物理事实，不落库。调用者在所属 Entity Guard 内使用，
// handler 必须为 rollback=undo；成功返回并释放 Guard 后才能向同步器交出快照。
// 原生世界不可回滚；若 Nest 提交失败，外层执行器必须停止，不能跳过失败 Tick。
type StateDAO struct{ frame Frame }

func (d *StateDAO) Accept(f Frame) error {
	if f.Epoch == 0 || f.Tick == 0 || len(f.States) > 16384 {
		return errors.New("physics: invalid frame")
	}
	if d.frame.Tick != 0 && (f.Epoch != d.frame.Epoch || f.Tick != d.frame.Tick+1) {
		return errors.New("physics: out-of-order result")
	}
	seen := make(map[uint64]bool, len(f.States))
	for _, s := range f.States {
		if s.ID == 0 || seen[s.ID] {
			return errors.New("physics: duplicate body")
		}
		seen[s.ID] = true
		values := []float32{s.Position[0], s.Position[1], s.Position[2], s.Rotation[0], s.Rotation[1], s.Rotation[2], s.Rotation[3], s.Velocity[0], s.Velocity[1], s.Velocity[2], s.AngularVelocity[0], s.AngularVelocity[1], s.AngularVelocity[2]}
		for _, v := range values {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return errors.New("physics: non-finite result")
			}
		}
		var norm float32
		for _, v := range s.Rotation {
			norm += v * v
		}
		if norm < .5 || norm > 1.5 {
			return errors.New("physics: invalid rotation")
		}
	}
	old := d.frame
	if !nest.RecordUndo(d, 1, func() error { d.frame = old; return nil }) {
		return errors.New("physics: StateDAO requires Nest undo transaction")
	}
	f.States = slices.Clone(f.States)
	d.frame = f
	return nil
}
func (d *StateDAO) Snapshot() Frame { f := d.frame; f.States = slices.Clone(f.States); return f }

// InitialFrame 是刚体创建后的基线，与 native 初始化值一致；尚未推进物理时间。
func InitialFrame(epoch uint64, bodies []physx.Body) Frame {
	f := Frame{Epoch: epoch}
	for _, b := range bodies {
		if b.Shape != physx.Box {
			q := b.Rotation
			if q == (physx.Quat{}) {
				q = physx.Quat{0, 0, 0, 1}
			}
			f.States = append(f.States, physx.State{ID: b.ID, Position: b.Position, Rotation: q})
		}
	}
	return f
}
