package physics

import (
	"math"
	"math/rand/v2"
	"slices"
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

// 可见对象的输出次序决定下游组帧字节：按 Block 的 x、z 升序，同一 Block 内按对象下标。
// 用逐对象的参考实现对照，覆盖 ID 有序（物理房间原生输出）与乱序两种输入、跨负坐标的多个 Block。
func TestSpatialVisibilityKeepsBlockOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, shuffled := range []bool{false, true} {
		states := make([]physx.State, 120)
		for i := range states {
			states[i] = physx.State{ID: uint64(i + 1), Position: physx.Vec3{rng.Float32()*60 - 30, rng.Float32() * 4, rng.Float32()*60 - 30}, Rotation: physx.Quat{0, 0, 0, 1}}
		}
		if shuffled {
			rng.Shuffle(len(states), func(i, j int) { states[i], states[j] = states[j], states[i] })
		}
		spatial, err := NewSpatialFrame(Frame{Epoch: 1, Tick: 1, States: states}, 25, 10)
		if err != nil {
			t.Fatal(err)
		}
		previous := map[uint64]bool{3: true, 40: true, 77: true}
		for _, self := range states {
			view, _, err := spatial.Visible(self.ID, 12, 2, previous)
			if err != nil {
				t.Fatal(err)
			}
			var got, want []uint64
			for _, o := range view.Objects {
				got = append(got, o.ID)
			}
			block := func(v float32) int64 { return int64(math.Floor(float64(v) / 10)) }
			cx, cz := float64(self.Position[0]), float64(self.Position[2])
			for x := block(float32(cx - 14)); x <= block(float32(cx+14)); x++ {
				for z := block(float32(cz - 14)); z <= block(float32(cz+14)); z++ {
					for _, o := range states {
						if block(o.Position[0]) != x || block(o.Position[2]) != z {
							continue
						}
						limit := 12.0
						if previous[o.ID] {
							limit = 14
						}
						dx, dy, dz := float64(o.Position[0])-cx, float64(o.Position[1])-float64(self.Position[1]), float64(o.Position[2])-cz
						if o.ID == self.ID || dx*dx+dy*dy+dz*dz <= limit*limit {
							want = append(want, o.ID)
						}
					}
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("shuffled=%v self=%d: got %v want %v", shuffled, self.ID, got, want)
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
