package physics

import (
	"bytes"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	"testing"
)

func TestViewCadencePromotionRemovalAndFieldRestoration(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{12, 0, 0}, Velocity: physx.Vec3{1, 0, 0}}}
	policy := DefaultReplicationPolicy()
	plan := func(tick uint64, previous map[uint64]ViewEntry, grabs map[uint64]uint64) ViewPlan {
		t.Helper()
		s, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		if err != nil {
			t.Fatal(err)
		}
		p, err := PlanView(s, states, bodies, nil, 1, grabs, previous, policy)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := plan(1, nil, nil)
	if first.Entries[2].Class != "far" || !bytes.Equal(first.Entries[2].Object.Data[15:33], make([]byte, 18)) {
		t.Fatal("far fields not trimmed")
	}
	states[1].Position[0] = 13
	deferred := plan(4, first.Entries, nil)
	if deferred.Entries[2].Tick != 1 || deferred.Deferred["far"] != 1 {
		t.Fatal("far cadence")
	}
	refreshed := plan(11, first.Entries, nil)
	if refreshed.Entries[2].Tick != 11 {
		t.Fatal("far did not refresh")
	}
	states[1].Position[0] = 2
	near := plan(12, refreshed.Entries, nil)
	if near.Entries[2].Tick != 12 || near.Entries[2].Class != "near" || bytes.Equal(near.Entries[2].Object.Data[15:33], make([]byte, 18)) {
		t.Fatal("promotion did not restore velocity immediately")
	}
	states[1].Position[0] = 13
	grabbed := plan(13, first.Entries, map[uint64]uint64{2: 1})
	if grabbed.Entries[2].Class != "near" {
		t.Fatal("reverse grab not prioritized")
	}
	states[1].Position[0] = 25.1
	migrated := plan(4, first.Entries, nil)
	if migrated.Entries[2].Tick != 4 || migrated.Entries[2].Object.Chunk.X != 1 {
		t.Fatal("migration deferred")
	}
	states = states[:1]
	left := plan(5, migrated.Entries, nil)
	if len(left.Entries) != 1 || len(left.Snapshot.Objects) != 1 {
		t.Fatal("removal deferred")
	}
	// 暂存计划不能污染 ACK 基线。
	if first.Entries[2].Tick != 1 || first.Entries[2].Object.Chunk.X != 0 {
		t.Fatal("baseline mutated")
	}
}

func TestViewLargeFastBodiesAndMotionCalibration(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Box, Motion: physx.Kinematic, Dimensions: physx.Vec3{1, 1, 1}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{12, 0, 0}}}
	run := func(tick uint64, previous map[uint64]ViewEntry) ViewPlan {
		s, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		p, err := PlanView(s, states, bodies, []MotionTrack{{ID: 2}}, 1, nil, previous, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := run(1, nil)
	if first.Entries[2].Class != "motion" {
		t.Fatal("motion profile")
	}
	states[1].Position[0] = 13
	held := run(49, first.Entries)
	if held.Entries[2].Tick != 1 {
		t.Fatal("early calibration")
	}
	updated := run(51, first.Entries)
	if updated.Entries[2].Tick != 51 {
		t.Fatal("missing calibration")
	}
	states[1].Velocity[0] = -100
	fast := run(52, updated.Entries)
	if fast.Entries[2].Class != "near" {
		t.Fatal("fast approaching body throttled")
	}
	states[1].Velocity[0] = 0
	bodies[1].Dimensions[0] = 10
	large := run(52, updated.Entries)
	if large.Entries[2].Class != "near" {
		t.Fatal("large nearby surface throttled")
	}
}

func TestSharedReplicationFrameOwnsItsInputs(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Position: physx.Vec3{11, 0, 0}, Rotation: physx.Quat{0, 0, 0, 1}}}
	snapshot, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: states})
	scene := NewReplicationScene(bodies, nil)
	frame := scene.Frame(states)
	bodies[1].Dimensions[0] = 100
	states[1].Position[0] = 1
	for i := 0; i < 2; i++ {
		p, err := frame.Plan(snapshot, 1, nil, nil, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if p.Entries[2].Class != "far" {
			t.Fatal("shared index aliased mutable inputs")
		}
	}
	next := scene.Frame(states)
	p, err := next.Plan(snapshot, 1, nil, nil, DefaultReplicationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[2].Class != "near" {
		t.Fatal("next frame did not refresh index")
	}
}

func BenchmarkRecipientViewIndex(b *testing.B) {
	bodies := CrowdScene()
	states := InitialFrame(1, bodies).States
	snapshot, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: states})
	cached := NewReplicationScene(bodies, nil).Frame(states)
	for _, shared := range []bool{false, true} {
		name := "rebuild"
		if shared {
			name = "shared"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				if shared {
					_, err = cached.Plan(snapshot, 51, nil, nil, DefaultReplicationPolicy())
				} else {
					_, err = PlanView(snapshot, states, bodies, nil, 51, nil, nil, DefaultReplicationPolicy())
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
