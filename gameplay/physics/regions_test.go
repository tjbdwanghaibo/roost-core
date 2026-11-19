package physics

import (
	"maps"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func regionFrame(positions map[uint64]physx.Vec3) *ReplicationFrame {
	states := make([]physx.State, 0, len(positions))
	for id, p := range positions {
		states = append(states, physx.State{ID: id, Position: p, Rotation: physx.Quat{0, 0, 0, 1}})
	}
	return NewReplicationScene(nil, nil).Frame(states) // map 次序随机：顺带验证结果不依赖输入次序。
}

// 进入、带滞回的离开、高速穿过薄区域、传送只判终点、有向区域、关注集合与事实次序。
func TestRegionTrackerEntersLeavesAndCatchesFastCrossings(t *testing.T) {
	// gate 是一道沿 z 很薄的门：x、y 各 ±1，z ±0.5，中心 (10, 1, 0)。
	gate := Region{ID: "gate", Center: physx.Vec3{10, 1, 0}, HalfExtents: physx.Vec3{1, 1, .5}}
	tracker := NewRegionTracker([]Region{gate}, []uint64{1})
	var inside RegionOccupancy
	at := func(z float32) *ReplicationFrame { return regionFrame(map[uint64]physx.Vec3{1: {10, 1, z}}) }
	enter, leave := RegionFact{Region: "gate", Actor: 1, Enter: true}, RegionFact{Region: "gate", Actor: 1}
	previous := at(-3)
	// 一步一步走：每一步给出本帧位置、是否被传送、应有的事实。
	for i, step := range []struct {
		name     string
		z        float32
		teleport bool
		want     []RegionFact
		inside   bool // 这一步之后是否算在区域里（带滞回，不能只看位置）
	}{
		{"approach, still outside", -1, false, nil, false},
		{"on the face counts as inside", -.5, false, []RegionFact{enter}, true},
		{"deep inside, no repeat", 0, false, nil, true},
		{"just outside the box but within the exit margin", .5 + RegionExitMargin, false, nil, true},
		{"back inside, still no repeat", .25, false, nil, true},
		{"beyond the exit margin", .875, false, []RegionFact{leave}, false},
		{"inside the margin from outside is not an entry", .625, false, nil, false},
		{"out again", 3, false, nil, false},
		{"through the whole gate in one tick", -3, false, []RegionFact{enter, leave}, false},
		{"and back, again caught", 3, false, []RegionFact{enter, leave}, false},
		{"teleported across: displacement is not a path", -3, true, nil, false},
		{"teleported into the region: end point decides", 0, true, []RegionFact{enter}, true},
		{"teleported out of it", 30, true, []RegionFact{leave}, false},
		{"moving along outside", 20, false, nil, false},
	} {
		current := at(step.z)
		before := maps.Clone(inside)
		teleported := map[uint64]bool{}
		if step.teleport {
			teleported[1] = true
		}
		facts, next := tracker.Step(inside, previous, current, teleported)
		if !reflect.DeepEqual(facts, step.want) {
			t.Fatalf("step %d (%s): facts %+v, want %+v", i, step.name, facts, step.want)
		}
		if !maps.Equal(inside, before) {
			t.Fatalf("step %d (%s): Step modified the occupancy it was given", i, step.name)
		}
		_, in := next[RegionActor{Region: 0, Actor: 1}]
		if in != step.inside {
			t.Fatalf("step %d (%s): occupancy %v, want %v", i, step.name, in, step.inside)
		}
		inside, previous = next, current
	}

	// 线段只有真的穿过盒子才算：从门外侧面掠过、或者与门平行地走，都不算。
	for name, path := range map[string][2]physx.Vec3{
		"past the side":              {{12, 1, -3}, {12, 1, 3}},
		"over the top":               {{10, 2.5, -3}, {10, 2.5, 3}},
		"parallel in front":          {{7, 1, -1}, {13, 1, -1}},
		"cutting the corner outside": {{11.5, 1, -1.5}, {12.5, 1, .5}},
		"stopping short":             {{10, 1, -3}, {10, 1, -.75}},
	} {
		facts, next := tracker.Step(nil, regionFrame(map[uint64]physx.Vec3{1: path[0]}), regionFrame(map[uint64]physx.Vec3{1: path[1]}), nil)
		if len(facts) != 0 || len(next) != 0 {
			t.Fatalf("%s: %+v", name, facts)
		}
	}
	// 斜着穿过也算，不要求沿某个轴。
	if facts, _ := tracker.Step(nil, regionFrame(map[uint64]physx.Vec3{1: {8, 0, -2}}), regionFrame(map[uint64]physx.Vec3{1: {12, 2, 2}}), nil); !reflect.DeepEqual(facts, []RegionFact{enter, leave}) {
		t.Fatalf("diagonal crossing: %+v", facts)
	}
	// 没有上一帧（或上一帧里没有这个刚体）时只按终点判定。
	if facts, _ := tracker.Step(nil, nil, at(0), nil); !reflect.DeepEqual(facts, []RegionFact{enter}) {
		t.Fatalf("no previous frame: %+v", facts)
	}
	if facts, _ := tracker.Step(nil, regionFrame(nil), at(3), nil); len(facts) != 0 {
		t.Fatalf("actor missing from the previous frame: %+v", facts)
	}
	// 本帧里没有的刚体跳过，占用集合原样返回。
	occupied := RegionOccupancy{{Region: 0, Actor: 1}: {}}
	if facts, next := tracker.Step(occupied, at(0), regionFrame(nil), nil); len(facts) != 0 || !maps.Equal(next, occupied) {
		t.Fatalf("actor missing from the current frame: %+v %+v", facts, next)
	}
}

func TestRegionTrackerRotationWatchListsAndOrder(t *testing.T) {
	// 绕 y 轴转 45° 的长条：局部 x 方向长 4、z 方向宽 0.5。世界坐标里它沿 (1,0,-1) 方向伸展。
	slanted := Region{ID: "slanted", Center: physx.Vec3{0, 0, 0}, HalfExtents: physx.Vec3{4, 1, .5}, Rotation: physx.Quat{0, .38268343, 0, .92387953}}
	tracker := NewRegionTracker([]Region{slanted}, []uint64{1})
	for name, c := range map[string]struct {
		p    physx.Vec3
		want bool
	}{
		"far along the rotated axis":             {physx.Vec3{2.5, 0, -2.5}, true},
		"same distance along the other diagonal": {physx.Vec3{2.5, 0, 2.5}, false},
		"inside the unrotated box only":          {physx.Vec3{3.5, 0, 0}, false},
		"centre":                                 {physx.Vec3{0, 0, 0}, true},
		"above":                                  {physx.Vec3{0, 1.5, 0}, false},
	} {
		facts, _ := tracker.Step(nil, nil, regionFrame(map[uint64]physx.Vec3{1: c.p}), nil)
		if (len(facts) == 1) != c.want {
			t.Fatalf("%s: %+v", name, facts)
		}
	}
	// 穿过斜放的长条：沿另一条对角线走，起点终点都在外面。
	if facts, _ := tracker.Step(nil, regionFrame(map[uint64]physx.Vec3{1: {-2, 0, -2}}), regionFrame(map[uint64]physx.Vec3{1: {2, 0, 2}}), nil); len(facts) != 2 || !facts[0].Enter || facts[1].Enter {
		t.Fatalf("crossing a rotated region: %+v", facts)
	}

	// 关注集合：Watch 为空的区域看全部玩家；给了 Watch 的只看名单里的刚体。事实次序是区域顺序、再按刚体 ID。
	regions := []Region{
		{ID: "b-players", HalfExtents: physx.Vec3{1, 1, 1}},
		{ID: "a-crates", HalfExtents: physx.Vec3{1, 1, 1}, Watch: []uint64{9, 3, 7}},
	}
	players := []uint64{5, 2, 4}
	watch := NewRegionTracker(regions, players)
	everyoneIn := map[uint64]physx.Vec3{2: {}, 3: {}, 4: {}, 5: {}, 7: {}, 9: {}, 11: {}}
	want := []RegionFact{
		{Region: "b-players", Actor: 2, Enter: true}, {Region: "b-players", Actor: 4, Enter: true}, {Region: "b-players", Actor: 5, Enter: true},
		{Region: "a-crates", Actor: 3, Enter: true}, {Region: "a-crates", Actor: 7, Enter: true}, {Region: "a-crates", Actor: 9, Enter: true},
	}
	var inside RegionOccupancy
	for range 20 { // regionFrame 每次按随机次序给状态
		facts, next := watch.Step(nil, nil, regionFrame(everyoneIn), nil)
		if !reflect.DeepEqual(facts, want) {
			t.Fatalf("facts or order: %+v", facts)
		}
		inside = next
	}
	if len(inside) != 6 {
		t.Fatalf("occupancy: %+v", inside)
	}
	// 没有变化时返回的就是传入的那份集合，不复制。
	facts, same := watch.Step(inside, regionFrame(everyoneIn), regionFrame(everyoneIn), nil)
	if len(facts) != 0 || reflect.ValueOf(same).Pointer() != reflect.ValueOf(inside).Pointer() {
		t.Fatal("unchanged occupancy was copied or reported")
	}
	// 全部离开：次序同样固定；同一刚体在两个区域里各算各的。
	far := map[uint64]physx.Vec3{}
	for id := range everyoneIn {
		far[id] = physx.Vec3{50, 0, 0}
	}
	facts, next := watch.Step(inside, regionFrame(everyoneIn), regionFrame(far), nil)
	for i := range want {
		want[i].Enter = false
	}
	if !reflect.DeepEqual(facts, want) || len(next) != 0 || len(inside) != 6 {
		t.Fatalf("leave facts or order: %+v (%d left, %d in the old set)", facts, len(next), len(inside))
	}
	// 追踪器不保留调用方的切片：建好后改关卡数据不影响判定。
	regions[0].HalfExtents = physx.Vec3{.1, .1, .1}
	regions[1].Watch[0] = 11
	players[0] = 11
	if facts, _ := watch.Step(nil, nil, regionFrame(map[uint64]physx.Vec3{5: {.5, 0, 0}, 9: {.5, 0, 0}, 11: {}}), nil); len(facts) != 2 || facts[0].Actor != 5 || facts[1].Actor != 9 {
		t.Fatalf("tracker aliases level data: %+v", facts)
	}
}
