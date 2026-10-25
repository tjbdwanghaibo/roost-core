package physics

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func TestSpatialVisibilityEnterExitReenterAndBlocks(t *testing.T) {
	var cursor chunksync.Cursor
	var replica chunksync.Replica
	var visible map[uint64]bool
	// 穿过 x=0 的协议 Chunk 边界和 x=-10/10 的 AOI Block 边界。
	steps := []struct {
		self, other float32
		want        bool
	}{
		{-1, 10, true}, {-1, 12, true}, {-1, 14, false}, {-1, 12, false}, {1, 12, true}, {21, 12, true}, {31, 12, false},
	}
	for i, step := range steps {
		frame := Frame{Epoch: 1, Tick: uint64(i + 1), States: []physx.State{
			{ID: 1, Position: physx.Vec3{step.self, 1, 0}, Rotation: physx.Quat{0, 0, 0, 1}},
			{ID: 2, Position: physx.Vec3{step.other, 1, 0}, Rotation: physx.Quat{0, 0, 0, 1}},
		}}
		spatial, err := NewSpatialFrame(frame, 25, 10)
		if err != nil {
			t.Fatal(err)
		}
		view, next, err := spatial.Visible(1, 12, 2, visible)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := cursor.Plan(view)
		if err != nil {
			t.Fatal(err)
		}
		if err = replica.Apply(plan.Data); err != nil {
			t.Fatal(err)
		}
		if err = cursor.Commit(plan); err != nil {
			t.Fatal(err)
		}
		visible = next
		_, other := replica.Objects[2]
		if _, self := replica.Objects[1]; !self || other != step.want {
			t.Fatalf("step %d: objects=%v", i, replica.Objects)
		}
	}
}

func TestSpatialVisibilityMatchesBruteForce(t *testing.T) {
	frame := InitialFrame(1, CrowdScene())
	spatial, err := NewSpatialFrame(frame, 25, 10)
	if err != nil {
		t.Fatal(err)
	}
	previous := map[uint64]bool{1: true, 2: true, CrowdBallID: true}
	for _, self := range frame.States {
		_, got, err := spatial.Visible(self.ID, 12, 2, previous)
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range frame.States {
			limit := float64(12)
			if previous[other.ID] {
				limit = 14
			}
			dx, dz := float64(other.Position[0])-float64(self.Position[0]), float64(other.Position[2])-float64(self.Position[2])
			dy := float64(other.Position[1]) - float64(self.Position[1])
			want := self.ID == other.ID || dx*dx+dz*dz+dy*dy <= limit*limit
			if got[other.ID] != want {
				t.Fatalf("self=%d other=%d: selected=%v want=%v", self.ID, other.ID, got[other.ID], want)
			}
		}
	}
}

func TestSpatialVisibilityBoundsConfiguration(t *testing.T) {
	frame := InitialFrame(1, CrowdScene())
	if _, err := NewSpatialFrame(frame, 25, .001); err == nil {
		t.Fatal("tiny blocks accepted")
	}
	spatial, err := NewSpatialFrame(frame, 25, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = spatial.Visible(51, 1000, 0, nil); err == nil {
		t.Fatal("unbounded empty block scan accepted")
	}
	if _, _, err = spatial.Visible(999, 12, 2, nil); err == nil {
		t.Fatal("missing observer accepted")
	}
}

func TestSpatialVisibilityAccountsForHeight(t *testing.T) {
	frame := Frame{Epoch: 1, Tick: 1, States: []physx.State{
		{ID: 1, Position: physx.Vec3{0, 1, 0}, Rotation: physx.Quat{0, 0, 0, 1}},
		{ID: 2, Position: physx.Vec3{0, 16, 0}, Rotation: physx.Quat{0, 0, 0, 1}},
		{ID: 3, Position: physx.Vec3{0, 14, 0}, Rotation: physx.Quat{0, 0, 0, 1}},
	}}
	spatial, err := NewSpatialFrame(frame, 25, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := spatial.Visible(1, 12, 2, map[uint64]bool{2: true, 3: true})
	if err != nil {
		t.Fatal(err)
	}
	if got[2] || !got[3] || !got[1] {
		t.Fatalf("3D hysteresis: %v", got)
	}
}
