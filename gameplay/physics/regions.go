package physics

import (
	"maps"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// RegionExitMargin 是离开判定的滞回：进入按区域本身判，离开要走出向外扩 0.25m 的范围，
// 站在边界上的刚体不会每帧抖出一对进入 / 离开。
const RegionExitMargin = 0.25

// RegionFact 是一次进入或离开，由已提交的物理帧推出，交给关卡流程当事实用。
type RegionFact struct {
	Region string
	Actor  uint64
	Enter  bool
}

// RegionActor 是“某个刚体在某个区域里”；Region 是区域在关卡里的下标。
type RegionActor struct {
	Region int
	Actor  uint64
}

// RegionOccupancy 是当前在各区域里的刚体集合。它由 Step 产出、原样交回下一次 Step；
// 调用方只保存，不修改（Step 没有变化时返回的就是传入的那一份）。
type RegionOccupancy map[RegionActor]struct{}

// RegionTracker 持有建房时固定的区域与各自关注的刚体，自身没有可变状态：
// 占用集合由调用方保存，这样物理房间可以只在帧提交成功后才采纳新的集合。
type RegionTracker struct {
	regions []trackedRegion
}
type trackedRegion struct {
	id      string
	center  [3]float64
	half    [3]float64
	inverse [4]float64 // 区域旋转的共轭：把世界坐标里的偏移转到区域局部坐标。
	watch   []uint64   // 按 ID 升序。
}

// NewRegionTracker 复制所需的值，不保留调用方切片。Watch 为空的区域关注全部 players。
// 入参应已通过 ValidateRegions。
func NewRegionTracker(regions []Region, players []uint64) *RegionTracker {
	everyone := slices.Clone(players)
	slices.Sort(everyone)
	t := &RegionTracker{regions: make([]trackedRegion, 0, len(regions))}
	for _, r := range regions {
		q := r.Rotation
		if q == (physx.Quat{}) {
			q = physx.Quat{0, 0, 0, 1}
		}
		region := trackedRegion{id: r.ID, inverse: [4]float64{-float64(q[0]), -float64(q[1]), -float64(q[2]), float64(q[3])}, watch: everyone}
		for i := range 3 {
			region.center[i] = float64(r.Center[i])
			region.half[i] = float64(r.HalfExtents[i])
		}
		if len(r.Watch) > 0 {
			region.watch = slices.Clone(r.Watch)
			slices.Sort(region.watch)
		}
		t.regions = append(t.regions, region)
	}
	return t
}

// Step 比较上一个已提交帧与本帧，给出本帧的进入 / 离开事实和新的占用集合。纯函数：不改任何入参。
//
// 规则：不在区域里的刚体，本帧中心在区域内算进入；在区域里的刚体，本帧中心走出外扩 RegionExitMargin 的范围
// 才算离开；两帧都在区域外、但两帧中心的连线穿过区域的，依次给进入、离开（高速穿过薄区域不会漏掉）。
// teleported 里的刚体本帧被传送过，位移不是运动轨迹，只按终点判定，不做连线。
// 事实的次序固定：区域按关卡里的顺序，刚体按 ID 升序。本帧里查不到的刚体跳过；previous 为 nil 或查不到时只按终点判定。
func (t *RegionTracker) Step(inside RegionOccupancy, previous, current *ReplicationFrame, teleported map[uint64]bool) ([]RegionFact, RegionOccupancy) {
	var facts []RegionFact
	next, copied := inside, false
	change := func(key RegionActor, enter bool) {
		if !copied { // 第一次改动前复制，传入的集合保持原样。
			next, copied = maps.Clone(inside), true
			if next == nil {
				next = make(RegionOccupancy)
			}
		}
		if enter {
			next[key] = struct{}{}
		} else {
			delete(next, key)
		}
	}
	for index, region := range t.regions {
		for _, actor := range region.watch {
			now, ok := current.State(actor)
			if !ok {
				continue
			}
			key := RegionActor{Region: index, Actor: actor}
			here := region.local(now.Position)
			if _, was := inside[key]; was {
				if !contains(here, region.half, RegionExitMargin) {
					change(key, false)
					facts = append(facts, RegionFact{Region: region.id, Actor: actor})
				}
				continue
			}
			if contains(here, region.half, 0) {
				change(key, true)
				facts = append(facts, RegionFact{Region: region.id, Actor: actor, Enter: true})
				continue
			}
			if previous == nil || teleported[actor] {
				continue
			}
			before, ok := previous.State(actor)
			if !ok {
				continue
			}
			if crosses(region.local(before.Position), here, region.half) {
				// 穿过去了：本帧结束时不在里面，占用集合不变，只给一对事实。
				facts = append(facts, RegionFact{Region: region.id, Actor: actor, Enter: true}, RegionFact{Region: region.id, Actor: actor})
			}
		}
	}
	return facts, next
}

// local 把世界坐标点转成以区域中心为原点、沿区域自身轴向的坐标。
func (r *trackedRegion) local(p physx.Vec3) [3]float64 {
	v := [3]float64{float64(p[0]) - r.center[0], float64(p[1]) - r.center[1], float64(p[2]) - r.center[2]}
	// 用单位四元数 q 旋转向量：v' = v + 2w(u×v) + 2u×(u×v)，u 是 q 的向量部分。
	u, w := [3]float64{r.inverse[0], r.inverse[1], r.inverse[2]}, r.inverse[3]
	uv := cross(u, v)
	uuv := cross(u, uv)
	return [3]float64{v[0] + 2*(w*uv[0]+uuv[0]), v[1] + 2*(w*uv[1]+uuv[1]), v[2] + 2*(w*uv[2]+uuv[2])}
}
func cross(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}
func contains(p, half [3]float64, margin float64) bool {
	for i := range 3 {
		if math.Abs(p[i]) > half[i]+margin {
			return false
		}
	}
	return true
}

// crosses 判断局部坐标里的线段 a→b 是否与轴对齐盒 [-half, half] 相交（逐轴收紧参数区间）。
func crosses(a, b, half [3]float64) bool {
	enter, leave := 0.0, 1.0
	for i := range 3 {
		d := b[i] - a[i]
		if d == 0 {
			if math.Abs(a[i]) > half[i] {
				return false
			}
			continue
		}
		t1, t2 := (-half[i]-a[i])/d, (half[i]-a[i])/d
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		enter, leave = math.Max(enter, t1), math.Min(leave, t2)
		if enter > leave {
			return false
		}
	}
	return true
}
