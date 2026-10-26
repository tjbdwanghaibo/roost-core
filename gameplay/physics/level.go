package physics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// Level 是不执行用户脚本的 UGC 关卡白名单。发布层负责作者权限和审核；
// 房间固定内容摘要，编辑后的关卡必须开新房，不能静默替换进行中的碰撞世界。
type Level struct {
	Version  int
	Name     string
	Revision uint32
	Bodies   []physx.Body
	Players  []uint64 // 槽号从 1 开始，服务器认证后选槽，不能由输入指定 body。
	Motions  []MotionTrack
	Joints   []physx.Joint
}

const MaxLevelBytes = 256 << 10

// MotionTrack 由服务器 tick 驱动：Translation 为正弦位移幅度，Axis 为旋转轴，
// AngularSpeed 单位 rad/s。没有墙钟追帧，也不接受玩家传入目标姿态。
type MotionTrack struct {
	ID            uint64
	Translation   physx.Vec3
	PeriodSeconds float32
	Axis          physx.Vec3
	AngularSpeed  float32
}

func DecodeLevel(data []byte) (Level, error) {
	var l Level
	if len(data) > MaxLevelBytes {
		return l, errors.New("physics: level byte limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&l); err != nil {
		return Level{}, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Level{}, errors.New("physics: trailing level data")
	}
	if err := l.Validate(); err != nil {
		return Level{}, err
	}
	return l, nil
}
func (l Level) Validate() error {
	if l.Version != 1 || l.Revision == 0 || len(l.Name) == 0 || len(l.Name) > 128 || len(l.Bodies) == 0 || len(l.Bodies) > 256 || len(l.Players) == 0 || len(l.Players) > 20 || len(l.Joints) > 128 {
		return errors.New("physics: level header or capacity")
	}
	bodies := make(map[uint64]physx.Body, len(l.Bodies))
	for _, b := range l.Bodies {
		if err := b.Validate(); err != nil {
			return err
		}
		if _, ok := bodies[b.ID]; ok || b.ID >= 1<<63 {
			return errors.New("physics: duplicate or reserved body ID")
		}
		// UGC 发布边界比通用 PhysX 世界严格，拒绝极端尺寸/质量破坏求解稳定性。
		if b.Dimensions[0] < .05 || b.MotionType() != physx.Static && (b.Mass < .01 || b.Mass > 10000) {
			return errors.New("physics: level body scale or mass limit")
		}
		for i := range 3 {
			if !bounded(float64(b.Position[i]), 1000) || !bounded(float64(b.Dimensions[i]), 100) {
				return errors.New("physics: level geometry limit")
			}
		}
		if b.Shape == physx.Box && (b.Dimensions[1] < .05 || b.Dimensions[2] < .05) {
			return errors.New("physics: level box too thin")
		}
		bodies[b.ID] = b
	}
	seen := make(map[uint64]bool)
	for _, id := range l.Players {
		b, ok := bodies[id]
		if !ok || seen[id] || b.MotionType() != physx.Dynamic || b.Shape == physx.Box {
			return errors.New("physics: invalid player body")
		}
		seen[id] = true
	}
	return ValidateMechanisms(l.Bodies, l.Motions, l.Joints)
}

// ValidateMechanisms 供直接 Spec 与文件加载共用；关节 ID 高位留给瞬态抓取。
func ValidateMechanisms(bodies []physx.Body, motions []MotionTrack, joints []physx.Joint) error {
	if len(motions) > 128 || len(joints) > 128 {
		return errors.New("physics: mechanism limit")
	}
	ids := make(map[uint64]physx.Body, len(bodies))
	for _, b := range bodies {
		ids[b.ID] = b
	}
	seen := make(map[uint64]bool)
	for _, m := range motions {
		b, ok := ids[m.ID]
		if !ok || b.MotionType() != physx.Kinematic || seen[m.ID] || !bounded(float64(m.PeriodSeconds), 3600) || m.PeriodSeconds < .1 || !bounded(float64(m.AngularSpeed), 20) {
			return errors.New("physics: invalid motion")
		}
		seen[m.ID] = true
		n := float64(0)
		for i := range 3 {
			if !bounded(float64(m.Translation[i]), 1000) || !bounded(float64(m.Axis[i]), 1) {
				return errors.New("physics: invalid motion vector")
			}
			n += float64(m.Axis[i]) * float64(m.Axis[i])
		}
		if m.AngularSpeed != 0 && math.Abs(n-1) > 1e-4 {
			return errors.New("physics: motion axis must be unit length")
		}
	}
	clear(seen)
	for _, j := range joints {
		if err := j.Validate(); err != nil {
			return err
		}
		a, aok := ids[j.A]
		b, bok := ids[j.B]
		if seen[j.ID] || j.ID >= 1<<63 || !aok || j.B != 0 && !bok || (a.MotionType() != physx.Dynamic && (j.B == 0 || b.MotionType() != physx.Dynamic)) {
			return errors.New("physics: invalid joint actors or ID")
		}
		seen[j.ID] = true
	}
	return nil
}
func bounded(x, limit float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && math.Abs(x) <= limit
}

// Content 返回稳定的精确发布字节和 SHA-256，客户端按这些字节校验后再建立场景。
func (l Level) Content() ([]byte, string, error) {
	if err := l.Validate(); err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(l)
	if err != nil {
		return nil, "", err
	}
	if len(data) > MaxLevelBytes {
		return nil, "", errors.New("physics: level byte limit")
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}
func MotionCommands(bodies []physx.Body, tracks []MotionTrack, tick uint64) []physx.Command {
	out := make([]physx.Command, 0, len(tracks))
	seconds := float64(tick) * float64(StepSeconds)
	for _, m := range tracks {
		var b physx.Body
		for _, v := range bodies {
			if v.ID == m.ID {
				b = v
				break
			}
		}
		p := b.Position
		phase := math.Sin(2 * math.Pi * math.Mod(seconds, float64(m.PeriodSeconds)) / float64(m.PeriodSeconds))
		for i := range 3 {
			p[i] += m.Translation[i] * float32(phase)
		}
		q := b.Rotation
		if q == (physx.Quat{}) {
			q = physx.Quat{0, 0, 0, 1}
		}
		angle := math.Mod(seconds*float64(m.AngularSpeed), 2*math.Pi) / 2
		a := physx.Quat{m.Axis[0] * float32(math.Sin(angle)), m.Axis[1] * float32(math.Sin(angle)), m.Axis[2] * float32(math.Sin(angle)), float32(math.Cos(angle))}
		// 世界轴旋转乘初始姿态。
		rot := physx.Quat{a[3]*q[0] + a[0]*q[3] + a[1]*q[2] - a[2]*q[1], a[3]*q[1] - a[0]*q[2] + a[1]*q[3] + a[2]*q[0], a[3]*q[2] + a[0]*q[1] - a[1]*q[0] + a[2]*q[3], a[3]*q[3] - a[0]*q[0] - a[1]*q[1] - a[2]*q[2]}
		out = append(out, physx.Command{ID: m.ID, Target: true, Position: p, Rotation: rot})
	}
	return out
}

// ObstacleLevel 是可导出、修改再加载的最小关卡：升降台、转杆、马达锤与可抓球。
func ObstacleLevel() Level {
	return Level{Version: 1, Name: "ugc-obstacles", Revision: 1, Players: []uint64{1, 2}, Bodies: []physx.Body{
		{ID: 100, Shape: physx.Box, Position: physx.Vec3{0, -.5, 0}, Dimensions: physx.Vec3{20, .5, 20}},
		{ID: 1, Shape: physx.Sphere, Position: physx.Vec3{0, .6, -3}, Dimensions: physx.Vec3{.5}, Mass: 1},
		{ID: 2, Shape: physx.Sphere, Position: physx.Vec3{1, .6, -3}, Dimensions: physx.Vec3{.5}, Mass: 1},
		{ID: 3, Shape: physx.Sphere, Position: physx.Vec3{0, .6, -2}, Dimensions: physx.Vec3{.5}, Mass: 1},
		{ID: 4, Shape: physx.Box, Motion: physx.Kinematic, Position: physx.Vec3{5, 1, 0}, Dimensions: physx.Vec3{2, .25, 2}, Mass: 10},
		{ID: 5, Shape: physx.Box, Motion: physx.Kinematic, Position: physx.Vec3{0, 1, 3}, Dimensions: physx.Vec3{3, .2, .2}, Mass: 10},
		{ID: 6, Shape: physx.Box, Motion: physx.Dynamic, Position: physx.Vec3{-6, 3, 0}, Dimensions: physx.Vec3{.3, 2, .3}, Mass: 10},
	}, Motions: []MotionTrack{
		{ID: 4, Translation: physx.Vec3{0, 1, 0}, PeriodSeconds: 6},
		{ID: 5, PeriodSeconds: 6, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1},
	}, Joints: []physx.Joint{{ID: 1, Kind: physx.Hinge, A: 6, AnchorA: physx.Vec3{0, 2, 0}, AnchorB: physx.Vec3{-6, 5, 0}, MotorSpeed: 1, MotorForce: 100, BreakForce: 10000}}}
}
