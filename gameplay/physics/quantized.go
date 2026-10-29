package physics

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// QuantizedLayout 每次返回独立布局，避免调用方修改全局协议。位置8、旋转7、
// 速度12、角速度6、休眠1；输入确认不属于公共对象数据。
func QuantizedLayout() chunksync.FieldLayout { return chunksync.FieldLayout{8, 7, 12, 6, 1} }

const QuantizedBodyBytes = 34
const RoomHeaderBytes = 20

type InputAck struct {
	Sequence, Generation uint64
	Active               bool
}

// QuantizedSnapshot 仅用于网络表示，不修改服务器物理状态。固定 25m Chunk，
// 位置/线速度/角速度步长均 .01；Y 和线速度用 int32，避免高速下落静默饱和。
func QuantizedSnapshot(f Frame) (chunksync.Snapshot, error) {
	out := chunksync.Snapshot{Epoch: f.Epoch, Tick: f.Tick, Objects: make([]chunksync.Object, 0, len(f.States))}
	// 全部对象载荷切自同一块数组，每个对象一次分配变为每帧一次；容量截断到本对象，
	// 调用方 append 不会越界写到相邻对象。载荷生成后不再修改。
	backing := make([]byte, len(f.States)*QuantizedBodyBytes)
	for i, s := range f.States {
		x, z := math.Floor(float64(s.Position[0])/25), math.Floor(float64(s.Position[2])/25)
		if !bounded(x, math.MaxInt32) || !bounded(z, math.MaxInt32) {
			return out, errors.New("physics: quantized chunk range")
		}
		key := chunksync.Key{X: int32(x), Z: int32(z)}
		data := backing[i*QuantizedBodyBytes : (i+1)*QuantizedBodyBytes : (i+1)*QuantizedBodyBytes]
		values := []struct {
			at, bits int
			v        float64
		}{
			{0, 16, float64(s.Position[0]) - x*25}, {2, 16, float64(s.Position[2]) - z*25}, {4, 32, float64(s.Position[1])},
			{15, 32, float64(s.Velocity[0])}, {19, 32, float64(s.Velocity[1])}, {23, 32, float64(s.Velocity[2])},
			{27, 16, float64(s.AngularVelocity[0])}, {29, 16, float64(s.AngularVelocity[1])}, {31, 16, float64(s.AngularVelocity[2])},
		}
		for _, v := range values {
			n := math.Round(v.v * 100)
			limit := float64(math.MaxInt32)
			if v.bits == 16 {
				limit = math.MaxInt16
			}
			if !bounded(n, limit) {
				return out, errors.New("physics: quantized scalar range")
			}
			if v.bits == 16 {
				binary.LittleEndian.PutUint16(data[v.at:], uint16(int16(n)))
			} else {
				binary.LittleEndian.PutUint32(data[v.at:], uint32(int32(n)))
			}
		}
		q := s.Rotation
		norm := float64(0)
		largest := 0
		for i, v := range q {
			if !bounded(float64(v), 2) {
				return out, errors.New("physics: invalid quaternion")
			}
			norm += float64(v) * float64(v)
			if math.Abs(float64(v)) > math.Abs(float64(q[largest])) {
				largest = i
			}
		}
		if norm < .5 || norm > 1.5 {
			return out, errors.New("physics: invalid quaternion norm")
		}
		scale := math.Sqrt(.5) * math.MaxInt16 * 2 / math.Sqrt(norm)
		if q[largest] < 0 {
			scale = -scale
		}
		data[8] = byte(largest)
		offset := 9
		for i, v := range q {
			if i == largest {
				continue
			}
			binary.LittleEndian.PutUint16(data[offset:], uint16(int16(math.Round(float64(v)*scale))))
			offset += 2
		}
		if s.Sleeping {
			data[33] = 1
		}
		out.Objects = append(out.Objects, chunksync.Object{ID: s.ID, Chunk: key, Data: data})
	}
	return out, nil
}

