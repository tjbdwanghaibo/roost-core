package terrain

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// walkMeta 是行走用例的小地图：3 × 2 块（192m × 128m），水位线 400m（原始值 4000）。
var walkMeta = Meta{BaseRevision: 1, ChunksX: 3, ChunksY: 2, WaterLevel: 40000}

// flatSource 是所有采样点同一高度原始值的地形。
func flatSource(raw uint16) ChunkSource {
	return func(key ChunkKey) (Chunk, error) {
		c := NewChunk()
		for i := range c.Heights {
			c.Heights[i] = raw
		}
		return c, nil
	}
}

// newWalkView 返回装满了的平地视图（高度原始值 raw）。
func newWalkView(t testing.TB, meta Meta, raw uint16) *View {
	t.Helper()
	p, _ := writeTestPack(t, meta, flatSource(raw))
	v := NewView(p)
	loadAll(t, v)
	return v
}

// setRaw 把一组世界采样点 (gx, gy) 改成给定的高度原始值：按块分组，每块装一个版本 + 1 的覆盖块（模拟一次提交）。
func setRaw(t testing.TB, v *View, edits map[[2]int64]uint16) {
	t.Helper()
	byChunk := map[ChunkKey][]int{}
	for g := range edits {
		key := ChunkKey{X: int32(g[0] / ChunkSamples), Y: int32(g[1] / ChunkSamples)}
		byChunk[key] = append(byChunk[key], 0)
	}
	for key := range byChunk {
		c, version, loaded := v.Chunk(key)
		if !loaded {
			t.Fatalf("chunk %v not loaded", key)
		}
		for g, raw := range edits {
			if (ChunkKey{X: int32(g[0] / ChunkSamples), Y: int32(g[1] / ChunkSamples)}) == key {
				c.Heights[(g[1]%ChunkSamples)*ChunkSamples+g[0]%ChunkSamples] = raw
			}
		}
		if err := v.SetOverlay(key, Overlay{BaseRevision: v.Meta().BaseRevision, Version: version + 1, Heights: c.Heights, Materials: c.Materials}); err != nil {
			t.Fatal(err)
		}
	}
}

func node(x, y int64) spatial.Point { return spatial.Point{X: x, Y: y} }

// 坡度 / 台阶 / 下落的边界：高度是 10 厘米一档，缺省规则下正交一步上 100 厘米可以、110 不行，对角 140 可以、150 不行；
// 陡于坡度上限的下行按落差：300 可以、310 不行。调低坡度后台阶起作用；MaxDrop 0 时陡降只按台阶放行。
func TestWalkSlopeStepAndDropBoundaries(t *testing.T) {
	for _, c := range []struct {
		rules WalkRules
		dh    int64
		run   int64
		want  bool
	}{
		{DefaultWalkRules, 100, runStraight, true},
		{DefaultWalkRules, 110, runStraight, false},
		{DefaultWalkRules, 140, runDiagonal, true},
		{DefaultWalkRules, 150, runDiagonal, false},
		{DefaultWalkRules, -100, runStraight, true},
		{DefaultWalkRules, -300, runStraight, true},
		{DefaultWalkRules, -310, runStraight, false},
		{WalkRules{MaxSlope: 300, MaxStep: 50, MaxDrop: 300}, 30, runStraight, true},
		{WalkRules{MaxSlope: 300, MaxStep: 50, MaxDrop: 300}, 50, runStraight, true},
		{WalkRules{MaxSlope: 300, MaxStep: 50, MaxDrop: 300}, 60, runStraight, false},
		{WalkRules{MaxSlope: 300, MaxStep: 50, MaxDrop: 300}, -60, runStraight, true},
		{WalkRules{MaxSlope: 300, MaxStep: 50}, -50, runStraight, true},
		{WalkRules{MaxSlope: 300, MaxStep: 50}, -60, runStraight, false},
	} {
		if got := c.rules.climb(c.dh, c.run); got != c.want {
			t.Errorf("%+v climb(%d over %d) = %v, want %v", c.rules, c.dh, c.run, got, c.want)
		}
	}

	// 同样的边界经过视图：(10,10) 在 500m，右边一格 +110 / +100 厘米，下边一格 −310 / −300 厘米，对角 +150 / +140。
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	setRaw(t, v, map[[2]int64]uint16{{11, 10}: 5011, {9, 10}: 5010, {10, 11}: 4969, {10, 9}: 4970, {11, 11}: 5000})
	for _, c := range []struct {
		to   spatial.Point
		want bool
	}{{node(11, 10), false}, {node(9, 10), true}, {node(10, 11), false}, {node(10, 9), true}} {
		if _, ok := w.Step(node(10, 10), c.to); ok != c.want {
			t.Errorf("Step((10,10) → %v) = %v, want %v", c.to, ok, c.want)
		}
	}
	// 对角的两个拐角各高 70 厘米（正交两步各 70 都能走），只有对角这一步本身的高差在边界上。
	setRaw(t, v, map[[2]int64]uint16{{11, 10}: 5007, {10, 11}: 5007, {11, 11}: 5015})
	if _, ok := w.Step(node(10, 10), node(11, 11)); ok {
		t.Error("a diagonal step up 150cm was allowed")
	}
	setRaw(t, v, map[[2]int64]uint16{{11, 11}: 5014})
	if _, ok := w.Step(node(10, 10), node(11, 11)); !ok {
		t.Error("a diagonal step up 140cm was refused")
	}
	if _, ok := w.Step(node(10, 10), node(12, 10)); ok {
		t.Error("a step to a node two away was allowed")
	}
}

