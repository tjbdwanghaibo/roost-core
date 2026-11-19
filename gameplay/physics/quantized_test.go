package physics

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func TestQuantizedRoundTripErrorBoundsAndCanonicalRotation(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 0; n < 1000; n++ {
		s := physx.State{ID: 1, Sleeping: n%2 == 0}
		norm := float64(0)
		for i := range 4 {
			v := rng.Float64()*2 - 1
			s.Rotation[i] = float32(v)
			norm += v * v
		}
		for i := range 4 {
			s.Rotation[i] /= float32(math.Sqrt(norm))
		}
		for i := range 3 {
			s.Position[i] = float32(rng.Float64()*600 - 300)
			s.Velocity[i] = float32(rng.Float64()*2000 - 1000)
			s.AngularVelocity[i] = float32(rng.Float64()*100 - 50)
		}
		snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, States: []physx.State{s}})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeQuantized(snapshot.Objects[0])
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			if math.Abs(float64(decoded.Position[i]-s.Position[i])) > .0051 || math.Abs(float64(decoded.Velocity[i]-s.Velocity[i])) > .0051 || math.Abs(float64(decoded.AngularVelocity[i]-s.AngularVelocity[i])) > .0051 {
				t.Fatalf("quantization error: %+v %+v", s, decoded)
			}
		}
		dot := float64(0)
		for i := range 4 {
			dot += float64(s.Rotation[i]) * float64(decoded.Rotation[i])
			s.Rotation[i] = -s.Rotation[i]
		}
		if math.Abs(dot) < .9999998 {
			t.Fatalf("rotation error %g", dot)
		}
		opposite, err := QuantizedSnapshot(Frame{Epoch: 1, States: []physx.State{s}})
		if err != nil || !bytes.Equal(snapshot.Objects[0].Data, opposite.Objects[0].Data) {
			t.Fatal("q/-q churn", err)
		}
	}
}
func TestQuantizedJitterAndPrivateAckDoNotDirtyPublicObjects(t *testing.T) {
	s := physx.State{ID: 1, Position: physx.Vec3{1.001, 2, 3}, Rotation: physx.Quat{0, 0, 0, 1}, InputSequence: 1, InputGeneration: 7, InputActive: true}
	f := Frame{Epoch: 1, Tick: 1, States: []physx.State{s}}
	snapshot, _ := QuantizedSnapshot(f)
	var c chunksync.Cursor
	var r RoomReplica
	full, _ := c.PlanFields(snapshot, QuantizedLayout())
	data := RoomPayload(full.Data, InputAck{Sequence: 1, Generation: 7, Active: true})
	if err := r.Apply(data); err != nil {
		t.Fatal(err)
	}
	_ = c.Commit(full)
	f.Tick++
	f.States[0].Position[0] = 1.002
	f.States[0].InputSequence = 999
	f.States[0].InputGeneration = 123
	snapshot, _ = QuantizedSnapshot(f)
	delta, _ := c.PlanFields(snapshot, QuantizedLayout())
	if len(delta.Data) != 36 {
		t.Fatalf("unchanged quantized body retransmitted: %d", len(delta.Data))
	}
	data = RoomPayload(delta.Data, InputAck{Sequence: 2, Generation: 7, Active: true})
	if err := r.Apply(data); err != nil {
		t.Fatal(err)
	}
	if r.Ack.Sequence != 2 || r.States[1].InputSequence != 0 || r.States[1].InputGeneration != 0 {
		t.Fatal("private ACK leaked into public state")
	}
	before := r.Tick
	for i := 0; i < len(data); i++ {
		if err := r.Apply(data[:i]); err == nil {
			t.Fatalf("prefix %d accepted", i)
		}
		if r.Tick != before {
			t.Fatal("partial mutation")
		}
	}
}
func TestPackedGoldenSharedWithCSharp(t *testing.T) {
	raw, err := os.ReadFile("../../client/physics-viewer/tests/packed.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct{ Full, Delta string }
	if err = json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	full, _ := base64.StdEncoding.DecodeString(g.Full)
	delta, _ := base64.StdEncoding.DecodeString(g.Delta)
	s := physx.State{ID: 1, Position: physx.Vec3{12.34, 1.23, -.01}, Rotation: physx.Quat{0, 0, 0, 1}, Velocity: physx.Vec3{1.23, -10.5, 0}}
	snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, States: []physx.State{s}})
	if err != nil {
		t.Fatal(err)
	}
	var c chunksync.Cursor
	p, err := c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data[58:], full) {
		t.Fatalf("full differs: %v", p.Data[58:])
	}
	_ = c.Commit(p)
	s.Position[0] = 12.35
	snapshot, _ = QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: []physx.State{s}})
	p, err = c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data[58:], delta) {
		t.Fatalf("delta differs: %v", p.Data[58:])
	}
}

