package terrain

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// globalRaw 把生成器的整张小地图展开成世界采样数组 [gy][gx]，作为查询的参照（不经过 View 的块表）。
func globalRaw(t testing.TB, meta Meta, src ChunkSource) [][]uint16 {
	t.Helper()
	w, h := int(meta.ChunksX)*ChunkSamples, int(meta.ChunksY)*ChunkSamples
	out := make([][]uint16, h)
	for gy := range out {
		out[gy] = make([]uint16, w)
	}
	for key := range ChunksIn(meta.Bounds()) {
		c, err := src(key)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range c.Heights {
			out[int(key.Y)*ChunkSamples+k/ChunkSamples][int(key.X)*ChunkSamples+k%ChunkSamples] = v
		}
	}
	return out
}

// refHeight 按 View.Height 文档里的公式直接在世界采样数组上算（边缘复制），与块表无关。
func refHeight(meta Meta, raw [][]uint16, x, y int64) int32 {
	gx, gy, fx, fy := x/SampleSpacing, y/SampleSpacing, x%SampleSpacing, y%SampleSpacing
	gx1, gy1 := min(gx+1, int64(len(raw[0])-1)), min(gy+1, int64(len(raw)-1))
	at := func(x, y int64) int64 { return int64(raw[y][x]) }
	num := (at(gx, gy)*(100-fx)+at(gx1, gy)*fx)*(100-fy) + (at(gx, gy1)*(100-fx)+at(gx1, gy1)*fx)*fy
	return meta.HeightBase + int32((num*10+5000)/10000)
}

func loadAll(t testing.TB, v *View) {
	t.Helper()
	for key := range ChunksIn(v.Meta().Bounds()) {
		if err := v.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
}

// flatOverlay 返回所有点同一高度原始值与材质的覆盖块。
func flatOverlay(meta Meta, version int64, raw uint16, material byte) Overlay {
	c := NewChunk()
	for k := range c.Heights {
		c.Heights[k], c.Materials[k] = raw, material
	}
	return Overlay{BaseRevision: meta.BaseRevision, Version: version, Heights: c.Heights, Materials: c.Materials}
}

// 双线性插值的一个手算例子：四个采样 100 / 200 / 300 / 500，位置在格内 (25, 75)。
// 原始值插值 293.75 → 2937.5cm → 四舍五入 2938cm；采样点正上方等于该点。
func TestHeightInterpolationByHand(t *testing.T) {
	p, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	v := NewView(p)
	o := flatOverlay(testMeta, 1, 0, 0)
	set := func(i, j int, raw uint16) { o.Heights[j*ChunkSamples+i] = raw }
	set(5, 5, 100)
	set(6, 5, 200)
	set(5, 6, 300)
	set(6, 6, 500)
	if err := v.SetOverlay(ChunkKey{}, o); err != nil {
		t.Fatal(err)
	}
	base := testMeta.HeightBase
	for _, c := range []struct {
		x, y int64
		want int32
	}{
		{525, 575, base + 2938},
		{500, 500, base + 1000}, // 正好在采样点 (5,5)
		{600, 600, base + 5000},
		{550, 500, base + 1500}, // 沿 X 的中点
		{500, 550, base + 2000}, // 沿 Y 的中点
		{599, 599, base + 4950}, // num = (100·1 + 200·99)·1 + (300·1 + 500·99)·99 = 4950100，(4950100·10 + 5000) / 10000 = 4950
	} {
		got, err := v.Height(c.x, c.y)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("Height(%d, %d) = %d, want %d", c.x, c.y, got, c.want)
		}
	}
}

// 全部块装入时，块内、块边界两侧、四块交点、地图边缘的查询都与参照公式一致。
func TestHeightMatchesReferenceAcrossChunks(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	loadAll(t, v)
	raw := globalRaw(t, testMeta, src)
	b := testMeta.Bounds()
	var xs []int64
	for _, edge := range []int64{0, ChunkSize, 2 * ChunkSize, b.Max.X} {
		for _, d := range []int64{-101, -100, -99, -50, -1, 0, 1, 50, 99, 100} {
			if x := edge + d; x >= 0 && x < b.Max.X {
				xs = append(xs, x)
			}
		}
	}
	checked := 0
	for _, x := range xs {
		for _, y := range xs {
			if y >= b.Max.Y {
				continue
			}
			got, err := v.Height(x, y)
			if err != nil {
				t.Fatal(err)
			}
			if want := refHeight(testMeta, raw, x, y); got != want {
				t.Fatalf("Height(%d, %d) = %d, want %d", x, y, got, want)
			}
			key, i := PointIndex(x, y)
			c, _ := src(key)
			if m, _ := v.Material(x, y); m != c.Materials[i] {
				t.Fatalf("Material(%d, %d) = %d, want %d", x, y, m, c.Materials[i])
			}
			checked++
		}
	}
	if checked < 300 {
		t.Fatalf("only %d points checked", checked)
	}
}

