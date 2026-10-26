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

// MotionDefault 保留旧场景语义：Box 静态，其余动态；新关卡建议显式指定。
type Motion uint32

const (
	MotionDefault Motion = iota
	Static
	Dynamic
	Kinematic
)

type Body struct {
	Motion Motion

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
	Jump         bool
	// Target 仅用于运动学刚体，由可信关卡驱动填写，不能来自玩家输入。
	Target   bool
	Position Vec3
	Rotation Quat
}
type State struct {
	InputGeneration           uint64 // 成员代际，防止重连后的序列复用确认旧输入。
	InputSequence             uint64 // 房间附加：此已提交帧实际采样的输入序列；不是 RPC 接收确认。
	InputActive               bool   // TTL 内仍在施力；过期帧仍携带最后采样序列以终止客户端预测。
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
	if b.ID == 0 || b.Shape > Sphere || b.Motion > Kinematic || !validVec(b.Position) || !validVec(b.Dimensions) || !finite(b.Mass) {
		return errors.New("physx: invalid body")
	}
	if b.Dimensions[0] <= 0 || b.Shape == Box && (b.Dimensions[1] <= 0 || b.Dimensions[2] <= 0) || b.Shape == Capsule && b.Dimensions[1] < 0 || b.MotionType() != Static && b.Mass <= 0 {
		return errors.New("physx: invalid shape dimensions or mass")
	}
	return nil
}

// MotionType 返回最终的运动类型，供快照、准入与客户端描述共用。
func (b Body) MotionType() Motion {
	if b.Motion != MotionDefault {
		return b.Motion
	}
	if b.Shape == Box {
		return Static
	}
	return Dynamic
}

// Validate 在构造原生世界前验证不可信关卡数据。
func (b Body) Validate() error { return validateBody(b) }
func validQuat(q Quat) bool {
	var n float64
	for _, v := range q {
		if !finite(v) {
			return false
		}
		n += float64(v) * float64(v)
	}
	return math.Abs(n-1) <= 1e-4
}

// Joint 的两个锚点均为 actor 局部坐标；B=0 时 AnchorB/RotationB 为世界姿态。
// Hinge 的旋转轴是锚点局部 X；Distance 为有界弹簧绳，可用于可信业务抓取。
type JointKind uint32

const (
	Distance JointKind = iota
	Hinge
)

type Joint struct {
	ID, A, B                           uint64
	Kind                               JointKind
	AnchorA, AnchorB                   Vec3
	RotationA, RotationB               Quat
	MaxDistance, Stiffness, Damping    float32
	MotorSpeed, MotorForce, BreakForce float32
}

func (j Joint) Validate() error {
	if j.ID == 0 || j.A == 0 || j.A == j.B || j.Kind > Hinge || !validVec(j.AnchorA) || !validVec(j.AnchorB) {
		return errors.New("physx: invalid joint")
	}
	for _, q := range []Quat{j.RotationA, j.RotationB} {
		if q != (Quat{}) && !validQuat(q) {
			return errors.New("physx: invalid joint rotation")
		}
	}
	for _, v := range []float32{j.MaxDistance, j.Stiffness, j.Damping, j.MotorSpeed, j.MotorForce, j.BreakForce} {
		if !finite(v) || math.Abs(float64(v)) > 1e6 {
			return errors.New("physx: invalid joint parameter")
		}
	}
	if j.MaxDistance < 0 || j.Stiffness < 0 || j.Damping < 0 || j.MotorForce < 0 || j.BreakForce <= 0 {
		return errors.New("physx: invalid joint limit")
	}
	return nil
}
