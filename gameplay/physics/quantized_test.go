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
	snapshot.Objects[0].Data[33] = 8 // 合法字段补丁，但休眠值非法。
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
	old := bytes.Clone(packet)
	old[2] = 1
	if err = r.Apply(old); err == nil || r.Objects != nil {
		t.Fatal("old protocol accepted or mutated baseline")
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
}