func TestRoomInvalidBodyDoesNotPublishAckOrBaseline(t *testing.T) {
	s := physx.State{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}
	snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: []physx.State{s}})
	if err != nil {
		t.Fatal(err)
	}
	var c chunksync.Cursor
	p, err := c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	var r RoomReplica
	if err = r.Apply(RoomPayload(p.Data, InputAck{Sequence: 1, Generation: 7})); err != nil {
		t.Fatal(err)
	}
	if err = c.Commit(p); err != nil {
		t.Fatal(err)
	}
	snapshot.Tick++
	snapshot.Objects[0].Data[33] = 16 // 合法字段补丁，但状态字节带了未定义的位。
	bad, err := c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(RoomPayload(bad.Data, InputAck{Sequence: 2, Generation: 7})); err == nil {
		t.Fatal("invalid state accepted")
	}
	if r.Tick != 1 || r.Sequence != 1 || r.Ack.Sequence != 1 || r.Objects[1].Data[33] != 0 || r.States[1].Sleeping {
		t.Fatal("failed frame mutated state or acknowledgement")
	}
	snapshot.Objects[0].Data[33] = 1
	good, err := c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(RoomPayload(good.Data, InputAck{Sequence: 2, Generation: 7})); err != nil {
		t.Fatal(err)
	}
	if !r.States[1].Sleeping || r.Ack.Sequence != 2 {
		t.Fatal("valid retry not applied")
	}
}

func TestQuantizedRejectsOutOfRangeInsteadOfClamping(t *testing.T) {
	for _, mutate := range []func(*physx.State){
		func(s *physx.State) { s.Position[0] = float32(math.NaN()) },
		func(s *physx.State) { s.Position[1] = 3e7 },
		func(s *physx.State) { s.Velocity[1] = 3e7 },
		func(s *physx.State) { s.AngularVelocity[0] = 400 },
		func(s *physx.State) { s.Rotation = physx.Quat{} },
	} {
		s := physx.State{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}
		mutate(&s)
		if _, err := QuantizedSnapshot(Frame{Epoch: 1, States: []physx.State{s}}); err == nil {
			t.Fatalf("accepted invalid state: %+v", s)
		}
	}
}

func TestRoomProfileFlagsAndOldProtocolRejection(t *testing.T) {
	snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Objects[0].Data[33] = ViewDisplayOnly | ViewTrajectory | 1
	var c chunksync.Cursor
	p, err := c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	packet := RoomPayload(p.Data, InputAck{})
	var r RoomReplica
	// 只认 RP3：RP1、RP2（没有传送标记的旧客户端协议）和还不存在的 RP4 都在帧头拒绝，不动基线。
	if packet[2] != 3 {
		t.Fatalf("room frames must carry protocol version 3, got %d", packet[2])
	}
	for _, version := range []byte{1, 2, 4} {
		other := bytes.Clone(packet)
		other[2] = version
		if err = r.Apply(other); err == nil || r.Objects != nil {
			t.Fatalf("protocol version %d accepted or mutated baseline", version)
		}
	}
	if err = r.Apply(packet); err != nil {
		t.Fatal(err)
	}
	if r.Profiles[1] != ViewDisplayOnly|ViewTrajectory || !r.States[1].Sleeping {
		t.Fatal("profile confused with sleeping")
	}
	snapshot.Objects[0].Data[33] = ViewTrajectory
	if _, err = DecodeQuantized(snapshot.Objects[0]); err == nil {
		t.Fatal("trajectory without display flag accepted")
	}
	// 状态字节一共四个已定义的位；传送标记可以与任何合法档位组合，更高的位一律拒绝。
	for flags := range 256 {
		snapshot.Objects[0].Data[33] = byte(flags)
		valid := flags < 16 && (flags&int(ViewTrajectory) == 0 || flags&int(ViewDisplayOnly) != 0)
		state, err := DecodeQuantized(snapshot.Objects[0])
		if (err == nil) != valid {
			t.Fatalf("state byte %08b: err=%v, want valid=%v", flags, err, valid)
		}
		if valid && state.Sleeping != (flags&1 != 0) {
			t.Fatalf("state byte %08b: sleeping bit misread", flags)
		}
	}
	// 客户端副本把传送标记放在 Profiles 里，不混进休眠。
	snapshot.Tick++
	snapshot.Objects[0].Data[33] = ViewTeleported
	p, err = c.PlanFields(snapshot, QuantizedLayout())
	if err != nil {
		t.Fatal(err)
	}
	var fresh RoomReplica
	if err = fresh.Apply(RoomPayload(p.Data, InputAck{})); err != nil {
		t.Fatal(err)
	}
	if fresh.Profiles[1] != ViewTeleported || fresh.States[1].Sleeping {
		t.Fatalf("teleport mark misread: profile %b sleeping %v", fresh.Profiles[1], fresh.States[1].Sleeping)
	}
}