// 邻块的规则：权重非零的邻点所在块没装入 → ErrNotLoaded；权重为零的不读；材质只看本块。
func TestNeighbourChunkRequirement(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	raw := globalRaw(t, testMeta, src)
	if err := v.LoadBase(ChunkKey{0, 0}); err != nil {
		t.Fatal(err)
	}
	notLoaded := func(x, y int64, want ChunkKey) {
		t.Helper()
		_, err := v.Height(x, y)
		if !errors.Is(err, ErrNotLoaded) || err.Error() != fmt.Sprintf("%v: chunk %v", ErrNotLoaded, want) {
			t.Fatalf("Height(%d, %d) = %v, want ErrNotLoaded for %v", x, y, err, want)
		}
		if _, err := v.Probe(x, y); !errors.Is(err, ErrNotLoaded) {
			t.Fatalf("Probe(%d, %d) = %v", x, y, err)
		}
		if _, err := v.IsWater(x, y); !errors.Is(err, ErrNotLoaded) {
			t.Fatalf("IsWater(%d, %d) = %v", x, y, err)
		}
	}
	ok := func(x, y int64) {
		t.Helper()
		got, err := v.Height(x, y)
		if err != nil {
			t.Fatalf("Height(%d, %d): %v", x, y, err)
		}
		if want := refHeight(testMeta, raw, x, y); got != want {
			t.Fatalf("Height(%d, %d) = %d, want %d", x, y, got, want)
		}
	}
	ok(3000, 3000)
	ok(6300, 6300)                       // 第 63 列 / 行的采样点正上方：fx = fy = 0，不读邻块
	ok(6300, 1234)                       // fx = 0：不读右块
	notLoaded(6301, 100, ChunkKey{1, 0}) // 最后一米，右邻点在右块
	notLoaded(100, 6399, ChunkKey{0, 1})
	if m, err := v.Material(6399, 6399); err != nil { // 材质只看左下角采样点，仍在本块
		t.Fatalf("Material at the chunk's last metre: %v", err)
	} else if c, _ := src(ChunkKey{}); m != c.Materials[ChunkPoints-1] {
		t.Fatalf("material %d", m)
	}
	if _, err := v.Material(6400, 0); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("Material on the right chunk's first column: %v", err)
	}
	for _, k := range []ChunkKey{{1, 0}, {0, 1}} {
		if err := v.LoadBase(k); err != nil {
			t.Fatal(err)
		}
	}
	ok(6301, 100)
	ok(100, 6399)
	notLoaded(6350, 6350, ChunkKey{1, 1}) // 四块交点附近要右上块
	if err := v.LoadBase(ChunkKey{1, 1}); err != nil {
		t.Fatal(err)
	}
	ok(6350, 6350)
	if !v.Unload(ChunkKey{1, 0}) || v.Unload(ChunkKey{1, 0}) {
		t.Fatal("Unload reports the wrong previous state")
	}
	notLoaded(6301, 100, ChunkKey{1, 0})
}

// 地图边缘：最后一米的邻点越出地图时用边上的采样点（边缘复制），不要求地图外的数据；地图外报 ErrOutOfMap。
func TestMapEdge(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	loadAll(t, v)
	raw := globalRaw(t, testMeta, src)
	b := testMeta.Bounds()
	last := func(row []uint16) int64 { return int64(row[len(row)-1]) }
	// 右上角最后一米：四个点都是最后一个采样点。
	got, err := v.Height(b.Max.X-1, b.Max.Y-1)
	if err != nil {
		t.Fatal(err)
	}
	if want := testMeta.HeightBase + int32(last(raw[len(raw)-1]))*HeightStep; got != want {
		t.Fatalf("top-right corner %d, want %d", got, want)
	}
	for _, pt := range [][2]int64{{b.Max.X - 1, 50}, {50, b.Max.Y - 1}, {b.Max.X - 50, b.Max.Y - 50}} {
		got, err := v.Height(pt[0], pt[1])
		if err != nil {
			t.Fatal(err)
		}
		if want := refHeight(testMeta, raw, pt[0], pt[1]); got != want {
			t.Fatalf("Height%v = %d, want %d", pt, got, want)
		}
	}
	for _, pt := range [][2]int64{{-1, 0}, {0, -1}, {b.Max.X, 0}, {0, b.Max.Y}, {-ChunkSize * 100, 5}} {
		if _, err := v.Height(pt[0], pt[1]); !errors.Is(err, ErrOutOfMap) {
			t.Errorf("Height%v = %v, want ErrOutOfMap", pt, err)
		}
		if _, err := v.Material(pt[0], pt[1]); !errors.Is(err, ErrOutOfMap) {
			t.Errorf("Material%v = %v", pt, err)
		}
		if _, err := v.Probe(pt[0], pt[1]); !errors.Is(err, ErrOutOfMap) {
			t.Errorf("Probe%v = %v", pt, err)
		}
	}
}