// 跨块边界与未装入：块 (0,0) 与 (1,0) 之间的一步照常判定；邻块卸下后走不进去、站在里面的节点没有定义，查询答
// ErrNotLoaded，寻路到那里答 ErrNotLoaded（不猜）。地图边缘：最后半米归最后一个采样点，再往外走不出去。
func TestWalkAcrossChunksTheMapEdgeAndUnloadedChunks(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	if _, ok := w.Step(node(63, 10), node(64, 10)); !ok {
		t.Fatal("a flat step across the chunk border was refused")
	}
	setRaw(t, v, map[[2]int64]uint16{{64, 10}: 5020})
	if _, ok := w.Step(node(63, 10), node(64, 10)); ok {
		t.Fatal("a 2m step across the chunk border was allowed")
	}
	if _, ok := w.Step(node(64, 10), node(63, 10)); !ok {
		t.Fatal("a 2m drop across the chunk border was refused (MaxDrop 3m)")
	}

	v.Unload(ChunkKey{X: 1, Y: 0})
	if _, ok := w.Step(node(63, 11), node(64, 11)); ok {
		t.Fatal("a step into an unloaded chunk was allowed")
	}
	if _, ok := w.Step(node(64, 11), node(64, 11)); ok {
		t.Fatal("standing in an unloaded chunk is defined")
	}
	if w.Standable(spatial.Point{X: 6450, Y: 1100}) {
		t.Fatal("a point in an unloaded chunk is standable")
	}
	if _, err := w.Swimming(spatial.Point{X: 6450, Y: 1100}); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Swimming in an unloaded chunk: %v", err)
	}
	if _, err := w.FindPath(spatial.Point{X: 1000, Y: 1000}, spatial.Point{X: 7000, Y: 1000}, 0); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("a route into an unloaded chunk: %v, want ErrNotLoaded", err)
	}
	// 走到卸下的块边上就停：路线终点在块 (0,0) 里、贴着边界时照常。
	if _, err := w.FindPath(spatial.Point{X: 1000, Y: 1000}, spatial.Point{X: 6349, Y: 1000}, 0); err != nil {
		t.Fatalf("a route up to the unloaded chunk: %v", err)
	}

	edge := walkMeta.Bounds().Max.X - 1
	last := int64(walkMeta.ChunksX)*ChunkSamples - 1
	if c := w.Cell(spatial.Point{X: edge, Y: 3000}); c != node(last, 30) {
		t.Fatalf("Cell at the map edge = %v, want the last sample (%d, 30)", c, last)
	}
	if _, ok := w.Step(node(last, 30), node(last+1, 30)); ok {
		t.Fatal("a step off the map was allowed")
	}
	if c := w.Cell(spatial.Point{X: edge + 100, Y: 3000}); c.X != last+2 {
		t.Fatalf("Cell outside the map is clamped: %v", c)
	}
	if _, err := w.FindPath(spatial.Point{X: 13000, Y: 3000}, spatial.Point{X: edge + 1, Y: 3000}, 0); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("a route off the map: %v, want ErrOutOfMap", err)
	}
}

