package physics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
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

// 固定场景走完整房间复制链路（量化快照、AOI、视图分档、字段增量、私有确认），
// 逐帧比较载荷与视图账本。分配优化不得改变任何一帧的字节或分档结果。
func TestRoomReplicationPipelineStaysByteIdentical(t *testing.T) {
	bodies := CrowdScene()
	tracks := []MotionTrack{{ID: 1, PeriodSeconds: 4, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1}}
	base := InitialFrame(9, bodies).States
	scene := NewReplicationScene(bodies, tracks)
	type member struct {
		body              uint64
		cursor            chunksync.Cursor
		pending           *chunksync.Plan
		payload           []byte
		view, nextView    map[uint64]ViewEntry
		visible, nextSeen map[uint64]bool
	}
	members := []*member{{body: 51}, {body: 55}, {body: 60}, {body: 70}}
	digest := sha256.New()
	write := func(values ...any) { fmt.Fprintln(digest, values...) }
	polls := 0
	coverage := map[string]int{}
	for tick := uint64(1); tick <= 90; tick++ {
		states := slices.Clone(base)
		for i := range states {
			s := &states[i]
			phase := float64(tick)/7 + float64(s.ID)
			// 部分刚体做大幅往返，穿越 AOI 半径、分档边界和 25m Chunk。
			amplitude := float32(1 + s.ID%5*4)
			s.Position[0] += amplitude * float32(math.Sin(phase))
			s.Position[2] += amplitude * float32(math.Cos(phase*.7))
			s.Velocity = physx.Vec3{amplitude * float32(math.Cos(phase)) / 7, 0, -amplitude * float32(math.Sin(phase*.7)) / 10}
			half := float32(phase) / 2
			s.Rotation = physx.Quat{0, float32(math.Sin(float64(half))), 0, float32(math.Cos(float64(half)))}
			s.Sleeping = s.ID%11 == 0 && tick%13 < 4
			s.InputSequence, s.InputGeneration, s.InputActive = tick, 3, s.ID%2 == 0
		}
		frame := Frame{Epoch: 9, Tick: tick, States: states}
		spatial, err := NewQuantizedSpatialFrame(frame, 10)
		if err != nil {
			t.Fatal(err)
		}
		view := scene.Frame(states)
		grabs := map[uint64]uint64{}
		if tick%20 < 10 {
			grabs[51] = 3
			grabs[4] = 55
		}
		for k, m := range members {
			if (tick+uint64(k))%2 != 0 {
				continue
			}
			polls++
			if m.pending != nil {
				if polls%5 == 0 { // 未确认：重发同一载荷。
					digest.Write(m.payload)
					continue
				}
				if err = m.cursor.Commit(m.pending); err != nil {
					t.Fatal(err)
				}
				m.view, m.visible, m.pending = m.nextView, m.nextSeen, nil
			}
			snapshot, visible, err := spatial.Visible(m.body, 12, 2, m.visible)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := view.Plan(snapshot, m.body, grabs, m.view, DefaultReplicationPolicy())
			if err != nil {
				t.Fatal(err)
			}
			p, err := m.cursor.PlanFields(plan.Snapshot, QuantizedLayout())
			if err != nil {
				t.Fatal(err)
			}
			var own InputAck
			if state, ok := view.State(m.body); ok {
				own = InputAck{Sequence: state.InputSequence, Generation: state.InputGeneration, Active: state.InputActive}
			}
			m.pending, m.nextView, m.nextSeen = p, plan.Entries, visible
			m.payload = RoomPayload(p.Data, own)
			digest.Write(m.payload)
			for _, id := range slices.Sorted(maps.Keys(plan.Entries)) {
				e := plan.Entries[id]
				write(id, e.Tick, e.Class, e.Object.Chunk, e.Object.Data, visible[id])
			}
			for _, class := range slices.Sorted(maps.Keys(plan.Samples)) {
				write("sample", class, plan.Samples[class])
				coverage[class]++
			}
			for _, class := range slices.Sorted(maps.Keys(plan.Deferred)) {
				write("deferred", class, plan.Deferred[class])
				coverage["deferred"]++
			}
			for id := range m.visible {
				if !visible[id] {
					coverage["removed"]++
				}
			}
			for id, e := range plan.Entries {
				if old, ok := m.view[id]; ok && old.Object.Chunk != e.Object.Chunk {
					coverage["migrated"]++
				}
			}
			write("visible", len(visible))
		}
	}
	for _, need := range []string{"near", "middle", "far", "motion", "deferred", "removed", "migrated"} {
		if coverage[need] == 0 {
			t.Fatalf("scenario does not cover %s: %v", need, coverage)
		}
	}
	const want = "f862c6e98746cdb23e8e98b7bb9d5421cb824cbaea64027eb8d552d5d1d34790"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("replication bytes changed: %s", got)
	}
}