// 水位线：地面低于水位线才是水，等于不是。
func TestIsWaterAgainstWaterLevel(t *testing.T) {
	p, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	v := NewView(p)
	levelRaw := uint16((testMeta.WaterLevel - testMeta.HeightBase) / HeightStep)
	o := flatOverlay(testMeta, 1, levelRaw, 9)
	o.Heights[0] = levelRaw - 1
	if err := v.SetOverlay(ChunkKey{}, o); err != nil {
		t.Fatal(err)
	}
	if w, _ := v.IsWater(0, 0); !w {
		t.Fatal("ground 10cm below the water level is not water")
	}
	if w, _ := v.IsWater(3000, 3000); w {
		t.Fatal("ground exactly at the water level is water")
	}
	g, err := v.Probe(50, 0) // 两点中间：插值 levelRaw − 0.5 → 高度比水位低 5cm
	if err != nil {
		t.Fatal(err)
	}
	if !g.Water || g.Height != testMeta.WaterLevel-5 || g.Material != 9 {
		t.Fatalf("Probe = %+v", g)
	}
}

// 覆盖块与原始块的规则：覆盖块替换原始块、LoadBase 不降级、旧版本被拒、基于别的原始版本被拒、入参与出参都复制。
func TestOverlayRules(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	key := ChunkKey{1, 0}
	if err := v.LoadBase(key); err != nil {
		t.Fatal(err)
	}
	if ver, ok := v.Version(key); !ok || ver != 0 {
		t.Fatalf("base chunk version %d, loaded %v", ver, ok)
	}
	o := flatOverlay(testMeta, 5, 1000, 3)
	if err := v.SetOverlay(key, o); err != nil {
		t.Fatal(err)
	}
	h := func() int32 {
		got, err := v.Height(ChunkSize+50, 50)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if h() != testMeta.WorldHeight(1000) {
		t.Fatalf("overlay not visible: %d", h())
	}
	o.Heights[0], o.Heights[1] = 7, 7 // 调用方之后改自己的切片不影响 View
	if h() != testMeta.WorldHeight(1000) {
		t.Fatal("View aliases the caller's slice")
	}
	if err := v.LoadBase(key); err != nil || h() != testMeta.WorldHeight(1000) {
		t.Fatalf("LoadBase replaced an overlay (err %v)", err)
	}
	if err := v.SetOverlay(key, flatOverlay(testMeta, 4, 2000, 3)); !errors.Is(err, ErrStaleOverlay) {
		t.Fatalf("older overlay: %v", err)
	}
	if err := v.SetOverlay(key, flatOverlay(testMeta, 5, 1500, 3)); err != nil || h() != testMeta.WorldHeight(1500) {
		t.Fatalf("same-version overlay: %v, height %d", err, h())
	}
	stale := flatOverlay(testMeta, 6, 1, 1)
	stale.BaseRevision++
	if err := v.SetOverlay(key, stale); !errors.Is(err, ErrBaseRevision) {
		t.Fatalf("overlay on another base revision: %v", err)
	}
	if err := v.SetOverlay(key, flatOverlay(testMeta, 0, 1, 1)); !errors.Is(err, ErrInvalidChunk) {
		t.Fatalf("version 0: %v", err)
	}
	short := flatOverlay(testMeta, 9, 1, 1)
	short.Materials = short.Materials[:10]
	if err := v.SetOverlay(key, short); !errors.Is(err, ErrInvalidChunk) {
		t.Fatalf("short materials: %v", err)
	}
	if err := v.SetOverlay(ChunkKey{-1, 0}, flatOverlay(testMeta, 1, 1, 1)); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("overlay outside the map: %v", err)
	}
	if err := v.LoadBase(ChunkKey{0, 2}); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("LoadBase outside the map: %v", err)
	}
	c, ver, ok := v.Chunk(key)
	if !ok || ver != 5 || c.Heights[0] != 1500 {
		t.Fatalf("Chunk = ver %d, loaded %v, h0 %d", ver, ok, c.Heights[0])
	}
	c.Heights[0] = 1 // 返回的是副本
	if c2, _, _ := v.Chunk(key); c2.Heights[0] != 1500 {
		t.Fatal("Chunk returned a shared slice")
	}
	// 卸下后可以重新装原始块，回到原始数据。
	v.Unload(key)
	if err := v.LoadBase(key); err != nil {
		t.Fatal(err)
	}
	base, _ := src(key)
	if got, _, _ := v.Chunk(key); !equalChunk(got, base) {
		t.Fatal("reloaded base chunk differs from the pack")
	}
	if v.Len() != 1 {
		t.Fatalf("Len = %d", v.Len())
	}
}