// 水：水深不超过 WadeDepth 照常走（速度 1000），更深是游泳（SwimSpeed，站立高度是水面）；上岸按“水面 → 岸上地面”
// 的高差判定；SwimSpeed 0 时深水进不去、站在里面的能走出来。
func TestWalkWaterWadeSwimAndClimbOut(t *testing.T) {
	v := newWalkView(t, walkMeta, 4010) // 401m，水位 400m 之上 1m
	w := &Walker{View: v, Rules: DefaultWalkRules}
	setRaw(t, v, map[[2]int64]uint16{
		{20, 10}: 3970, // 397m：水深 3m，游泳
		{21, 10}: 3960,
		{19, 11}: 3996, // 399.6m：水深 40cm，涉水
		{22, 10}: 4012, // 401.2m：水面之上 1.2m 的陡岸
	})
	if speed, ok := w.Step(node(19, 10), node(20, 10)); !ok || speed != 600 {
		t.Fatalf("into deep water: speed %d ok %v, want 600 true", speed, ok)
	}
	if speed, ok := w.Step(node(19, 10), node(19, 11)); !ok || speed != 1000 {
		t.Fatalf("into shallow water: speed %d ok %v, want 1000 true (wading)", speed, ok)
	}
	// 水里两个节点的地面差 1m，但都浮在水面上：没有高差。
	if speed, ok := w.Step(node(20, 10), node(21, 10)); !ok || speed != 600 {
		t.Fatalf("swimming on: speed %d ok %v", speed, ok)
	}
	if _, ok := w.Step(node(21, 10), node(22, 10)); ok {
		t.Fatal("climbed out onto a bank 1.2m above the water surface")
	}
	if _, ok := w.Step(node(20, 10), node(20, 11)); !ok {
		t.Fatal("could not climb out onto a bank 1m above the water surface")
	}
	if swim, err := w.Swimming(spatial.Point{X: 2010, Y: 990}); err != nil || !swim {
		t.Fatalf("Swimming in deep water: %v %v", swim, err)
	}
	if swim, err := w.Swimming(spatial.Point{X: 1900, Y: 1100}); err != nil || swim {
		t.Fatalf("Swimming in shallow water: %v %v", swim, err)
	}
	if z, err := w.StandZ(spatial.Point{X: 2000, Y: 1000}); err != nil || z != walkMeta.WaterLevel {
		t.Fatalf("StandZ swimming = %d %v, want the water level", z, err)
	}
	if z, err := w.StandZ(spatial.Point{X: 1000, Y: 1000}); err != nil || z != 40100 {
		t.Fatalf("StandZ on land = %d %v, want the ground 40100", z, err)
	}

	dry := &Walker{View: v, Rules: DefaultWalkRules}
	dry.Rules.SwimSpeed = 0
	if _, ok := dry.Step(node(19, 10), node(20, 10)); ok {
		t.Fatal("entered deep water with swimming disabled")
	}
	if speed, ok := dry.Step(node(20, 10), node(20, 10)); !ok || speed != 1000 {
		t.Fatalf("standing in deep water with swimming disabled: %d %v", speed, ok)
	}
	if _, ok := dry.Step(node(20, 10), node(19, 10)); !ok {
		t.Fatal("could not walk out of deep water with swimming disabled")
	}
	if dry.Standable(spatial.Point{X: 2000, Y: 1000}) || !w.Standable(spatial.Point{X: 2000, Y: 1000}) {
		t.Fatal("standable deep water: want no without swimming, yes with")
	}
}

// 负高度（HeightBase −100 米，game-demo 资源的缺省；水位 0 米）：世界高度 = HeightBase + raw × 10 厘米。规则只看站立高度
// 的差，与基准无关：同样的原始值差在负高度上给出与正高度相同的坡度 / 下落判定；基线以下的池塘是游泳节点，站立高度是
// 水面 0；寻路穿过池塘时按游泳速度计代价，结果与把整张图平移到正高度时相同。
func TestWalkBelowTheBaselineIsTheSameAsAbove(t *testing.T) {
	below := Meta{BaseRevision: 1, ChunksX: 2, ChunksY: 1, HeightBase: -10000, WaterLevel: 0}
	above := below
	above.HeightBase, above.WaterLevel = 50000, 60000 // 整张图抬高 600 米，水位跟着抬
	pond := map[[2]int64]uint16{}
	for gx := int64(20); gx <= 30; gx++ {
		for gy := int64(5); gy <= 15; gy++ {
			pond[[2]int64{gx, gy}] = 970 // −3 米：水深 3 米
		}
	}
	pond[[2]int64{19, 10}] = 996  // −40 厘米：涉水
	pond[[2]int64{40, 10}] = 1011 // 正交一步 +110 厘米：陡于坡度上限
	pond[[2]int64{40, 12}] = 1041 // 从 (40,12) 下到 (41,12)：−310 厘米，超过 MaxDrop
	pond[[2]int64{41, 12}] = 1010
	walkers := map[string]*Walker{}
	for name, meta := range map[string]Meta{"below": below, "above": above} {
		v := newWalkView(t, meta, 1000)
		setRaw(t, v, pond)
		walkers[name] = &Walker{View: v, Rules: DefaultWalkRules}
	}
	w := walkers["below"]
	if z, err := w.StandZ(spatial.Point{X: 1000, Y: 1000}); err != nil || z != 0 {
		t.Fatalf("StandZ on the baseline = %d %v, want 0", z, err)
	}
	if z, err := w.StandZ(spatial.Point{X: 2500, Y: 1000}); err != nil || z != 0 {
		t.Fatalf("StandZ swimming in a pond below the baseline = %d %v, want the water level 0", z, err)
	}
	if g, err := w.View.Height(2500, 1000); err != nil || g != -300 {
		t.Fatalf("pond floor = %d %v, want -300", g, err)
	}
	if swim, err := w.Swimming(spatial.Point{X: 1900, Y: 1000}); err != nil || swim {
		t.Fatalf("40cm below the water is wading: %v %v", swim, err)
	}
	for _, c := range []struct {
		from, to spatial.Point
		speed    int32
		ok       bool
	}{
		{node(19, 10), node(20, 10), 600, true},  // 下水游泳
		{node(18, 10), node(19, 10), 1000, true}, // 涉水
		{node(39, 10), node(40, 10), 0, false},   // +110 厘米
		{node(40, 12), node(41, 12), 0, false},   // −310 厘米
		{node(40, 10), node(39, 10), 1000, true}, // −110 厘米：陡于坡度、在 MaxDrop 内
		{node(30, 10), node(31, 10), 1000, true}, // 水面 0 上岸到地面 0
	} {
		for name, walker := range walkers {
			speed, ok := walker.Step(c.from, c.to)
			if ok != c.ok || (ok && speed != c.speed) {
				t.Errorf("%s: Step(%v → %v) = %d %v, want %d %v", name, c.from, c.to, speed, ok, c.speed, c.ok)
			}
		}
	}
	from, to := spatial.Point{X: 1500, Y: 1000}, spatial.Point{X: 3500, Y: 1000}
	got := map[string]Route{}
	for name, walker := range walkers {
		route, err := walker.FindPath(from, to, 0)
		if err != nil {
			t.Fatalf("%s: route across the pond: %v", name, err)
		}
		got[name] = route
	}
	if !slices.Equal(got["below"].Points, got["above"].Points) || got["below"].Visited != got["above"].Visited {
		t.Fatalf("the baseline changed the route:\n below %v\n above %v", got["below"].Points, got["above"].Points)
	}
}

