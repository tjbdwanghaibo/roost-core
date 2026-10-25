// Package physx 提供可选的 CPU PhysX 后端。使用 -tags physx 并先构建原生库。
// World 持有原生资源，Close 必须在所属执行器结束后调用；所有操作在内部串行。
package physx

import (
	"errors"
	"math"
)

var ErrUnavailable = errors.New("physx: build native library and enable -tags physx with cgo")
var ErrClosed = errors.New("physx: world closed")

type Vec3 [3]float32
type Quat [4]float32
type Shape uint32

const (
	Box Shape = iota
	Capsule
	Sphere
)

type Body struct {
	ID         uint64
	Shape      Shape
	Position   Vec3
	Rotation   Quat // 零值表示单位旋转；非零值必须是单位四元数。
	Dimensions Vec3
	Mass       float32
}
type Command struct {
	ID           uint64
	Acceleration Vec3
}
type State struct {
	ID                        uint64
	Position                  Vec3
	Rotation                  Quat
	Velocity, AngularVelocity Vec3
	Sleeping                  bool
}

func finite(x float32) bool { return !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0) }
func validVec(v Vec3) bool {
	for _, x := range v {
		if !finite(x) || math.Abs(float64(x)) > 1e6 {
			return false
		}
	}
	return true
}
func validateBody(b Body) error {
	if b.Rotation != (Quat{}) {
		var norm float32
		for _, v := range b.Rotation {
			if !finite(v) {
				return errors.New("physx: invalid rotation")
			}
			norm += v * v
		}
		if math.Abs(float64(norm-1)) > 1e-4 {
			return errors.New("physx: rotation must be normalized")
		}
	}
	if b.ID == 0 || b.Shape > Sphere || !validVec(b.Position) || !validVec(b.Dimensions) || !finite(b.Mass) {
		return errors.New("physx: invalid body")
	}
	if b.Dimensions[0] <= 0 || b.Shape == Box && (b.Dimensions[1] <= 0 || b.Dimensions[2] <= 0) || b.Shape == Capsule && b.Dimensions[1] < 0 || b.Shape != Box && b.Mass <= 0 {
		return errors.New("physx: invalid shape dimensions or mass")
	}
	return nil
}