// 并发：查询与 SetOverlay / Unload / LoadBase 同时进行（-race）。每版覆盖块所有点同一高度、材质与高度一一对应，
// Probe 读到的高度与材质必须来自同一版（块数据整份替换，不会读到一半）；被卸下的块只会答 ErrNotLoaded 或原始数据。
func TestConcurrentQueriesAndUpdates(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	loadAll(t, v)
	raw := globalRaw(t, testMeta, src)
	if err := v.SetOverlay(ChunkKey{}, flatOverlay(testMeta, 1, 10, 1)); err != nil { // 材质 m ↔ 高度原始值 10·m
		t.Fatal(err)
	}
	var stop atomic.Bool
	var probes atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	report := func(err error) {
		select {
		case errs <- err:
		default:
		}
		stop.Store(true)
	}
	wg.Go(func() { // 写：块 (0,0) 不断换版本
		// 版本一直增大，数据按 版本 % 250 循环（材质是一个字节）；读够 probes 次后停。
		for ver := int64(2); probes.Load() < 30000 && !stop.Load(); ver++ {
			m := byte(ver % 250)
			if err := v.SetOverlay(ChunkKey{}, flatOverlay(testMeta, ver, uint16(m)*10, m)); err != nil {
				report(err)
			}
		}
		stop.Store(true)
	})
	wg.Go(func() { // 写：块 (2,1) 反复卸下、装回
		for !stop.Load() {
			v.Unload(ChunkKey{2, 1})
			if err := v.LoadBase(ChunkKey{2, 1}); err != nil {
				report(err)
			}
		}
	})
	for g := range 6 {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				probes.Add(1)
				x, y := int64(100+(g*997+i*31)%6000), int64(100+(g*389+i*17)%6000)
				gr, err := v.Probe(x, y)
				if err != nil {
					report(err)
					return
				}
				if ver := int64(gr.Material); gr.Height != testMeta.WorldHeight(uint16(ver*10)) {
					report(fmt.Errorf("Probe(%d, %d) = %+v: height and material from different versions", x, y, gr))
					return
				}
				x2, y2 := 2*ChunkSize+x, ChunkSize+y
				h, err := v.Height(x2, y2)
				switch {
				case errors.Is(err, ErrNotLoaded):
				case err != nil:
					report(err)
					return
				case h != refHeight(testMeta, raw, x2, y2):
					report(fmt.Errorf("Height(%d, %d) = %d during reloads", x2, y2, h))
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// BrushChanges 是事务之前的改动量估算（上层的每玩家改动点数配额用它）：与在同一份数据上 Apply 的结果按字段逐个
// 数出的点数相同；四块交点每个采样点只算一次；已在限外、材质相同等值不变的点不算；覆盖到没装的块报 ErrNotLoaded。
func TestViewBrushChangesCountsWhatApplyWouldChange(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	v := NewView(p)
	loadAll(t, v)
	// (1,0) 是覆盖块：一半的点材质已是 9，高度都在 2000。
	o := flatOverlay(testMeta, 3, 2000, 9)
	for k := range len(o.Materials) / 2 {
		o.Materials[k] = 4
	}
	if err := v.SetOverlay(ChunkKey{1, 0}, o); err != nil {
		t.Fatal(err)
	}
	corner := spatial.Point{X: ChunkSize, Y: ChunkSize}
	inOverlay := ChunkKey{1, 0}.Origin()
	limits := Limits{Bedrock: 0, Ceiling: 2000} // 覆盖块的高度正在上限
	for _, c := range []struct {
		b      Brush
		limits Limits
	}{
		{Brush{Op: BrushRaise, X: corner.X, Y: corner.Y, Radius: 1600, Falloff: FalloffSmooth, Strength: 40}, FullRange},
		{Brush{Op: BrushLower, X: corner.X, Y: corner.Y, Radius: 750, Falloff: FalloffLinear, Strength: 3}, FullRange},
		{Brush{Op: BrushFlatten, X: corner.X, Y: corner.Y, Radius: 1200, Target: 1500}, FullRange},
		{Brush{Op: BrushPaint, X: corner.X, Y: corner.Y, Radius: 750, Material: 201}, FullRange},
		{Brush{Op: BrushPaint, X: inOverlay.X + 3200, Y: inOverlay.Y + 3200, Radius: 1600, Material: 9}, limits},   // 一半已是 9
		{Brush{Op: BrushRaise, X: inOverlay.X + 3200, Y: inOverlay.Y + 3200, Radius: 500, Strength: 5}, limits},    // 都在上限
		{Brush{Op: BrushFlatten, X: inOverlay.X + 3200, Y: inOverlay.Y + 3200, Radius: 500, Target: 2000}, limits}, // 已是目标
	} {
		b, limits := c.b, c.limits
		chunks := map[ChunkKey]Chunk{}
		for _, key := range b.Chunks(testMeta) {
			c, _, _ := v.Chunk(key)
			chunks[key] = c
		}
		changes, err := b.Apply(testMeta, limits, chunks)
		if err != nil {
			t.Fatal(err)
		}
		wantHeights, wantMaterials := 0, 0
		for _, change := range changes {
			before := chunks[change.Key]
			for _, pt := range change.Points {
				if pt.Height != before.Heights[pt.Index] {
					wantHeights++
				}
				if pt.Material != before.Materials[pt.Index] {
					wantMaterials++
				}
			}
		}
		heights, materials, err := v.BrushChanges(b, limits)
		if err != nil || heights != wantHeights || materials != wantMaterials {
			t.Fatalf("%v r=%d: BrushChanges %d heights / %d materials (%v), Apply changed %d / %d", b.Op, b.Radius, heights, materials, err, wantHeights, wantMaterials)
		}
		t.Logf("%v r=%d: %d heights / %d materials", b.Op, b.Radius, heights, materials)
	}
	if h, m, _ := v.BrushChanges(Brush{Op: BrushPaint, X: corner.X, Y: corner.Y, Radius: 750, Material: 201}, limits); h != 0 || m != 177 {
		t.Fatalf("a corner paint counts %d heights / %d materials, want 0 / 177 (each sample point once)", h, m)
	}
	// 拆成每块一部分（P5 的跨格编辑）：各部分数出的点数合起来就是整笔的，也正是各块的 Footprint（改材质每个覆盖点都变）。
	paint := Brush{Op: BrushPaint, X: corner.X, Y: corner.Y, Radius: 750, Material: 201}
	footprint, sum := paint.Footprint(testMeta), 0
	for _, key := range paint.Chunks(testMeta) {
		h, m, err := v.BrushChangesIn(paint, limits, []ChunkKey{key})
		if err != nil || h != 0 || m != footprint[key] {
			t.Fatalf("corner paint part %v: %d / %d (%v), want 0 / %d", key, h, m, err, footprint[key])
		}
		sum += m
	}
	if sum != 177 {
		t.Fatalf("the corner paint's parts count %d materials, want 177", sum)
	}
	if h, m, _ := v.BrushChanges(Brush{Op: BrushRaise, X: inOverlay.X + 3200, Y: inOverlay.Y + 3200, Radius: 500, Strength: 5}, limits); h != 0 || m != 0 {
		t.Fatalf("raising points at the ceiling counts %d / %d, want nothing", h, m)
	}
	v.Unload(ChunkKey{0, 1})
	if _, _, err := v.BrushChanges(Brush{Op: BrushPaint, X: corner.X, Y: corner.Y, Radius: 750, Material: 1}, limits); !errors.Is(err, ErrNotLoaded) {
		t.Fatalf("a brush over an unloaded chunk: %v, want ErrNotLoaded", err)
	}
	// 只数装着的那一块：没装的邻块不在 part 里就不需要。
	if _, m, err := v.BrushChangesIn(Brush{Op: BrushPaint, X: corner.X, Y: corner.Y, Radius: 750, Material: 1}, limits, []ChunkKey{{1, 1}}); err != nil || m == 0 {
		t.Fatalf("a part over a loaded chunk next to an unloaded one: %d (%v)", m, err)
	}
	if _, _, err := v.BrushChanges(Brush{Op: BrushRaise, X: corner.X, Y: corner.Y, Radius: 750}, limits); !errors.Is(err, ErrInvalidBrush) {
		t.Fatalf("a raise without strength: %v, want ErrInvalidBrush", err)
	}
}