// 对角不切角：拐角上有一面墙时对角那一步不放行，寻路绕开拐角。占位钩子与 Bounds 让节点不可进入；Bounds 边上不足
// 半米的位置归最近的可用节点。
func TestWalkCornersOccupancyAndBounds(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	if _, ok := w.Step(node(10, 10), node(11, 11)); !ok {
		t.Fatal("a flat diagonal step was refused")
	}
	setRaw(t, v, map[[2]int64]uint16{{11, 10}: 5050})
	if _, ok := w.Step(node(10, 10), node(11, 11)); ok {
		t.Fatal("a diagonal step cut the corner of a wall")
	}
	route, err := w.FindPath(spatial.Point{X: 1000, Y: 1000}, spatial.Point{X: 1100, Y: 1100}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []spatial.Point{{X: 1000, Y: 1000}, {X: 1000, Y: 1100}, {X: 1100, Y: 1100}}; !slices.Equal(route.Points, want) {
		t.Fatalf("route past a corner wall %v, want %v", route.Points, want)
	}

	occupied := &Walker{View: v, Rules: DefaultWalkRules, Occupied: func(key ChunkKey, index int32) bool {
		return key == ChunkKey{} && index == 10*ChunkSamples+12
	}}
	if _, ok := occupied.Step(node(13, 10), node(12, 10)); ok || occupied.Standable(spatial.Point{X: 1200, Y: 1000}) {
		t.Fatal("an occupied sample can be entered")
	}
	if _, ok := occupied.Step(node(12, 10), node(13, 10)); !ok {
		t.Fatal("could not leave an occupied sample")
	}

	bounded := &Walker{View: v, Rules: DefaultWalkRules, Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}}
	if c := bounded.Cell(spatial.Point{X: 990, Y: 10}); c != node(9, 0) {
		t.Fatalf("Cell near the bounds edge = %v, want (9, 0)", c)
	}
	if _, ok := bounded.Step(node(9, 5), node(10, 5)); ok {
		t.Fatal("a step out of the bounds was allowed")
	}
	if _, err := bounded.FindPath(spatial.Point{X: 100, Y: 100}, spatial.Point{X: 1500, Y: 100}, 0); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("a route out of the bounds: %v", err)
	}
}

// 平地上的路线是直线：首尾各多一个节点位置，中间每米一个节点；展开的节点数就是路线上的节点数（估价在平地上精确）。
// 同一视图状态下两次结果相同。
func TestFindPathOnFlatGroundIsStraightAndDeterministic(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	from, to := spatial.Point{X: 1050, Y: 1049}, spatial.Point{X: 5030, Y: 1040}
	route, err := w.FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []spatial.Point{from}
	for x := int64(11); x <= 50; x++ {
		want = append(want, spatial.Point{X: x * 100, Y: 1000})
	}
	want = append(want, to)
	if !slices.Equal(route.Points, want) {
		t.Fatalf("flat route %v\nwant %v", route.Points, want)
	}
	if route.Visited != 40 {
		t.Fatalf("visited %d nodes, want the 40 on the line", route.Visited)
	}
	if again, _ := w.FindPath(from, to, 0); !slices.Equal(again.Points, route.Points) || !slices.Equal(again.Chunks, route.Chunks) {
		t.Fatal("the same query gave a different route")
	}
	if want := []ChunkVersion{{Key: ChunkKey{}, Version: 0}}; !slices.Equal(route.Chunks, want) {
		t.Fatalf("route chunks %v, want %v", route.Chunks, want)
	}
}