// 完整的 RP3 房间帧金样，与 C# 共用：RoomFull 是接收方（刚体 2）第一次看到刚体 1 的整帧；
// RoomTeleport 是刚体 1 被传送后的下一帧（字段增量，状态字节带 ViewTeleported，私有确认 7/3/活动）。
// 帧的生成走服务端同一条路径：量化快照 → 视图分档 → 字段增量 → 私有头。
func TestRoomTeleportGoldenSharedWithCSharp(t *testing.T) {
	raw, err := os.ReadFile("../../client/physics-viewer/tests/packed.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ RoomFull, RoomTeleport string }
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{
		{ID: 1, Position: physx.Vec3{3, 1, -2}, Rotation: physx.Quat{0, 0, 0, 1}, Velocity: physx.Vec3{0, 0, 1.5}},
		{ID: 2, Rotation: physx.Quat{0, 0, 0, 1}},
	}
	scene := NewReplicationScene(bodies, nil)
	var cursor chunksync.Cursor
	var ledger map[uint64]ViewEntry
	frame := func(tick uint64, ack InputAck) []byte {
		t.Helper()
		snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		if err != nil {
			t.Fatal(err)
		}
		view, err := scene.Frame(states).Plan(snapshot, 2, nil, ledger, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		fields, err := cursor.PlanFields(view.Snapshot, QuantizedLayout())
		if err != nil {
			t.Fatal(err)
		}
		if err = cursor.Commit(fields); err != nil {
			t.Fatal(err)
		}
		ledger = view.Entries
		return RoomPayload(fields.Data, ack)
	}
	full := frame(1, InputAck{})
	states[0].Position, states[0].Velocity, states[0].Teleports = physx.Vec3{2, .5, 3}, physx.Vec3{}, 1
	teleport := frame(2, InputAck{Sequence: 7, Generation: 3, Active: true})
	for _, c := range []struct {
		name, want string
		got        []byte
	}{{"RoomFull", golden.RoomFull, full}, {"RoomTeleport", golden.RoomTeleport, teleport}} {
		if got := base64.StdEncoding.EncodeToString(c.got); got != c.want {
			t.Errorf("%s differs from the golden shared with C#:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
	if t.Failed() {
		return
	}
	// 金样本身按客户端的方式解码，结果就是 C# 侧要断言的值。
	var replica RoomReplica
	if err = replica.Apply(full); err != nil {
		t.Fatal(err)
	}
	if replica.Profiles[1] != 0 || replica.States[1].Position != (physx.Vec3{3, 1, -2}) || replica.States[1].Velocity != (physx.Vec3{0, 0, 1.5}) {
		t.Fatalf("full frame: %+v profile %b", replica.States[1], replica.Profiles[1])
	}
	if err = replica.Apply(teleport); err != nil {
		t.Fatal(err)
	}
	if replica.Profiles[1] != ViewTeleported || replica.Profiles[2] != 0 || replica.States[1].Position != (physx.Vec3{2, .5, 3}) || replica.States[1].Velocity != (physx.Vec3{}) || replica.Ack != (InputAck{Sequence: 7, Generation: 3, Active: true}) || replica.Tick != 2 {
		t.Fatalf("teleport frame: %+v profiles %v ack %+v tick %d", replica.States[1], replica.Profiles, replica.Ack, replica.Tick)
	}
	if string(teleport[:3]) != "RP\x03" || len(teleport) >= len(full) {
		t.Fatalf("teleport frame must be an RP3 field delta: header %q, %d bytes against a %d-byte full frame", teleport[:3], len(teleport), len(full))
	}
}