// DecodeQuantized 校验完整对象；部分字段由 chunksync 在可信基线之上重建后才到这里。
func DecodeQuantized(o chunksync.Object) (physx.State, error) {
	var s physx.State
	s.ID = o.ID
	d := o.Data
	if len(d) != QuantizedBodyBytes || d[8] > 3 || d[33] > 7 || d[33]&ViewTrajectory != 0 && d[33]&ViewDisplayOnly == 0 {
		return s, errors.New("physics: quantized body format")
	}
	i16 := func(at int) float32 { return float32(int16(binary.LittleEndian.Uint16(d[at:]))) / 100 }
	i32 := func(at int) float32 { return float32(int32(binary.LittleEndian.Uint32(d[at:]))) / 100 }
	if i16(0) < 0 || i16(0) > 25 || i16(2) < 0 || i16(2) > 25 {
		return s, errors.New("physics: local position range")
	}
	s.Position = physx.Vec3{float32(o.Chunk.X)*25 + i16(0), i32(4), float32(o.Chunk.Z)*25 + i16(2)}
	s.Velocity = physx.Vec3{i32(15), i32(19), i32(23)}
	s.AngularVelocity = physx.Vec3{i16(27), i16(29), i16(31)}
	s.Sleeping = d[33]&1 != 0
	sum := float64(0)
	offset := 9
	for i := range 4 {
		if i == int(d[8]) {
			continue
		}
		v := float64(int16(binary.LittleEndian.Uint16(d[offset:]))) / (math.Sqrt(2) * math.MaxInt16)
		s.Rotation[i] = float32(v)
		sum += v * v
		offset += 2
	}
	if sum > 1 {
		return s, errors.New("physics: invalid packed quaternion")
	}
	s.Rotation[d[8]] = float32(math.Sqrt(1 - sum))
	return s, nil
}

// RoomPayload 只附带当前接收成员的采样确认，和 Chunk 一起冻结/重发。
func RoomPayload(chunk []byte, ack InputAck) []byte {
	out := make([]byte, RoomHeaderBytes+len(chunk))
	copy(out, "RP\x02")
	if ack.Active {
		out[3] = 1
	}
	binary.LittleEndian.PutUint64(out[4:], ack.Sequence)
	binary.LittleEndian.PutUint64(out[12:], ack.Generation)
	copy(out[20:], chunk)
	return out
}

// RoomReplica 先完整校验私有确认、字段补丁和量化对象，再原子替换，不发布半帧。
type RoomReplica struct {
	chunksync.Replica
	Ack    InputAck
	States map[uint64]physx.State
	// Profiles 的标志位说明速度是否被省略；展示档的零值不得作为物理事实。
	Profiles map[uint64]byte
}

func (r *RoomReplica) Apply(data []byte) error {
	if len(data) < RoomHeaderBytes+36 || len(data) > chunksync.MaxPayload || string(data[:3]) != "RP\x02" || data[3] > 1 {
		return errors.New("physics: room frame format")
	}
	ack := InputAck{Sequence: binary.LittleEndian.Uint64(data[4:]), Generation: binary.LittleEndian.Uint64(data[12:]), Active: data[3] == 1}
	if ack.Sequence != 0 && ack.Generation == 0 || ack.Active && ack.Sequence == 0 {
		return errors.New("physics: invalid input ack")
	}
	next := r.Replica
	if err := next.ApplyFields(data[20:], QuantizedLayout()); err != nil {
		return err
	}
	states := make(map[uint64]physx.State, len(next.Objects))
	profiles := make(map[uint64]byte, len(next.Objects))
	for id, o := range next.Objects {
		s, err := DecodeQuantized(o)
		if err != nil {
			return err
		}
		states[id] = s
		profiles[id] = o.Data[33] & (ViewDisplayOnly | ViewTrajectory)
	}
	r.Replica = next
	r.Ack = ack
	r.States = states
	r.Profiles = profiles
	return nil
}