// 寻路给出的路线，按移动的方式走（每 0.5 单位采样、四舍五入归节点、节点变了问 Step）每一步都放行：寻路与移动是同一套
// 规则。地形用起伏剧烈的程序化地形（多数地方是陡坡），起终点随机。
func TestFindPathRoutesAreWalkableStepByStep(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	loadAll(t, v)
	w := &Walker{View: v, Rules: WalkRules{MaxSlope: 3000, MaxStep: 50, MaxDrop: 600, WadeDepth: 50, SwimSpeed: 600, UphillCost: 2000}}
	rng := rand.New(rand.NewPCG(1, 2))
	b := testMeta.Bounds()
	found := 0
	for range 200 {
		from := spatial.Point{X: rng.Int64N(b.Max.X), Y: rng.Int64N(b.Max.Y)}
		to := spatial.Point{X: min(max(from.X+rng.Int64N(6000)-3000, 0), b.Max.X-1), Y: min(max(from.Y+rng.Int64N(6000)-3000, 0), b.Max.Y-1)}
		route, err := w.FindPath(from, to, 0)
		if err != nil {
			if errors.Is(err, spatial.ErrNoPath) || errors.Is(err, spatial.ErrPathBudgetExhausted) {
				continue
			}
			t.Fatal(err)
		}
		found++
		if err := walkRoute(w, route.Points); err != nil {
			t.Fatalf("route %v → %v: %v (points %v)", from, to, err, route.Points)
		}
	}
	if found < 50 {
		t.Fatalf("only %d of 200 random routes were found; the case does not exercise enough routes", found)
	}
}

// walkRoute 按 Move.advanceOn 的方式沿折线走：每 0.5 单位采样、四舍五入、节点变化时问 Step。
func walkRoute(w *Walker, points []spatial.Point) error {
	cell := w.Cell(points[0])
	if _, ok := w.Step(cell, cell); !ok {
		return fmt.Errorf("start %v undefined", points[0])
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		dx, dy := float64(b.X-a.X), float64(b.Y-a.Y)
		dist := math.Hypot(dx, dy)
		for d := 0.5; d < dist+0.5; d += 0.5 {
			d := min(d, dist)
			at := spatial.Point{X: int64(math.Round(float64(a.X) + dx/dist*d)), Y: int64(math.Round(float64(a.Y) + dy/dist*d))}
			if next := w.Cell(at); next != cell {
				if _, ok := w.Step(cell, next); !ok {
					return fmt.Errorf("segment %v → %v: step %v → %v refused at %v", a, b, cell, next, at)
				}
				cell = next
			}
		}
	}
	return nil
}

// 改块后路线失效与重算：一条路线经过块 (0,0)、(1,0)；在 (1,0) 里抬起一道墙，路线不再 Fresh，重算后绕过墙的缺口；
// 再在 (0,0) 里挖一条比 MaxDrop 深的沟，路线又失效，重算后从沟的缺口过。没经过改动块的路线仍然 Fresh。
func TestFindPathGoesAroundAWallAndADitchAfterTheChunksChange(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	from, to := spatial.Point{X: 1000, Y: 3200}, spatial.Point{X: 12000, Y: 3200}
	straight, err := w.FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := w.FindPath(spatial.Point{X: 14000, Y: 9000}, spatial.Point{X: 17000, Y: 11000}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !straight.Fresh(v) || maxDeviation(straight.Points, 3200) != 0 {
		t.Fatalf("setup: straight route %v fresh %v", straight.Points, straight.Fresh(v))
	}

	wall := map[[2]int64]uint16{}
	for gy := int64(10); gy <= 54; gy++ {
		wall[[2]int64{90, gy}] = 5050 // 5m 高的墙，y 10..54；两头留缺口
	}
	setRaw(t, v, wall)
	if straight.Fresh(v) {
		t.Fatal("a route through a chunk that got a wall is still fresh")
	}
	if !elsewhere.Fresh(v) {
		t.Fatal("a route that does not touch the edited chunk went stale")
	}
	around, err := w.FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := walkRoute(w, around.Points); err != nil {
		t.Fatalf("the rerouted path is not walkable: %v", err)
	}
	if d := maxDeviation(around.Points, 3200); d < 2300 {
		t.Fatalf("the rerouted path strays at most %d from the line; it should go round the wall's end (≥ 2300)", d)
	}
	if err := walkRoute(w, straight.Points); err == nil {
		t.Fatal("the stale straight route is still walkable through the wall")
	}

	ditch := map[[2]int64]uint16{}
	for gy := int64(0); gy <= 63; gy++ {
		if gy < 60 {
			ditch[[2]int64{30, gy}], ditch[[2]int64{31, gy}] = 4940, 4940 // 6m 深的沟（> MaxDrop 3m），y 60..63 是缺口
		}
	}
	setRaw(t, v, ditch)
	if around.Fresh(v) {
		t.Fatal("a route through a chunk that got a ditch is still fresh")
	}
	again, err := w.FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := walkRoute(w, again.Points); err != nil {
		t.Fatalf("the path round the ditch is not walkable: %v", err)
	}
	crossed := false
	for _, p := range again.Points {
		if p.X >= 3000 && p.X <= 3100 {
			crossed = true
			if p.Y < 5950 {
				t.Fatalf("the route crosses the ditch at %v, not through its gap (y ≥ 60m)", p)
			}
		}
	}
	if !crossed || !again.Fresh(v) {
		t.Fatalf("route %v crossed %v fresh %v", again.Points, crossed, again.Fresh(v))
	}
	// 沟底本身下不去（6m 落差 > MaxDrop 3m），整张图上扫满上限。
	if _, err := w.FindPath(from, spatial.Point{X: 3000, Y: 3000}, 0); !errors.Is(err, spatial.ErrPathBudgetExhausted) {
		t.Fatalf("a route into the 6m ditch: %v, want the budget exhausted", err)
	}
}

// ascent 是沿折线（每米取一次地面高度）的累计爬升。
func ascent(t testing.TB, v *View, points []spatial.Point) int64 {
	t.Helper()
	var up int64
	prev, err := v.Height(points[0].X, points[0].Y)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		n := max(abs64(b.X-a.X), abs64(b.Y-a.Y)) / 100
		for k := int64(1); k <= max(n, 1); k++ {
			h, err := v.Height(a.X+(b.X-a.X)*k/max(n, 1), a.Y+(b.Y-a.Y)*k/max(n, 1))
			if err != nil {
				t.Fatal(err)
			}
			up += max(int64(h-prev), 0)
			prev = h
		}
	}
	return up
}

func maxDeviation(points []spatial.Point, y int64) int64 {
	var d int64
	for _, p := range points {
		d = max(d, abs64(p.Y-y))
	}
	return d
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// 坡度代价：中间一座小山（每米 80cm，能走），上坡代价 2 时绕开山顶、爬升比直穿少，代价 0 时直穿。
func TestFindPathWeighsSlopes(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	hill := map[[2]int64]uint16{}
	for gx := int64(20); gx <= 40; gx++ {
		for gy := int64(22); gy <= 42; gy++ {
			d := int64(math.Round(math.Hypot(float64(gx-30), float64(gy-32))))
			if d < 8 {
				hill[[2]int64{gx, gy}] = uint16(5000 + (8-d)*8)
			}
		}
	}
	setRaw(t, v, hill)
	from, to := spatial.Point{X: 1000, Y: 3200}, spatial.Point{X: 5000, Y: 3200}
	weighted, err := (&Walker{View: v, Rules: DefaultWalkRules}).FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if maxDeviation(weighted.Points, 3200) < 300 || ascent(t, v, weighted.Points) >= ascent(t, v, []spatial.Point{from, to}) {
		t.Fatalf("with uphill cost the route climbs the hill: %v (ascent %d)", weighted.Points, ascent(t, v, weighted.Points))
	}
	flat := DefaultWalkRules
	flat.UphillCost = 0
	direct, err := (&Walker{View: v, Rules: flat}).FindPath(from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if maxDeviation(direct.Points, 3200) != 0 {
		t.Fatalf("without uphill cost the route still goes round the hill: %v", direct.Points)
	}
}

// 走不到：终点被墙围住。整张地图上搜索扫满上限答 ErrPathBudgetExhausted（可能有路），在小的 Bounds 里扫完了答
// ErrNoPath；终点不能站（不能游泳时的深水）答 ErrNoPath。
func TestFindPathBudgetAndNoPath(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	ring := map[[2]int64]uint16{}
	for d := int64(-3); d <= 3; d++ {
		for _, g := range [][2]int64{{100 + d, 37}, {100 + d, 43}, {97, 40 + d}, {103, 40 + d}} {
			ring[g] = 5100 // 10m 高的围墙，中心 (100, 40)
		}
	}
	ring[[2]int64{150, 40}] = 3900 // 深水
	setRaw(t, v, ring)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	goal := spatial.Point{X: 10000, Y: 4000}
	if _, err := w.FindPath(spatial.Point{X: 1000, Y: 4000}, goal, 0); !errors.Is(err, spatial.ErrPathBudgetExhausted) {
		t.Fatalf("an enclosed goal on the whole map: %v, want ErrPathBudgetExhausted", err)
	}
	small := &Walker{View: v, Rules: DefaultWalkRules, Bounds: spatial.Rect{Min: spatial.Point{X: 8000, Y: 2000}, Max: spatial.Point{X: 12000, Y: 6000}}}
	if _, err := small.FindPath(spatial.Point{X: 8500, Y: 4000}, goal, 0); !errors.Is(err, spatial.ErrNoPath) {
		t.Fatalf("an enclosed goal in a small area: %v, want ErrNoPath", err)
	}
	dry := &Walker{View: v, Rules: DefaultWalkRules}
	dry.Rules.SwimSpeed = 0
	if _, err := dry.FindPath(spatial.Point{X: 14000, Y: 4000}, spatial.Point{X: 15000, Y: 4000}, 0); !errors.Is(err, spatial.ErrNoPath) {
		t.Fatalf("a goal in deep water without swimming: %v, want ErrNoPath", err)
	}
	if err := (WalkRules{SwimSpeed: 1001}).Validate(); !errors.Is(err, ErrWalkRules) {
		t.Fatalf("swim speed above 1000 validated: %v", err)
	}
	if err := DefaultWalkRules.Validate(); err != nil {
		t.Fatal(err)
	}
}

// 寻路与改块并发：搜索不取锁，每块只读一次槽位；并发提交下每条路线的块版本都是某个真实提交过的版本、路线可走。
func TestFindPathWhileChunksChange(t *testing.T) {
	v := newWalkView(t, walkMeta, 5000)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			raw := uint16(5000)
			if i%2 == 0 {
				raw = 5050
			}
			setRaw(t, v, map[[2]int64]uint16{{20, 32}: raw, {20, 33}: raw, {20, 31}: raw})
		}
	}()
	for range 200 {
		route, err := w.FindPath(spatial.Point{X: 1000, Y: 3200}, spatial.Point{X: 3000, Y: 3200}, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, dep := range route.Chunks {
			if dep.Version < 0 || dep.Version > 200 {
				t.Fatalf("route read chunk %v at version %d", dep.Key, dep.Version)
			}
		}
	}
	<-done
}

// routeCost 按 FindPath 的边代价重算一条路线（Points 经 Cell 归到节点、相邻重复去掉）。
func routeCost(w *Walker, route Route) int64 {
	var nodes []spatial.Point
	for _, p := range route.Points {
		if c := w.Cell(p); len(nodes) == 0 || nodes[len(nodes)-1] != c {
			nodes = append(nodes, c)
		}
	}
	s := newSearch(w)
	var total int64
	for i := 1; i < len(nodes); i++ {
		a, b := nodes[i-1], nodes[i]
		total += s.cost(w.footingAt(nil, a.X, a.Y), w.footingAt(nil, b.X, b.Y), a.X != b.X && a.Y != b.Y)
	}
	return total
}

// 加权 A*（SearchWeight）：起伏地面上，最优搜索（1000）为证明“没有更省的路”展开的节点随坡度代价暴涨，200m 的路线超出
// 4096 的上限；缺省 1500 时同一条 200m 路线在上限内找到，代价不超过最优的 1.5 倍，展开的节点少得多。路线照样逐步可走；
// 平地上两者相同。SearchWeight 小于 1000 不合法。
func TestFindPathSearchWeightBoundsTheCostAndTheSearch(t *testing.T) {
	w := benchWalker(t)
	optimal := *w
	optimal.Rules.SearchWeight = 1000
	for _, c := range []struct {
		name     string
		from, to spatial.Point
	}{
		{"50m", spatial.Point{X: 20000, Y: 20000}, spatial.Point{X: 23500, Y: 23500}},
		{"200m", spatial.Point{X: 10000, Y: 20000}, spatial.Point{X: 30000, Y: 20500}},
	} {
		best, err := optimal.FindPath(c.from, c.to, 1<<20)
		if err != nil {
			t.Fatalf("%s optimal: %v", c.name, err)
		}
		got, err := w.FindPath(c.from, c.to, 0)
		if err != nil {
			t.Fatalf("%s with the default weight and budget: %v (the optimal search expanded %d nodes)", c.name, err, best.Visited)
		}
		bestCost, gotCost := routeCost(w, best), routeCost(w, got)
		if gotCost < bestCost || gotCost*1000 > bestCost*int64(DefaultWalkRules.SearchWeight) {
			t.Fatalf("%s: weighted cost %d, optimal %d: outside [1, 1.5] times", c.name, gotCost, bestCost)
		}
		if got.Visited >= best.Visited {
			t.Fatalf("%s: weighted search expanded %d nodes, optimal %d", c.name, got.Visited, best.Visited)
		}
		if err := walkRoute(w, got.Points); err != nil {
			t.Fatalf("%s: the weighted route does not walk: %v", c.name, err)
		}
		t.Logf("%s: optimal %d nodes cost %d; weighted %d nodes cost %d", c.name, best.Visited, bestCost, got.Visited, gotCost)
		if c.name == "200m" {
			if _, err := optimal.FindPath(c.from, c.to, 0); !errors.Is(err, spatial.ErrPathBudgetExhausted) {
				t.Fatalf("the optimal 200m search fits the 4096 budget (%v); the rolling ground no longer shows why the weight exists", err)
			}
		}
	}
	if err := (WalkRules{SearchWeight: 999}).Validate(); !errors.Is(err, ErrWalkRules) {
		t.Fatalf("search weight 999 validated: %v", err)
	}
}

// rollingSource 是基准用的起伏地形：两层正弦（周期约 125m / 40m，振幅 10m / 3m，坡度大多在 45° 以内）加 ±20cm 抖动，
// 比 Generated（小尺度很陡，多数地方走不了）更接近可玩的开放世界地面。
func rollingSource(key ChunkKey) (Chunk, error) {
	c := NewChunk()
	for j := range ChunkSamples {
		for i := range ChunkSamples {
			gx, gy := float64(int(key.X)*ChunkSamples+i), float64(int(key.Y)*ChunkSamples+j)
			h := 5000 + 100*math.Sin(gx/20)*math.Cos(gy/25) + 30*math.Sin((gx+gy)/6.5)
			c.Heights[j*ChunkSamples+i] = uint16(h) + uint16(lattice(9, 7, int64(gx), int64(gy))%5)
		}
	}
	return c, nil
}

func benchWalker(tb testing.TB) *Walker {
	meta := Meta{BaseRevision: 1, ChunksX: 8, ChunksY: 8, WaterLevel: 49500}
	p, _ := writeTestPack(tb, meta, rollingSource)
	v := NewView(p)
	loadAll(tb, v)
	return &Walker{View: v, Rules: DefaultWalkRules}
}

// BenchmarkWalkerStep 是移动的单步判定（一次正交 Step）。
func BenchmarkWalkerStep(b *testing.B) {
	w := benchWalker(b)
	b.ResetTimer()
	for i := range b.N {
		gx := int64(100 + i%200)
		w.Step(node(gx, 150), node(gx+1, 150))
	}
}

// BenchmarkWalkerFindPath 是起伏地面上典型 50m / 200m 路线（缺省规则；optimal 是 SearchWeight 1000 的最优搜索，
// 200m 时超出上限所以不列）、平地 200m，与扫满 4096 个节点的最坏情况（终点被围住）。
func BenchmarkWalkerFindPath(b *testing.B) {
	w := benchWalker(b)
	optimal := *w
	optimal.Rules.SearchWeight = 1000
	flat := &Walker{View: newWalkView(b, Meta{BaseRevision: 1, ChunksX: 8, ChunksY: 8, WaterLevel: 0}, 5000), Rules: DefaultWalkRules}
	for _, c := range []struct {
		name     string
		w        *Walker
		from, to spatial.Point
	}{
		{"50m", w, spatial.Point{X: 20000, Y: 20000}, spatial.Point{X: 23500, Y: 23500}},
		{"50m-optimal", &optimal, spatial.Point{X: 20000, Y: 20000}, spatial.Point{X: 23500, Y: 23500}},
		{"200m", w, spatial.Point{X: 10000, Y: 20000}, spatial.Point{X: 30000, Y: 20500}},
		{"200m-flat", flat, spatial.Point{X: 10000, Y: 20000}, spatial.Point{X: 30000, Y: 20500}},
	} {
		b.Run(c.name, func(b *testing.B) {
			w := c.w
			var route Route
			for range b.N {
				var err error
				if route, err = w.FindPath(c.from, c.to, 0); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(route.Visited), "visited")
			b.ReportMetric(float64(len(route.Points)), "points")
		})
	}
	b.Run("exhausted4096", func(b *testing.B) {
		ring := map[[2]int64]uint16{}
		for d := int64(-3); d <= 3; d++ {
			for _, g := range [][2]int64{{300 + d, 297}, {300 + d, 303}, {297, 300 + d}, {303, 300 + d}} {
				ring[g] = 9000
			}
		}
		setRaw(b, w.View, ring)
		from, to := spatial.Point{X: 26000, Y: 30000}, spatial.Point{X: 30000, Y: 30000}
		b.ResetTimer()
		for range b.N {
			if _, err := w.FindPath(from, to, 0); !errors.Is(err, spatial.ErrPathBudgetExhausted) {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkGridFindPath 是对照：现有的 spatial.FindPath（四方向、单位代价、GridTerrain），同样的格数（1 格 = 1m 节点）。
func BenchmarkGridFindPath(b *testing.B) {
	grid, err := spatial.NewGridTerrain(512, 512)
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		from, to spatial.Point
	}{
		{"50m", spatial.Point{X: 200, Y: 200}, spatial.Point{X: 235, Y: 235}},
		{"200m", spatial.Point{X: 100, Y: 200}, spatial.Point{X: 300, Y: 205}},
	} {
		b.Run(c.name, func(b *testing.B) {
			for range b.N {
				if _, err := spatial.FindPath(grid, c.from, c.to, spatial.PathOptions{MaxVisited: 4096}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("exhausted4096", func(b *testing.B) {
		for d := int64(-3); d <= 3; d++ {
			for _, p := range []spatial.Point{{X: 300 + d, Y: 297}, {X: 300 + d, Y: 303}, {X: 297, Y: 300 + d}, {X: 303, Y: 300 + d}} {
				_ = grid.SetBlocked(p, true)
			}
		}
		for range b.N {
			if _, err := spatial.FindPath(grid, spatial.Point{X: 260, Y: 300}, spatial.Point{X: 300, Y: 300}, spatial.PathOptions{MaxVisited: 4096}); !errors.Is(err, spatial.ErrPathBudgetExhausted) {
				b.Fatal(err)
			}
		}
	})
}
