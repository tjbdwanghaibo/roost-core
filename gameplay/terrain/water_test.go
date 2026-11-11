package terrain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 区域水位（可编辑地形 P4，方案 D3“全局 / 区域水位线”）：记在原始地形包里，查询（IsWater / WaterDepth / Probe）与
// 行走（Walker 的游泳与站立高度）按节点所属块的水位线判定，同一个来源。

// waterMeta 是 3 × 2 块的平地图：全局水位 400m；块 (1,0) 是高处的湖（水位 450m），块 (2,1) 没有水。
var waterMeta = Meta{BaseRevision: 1, ChunksX: 3, ChunksY: 2, WaterLevel: 40000, WaterRegions: []WaterRegion{
	{MinX: 1, MinY: 0, MaxX: 2, MaxY: 1, Level: 45000},
	{MinX: 2, MinY: 1, MaxX: 3, MaxY: 2, Level: math.MinInt32},
}}

// 包写出再打开：区域原样读回（Meta.Equal）；区域表任一字节坏了靠 metaSHA256 拒绝；区域重叠、越出地图、空区域、个数超限
// 写包时拒绝，手工改出来的包打开时同样拒绝。
func TestWaterRegionsRoundTripAndAreChecked(t *testing.T) {
	p, path := writeTestPack(t, waterMeta, flatSource(4200))
	if got := p.Meta(); !got.Equal(waterMeta) {
		t.Fatalf("meta read back %+v, want %+v", got, waterMeta)
	}
	got := p.Meta()
	got.WaterRegions[0].Level = 1
	if !p.Meta().Equal(waterMeta) {
		t.Fatal("Pack.Meta shares its water regions with the caller")
	}
	for _, c := range []struct {
		key  ChunkKey
		want int32
	}{{ChunkKey{0, 0}, 40000}, {ChunkKey{1, 0}, 45000}, {ChunkKey{2, 0}, 40000}, {ChunkKey{1, 1}, 40000}, {ChunkKey{2, 1}, math.MinInt32}} {
		if level := waterMeta.WaterLevelOf(c.key); level != c.want {
			t.Errorf("Meta.WaterLevelOf(%v) = %d, want %d", c.key, level, c.want)
		}
		if level := NewView(p).WaterLevelOf(c.key); level != c.want {
			t.Errorf("View.WaterLevelOf(%v) = %d, want %d", c.key, level, c.want)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	regionsAt := packHeaderSize + waterMeta.Chunks()*packIndexEntry
	open := func(name string, b []byte) error {
		bad := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(bad, b, 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := OpenPack(bad)
		if err == nil {
			p.Close()
		}
		return err
	}
	// 改区域表的一个字节：校验和不符。
	flipped := slices.Clone(raw)
	flipped[regionsAt+16] ^= 1
	if err := open("flipped", flipped); !errors.Is(err, ErrBadPack) {
		t.Errorf("a flipped water level: %v, want ErrBadPack", err)
	}
	// 改成与第一片重叠并重算校验和：校验和对了，区域规则仍拒绝。
	overlapping := slices.Clone(raw)
	binary.LittleEndian.PutUint32(overlapping[regionsAt+packRegionEntry:], 1)   // 第二片 minX = 1
	binary.LittleEndian.PutUint32(overlapping[regionsAt+packRegionEntry+4:], 0) // 第二片 minY = 0：覆盖了 (1,0)
	rehash(overlapping, waterMeta.Chunks(), len(waterMeta.WaterRegions))
	if err := open("overlapping", overlapping); !errors.Is(err, ErrBadPack) || !errors.Is(err, ErrInvalidMeta) {
		t.Errorf("overlapping regions: %v, want ErrBadPack and ErrInvalidMeta", err)
	}
	// 区域个数超过上限：读表之前拒绝。
	many := slices.Clone(raw)
	binary.LittleEndian.PutUint32(many[68:], MaxWaterRegions+1)
	if err := open("many", many); !errors.Is(err, ErrBadPack) {
		t.Errorf("%d regions: %v, want ErrBadPack", MaxWaterRegions+1, err)
	}

	for name, regions := range map[string][]WaterRegion{
		"overlap":   {{MinX: 0, MinY: 0, MaxX: 2, MaxY: 1}, {MinX: 1, MinY: 0, MaxX: 3, MaxY: 2}},
		"outside":   {{MinX: 2, MinY: 0, MaxX: 4, MaxY: 1}},
		"negative":  {{MinX: -1, MinY: 0, MaxX: 1, MaxY: 1}},
		"empty":     {{MinX: 1, MinY: 1, MaxX: 1, MaxY: 2}},
		"too many":  make([]WaterRegion, MaxWaterRegions+1),
		"inverted":  {{MinX: 2, MinY: 0, MaxX: 1, MaxY: 1}},
		"zero-size": {{}},
	} {
		meta := waterMeta
		meta.WaterRegions = regions
		if err := WritePackFile(filepath.Join(t.TempDir(), "bad.pack"), meta, flatSource(4200)); !errors.Is(err, ErrInvalidMeta) {
			t.Errorf("writing regions %q: %v, want ErrInvalidMeta", name, err)
		}
	}
}

// rehash 按改过的包头与两张表重算 metaSHA256。
func rehash(b []byte, chunks, regions int) {
	end := packHeaderSize + chunks*packIndexEntry + regions*packRegionEntry
	sum := sha256.New()
	sum.Write(b[:packMetaHashOff])
	sum.Write(b[packHeaderSize:end])
	copy(b[packMetaHashOff:], sum.Sum(nil))
}

// 挖坑成湖、填平成陆（方案 §9 P4 验收）：同一高度在不同水位的块里结论不同；把一个点挖到水位线以下就是水、水深按
// 水位线算，填回水位线及以上就是陆地。规则只看当前高度与块的水位，重放同一串编辑得到同一串结论。
func TestDigALakeFillItBackByTheChunksWaterLevel(t *testing.T) {
	v := newWalkView(t, waterMeta, 4200) // 地面 420m：高于全局 400m，低于湖区 450m
	at := func(key ChunkKey, i, j int64) (int64, int64) {
		o := key.Origin()
		return o.X + i*SampleSpacing, o.Y + j*SampleSpacing
	}
	probe := func(x, y int64) Ground {
		t.Helper()
		g, err := v.Probe(x, y)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	// 同一块地面：全局水位下是陆地，湖区里是 30m 深的水，没有水的区域永远是陆地。
	if g := probe(at(ChunkKey{0, 0}, 10, 10)); g.Water || g.Depth() != 0 || g.WaterLevel != 40000 {
		t.Errorf("ground 420m under the global level 400m: %+v, want dry land", g)
	}
	if g := probe(at(ChunkKey{1, 0}, 10, 10)); !g.Water || g.Depth() != 3000 || g.WaterLevel != 45000 {
		t.Errorf("ground 420m in the 450m lake region: %+v, want 30m of water", g)
	}
	if d, err := v.WaterDepth(at(ChunkKey{1, 0}, 10, 10)); err != nil || d != 3000 {
		t.Errorf("WaterDepth in the lake region = %d, %v, want 3000", d, err)
	}
	if g := probe(at(ChunkKey{2, 1}, 10, 10)); g.Water || g.WaterLevel != math.MinInt32 {
		t.Errorf("a region without water: %+v", g)
	}

	// 挖坑成湖：在全局水位的块 (0,0) 把 (10, 10) 挖到 399m → 是水、水深 1m；只挖到 400m（等于水位）→ 不是水。
	x, y := at(ChunkKey{0, 0}, 10, 10)
	steps := []struct {
		raw   uint16
		water bool
		depth int64
	}{{3990, true, 100}, {4000, false, 0}, {3950, true, 500}, {4001, false, 0}, {4200, false, 0}}
	var seen []bool
	for _, s := range steps {
		setRaw(t, v, map[[2]int64]uint16{{10, 10}: s.raw})
		g := probe(x, y)
		if g.Water != s.water || g.Depth() != s.depth {
			t.Errorf("ground raw %d: water %v depth %d, want %v %d", s.raw, g.Water, g.Depth(), s.water, s.depth)
		}
		seen = append(seen, g.Water)
	}
	// 填平成陆：湖区块 (1,0) 里把一片填到 450m（等于湖面）以上就是陆地。
	lx, ly := at(ChunkKey{1, 0}, 20, 20)
	setRaw(t, v, map[[2]int64]uint16{{64 + 20, 20}: 4500})
	if g := probe(lx, ly); g.Water {
		t.Errorf("ground filled to the lake surface 450m is still water: %+v", g)
	}
	// 重放同一串编辑：结论逐步相同（纯规则，没有隐藏状态）。
	v2 := newWalkView(t, waterMeta, 4200)
	for i, s := range steps {
		setRaw(t, v2, map[[2]int64]uint16{{10, 10}: s.raw})
		if g, err := v2.Probe(x, y); err != nil || g.Water != seen[i] {
			t.Errorf("replayed step %d: water %v, want %v (%v)", i, g.Water, seen[i], err)
		}
	}
}

// Walker 与查询同一来源：地图上每个节点，“是游泳节点”恰好等于“查询答是水且水深超过涉水深度”；游泳节点的站立高度是
// 节点所属块的水位线。区域边界两侧水位不同：从湖区走到全局水位的块按“湖面 → 岸上地面”判定。
func TestWalkerSwimsByTheSameWaterLevelAsTheQueries(t *testing.T) {
	src := func(key ChunkKey) (Chunk, error) {
		c := NewChunk()
		for k := range c.Heights {
			c.Heights[k] = uint16(3900 + (k*37+int(key.X)*101)%700) // 390m ～ 459.9m 起伏
		}
		return c, nil
	}
	p, _ := writeTestPack(t, waterMeta, src)
	v := NewView(p)
	loadAll(t, v)
	w := &Walker{View: v, Rules: DefaultWalkRules}
	swimming, wading := 0, 0
	for gy := range int64(waterMeta.ChunksY) * ChunkSamples {
		for gx := range int64(waterMeta.ChunksX) * ChunkSamples {
			pt := spatial.Point{X: gx * SampleSpacing, Y: gy * SampleSpacing}
			g, err := v.Probe(pt.X, pt.Y)
			if err != nil {
				t.Fatal(err)
			}
			swims, err := w.Swimming(pt)
			if err != nil {
				t.Fatal(err)
			}
			if want := g.Water && g.Depth() > int64(DefaultWalkRules.WadeDepth); swims != want {
				t.Fatalf("node (%d, %d): Swimming %v, query water %v depth %d (level %d)", gx, gy, swims, g.Water, g.Depth(), g.WaterLevel)
			}
			z, err := w.StandZ(pt)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case swims:
				swimming++
				if z != g.WaterLevel {
					t.Fatalf("node (%d, %d) swims: stand at %d, want the water level %d", gx, gy, z, g.WaterLevel)
				}
			case g.Water:
				wading++
				if z != g.Height {
					t.Fatalf("node (%d, %d) wades: stand at %d, want the ground %d", gx, gy, z, g.Height)
				}
			}
		}
	}
	if swimming == 0 || wading == 0 {
		t.Fatalf("no contrast: %d swimming and %d wading nodes", swimming, wading)
	}

	// 区域边界：块 (0,0) 与湖区块 (1,0) 之间。湖区一侧站在 450m 的湖面，全局一侧地面 420m（不是水）：两边差 30m，
	// 超过 MaxDrop 3m，两个方向都不能走（区域边界上水面有台阶，美术要把边界放在岸上）；把岸抬到 449.5m（差 50cm，
	// 1m 内是 500‰ 的坡）两个方向都能走。
	flat := newWalkView(t, waterMeta, 4200)
	fw := &Walker{View: flat, Rules: DefaultWalkRules}
	lake, shore := spatial.Point{X: 64, Y: 10}, spatial.Point{X: 63, Y: 10}
	if speed, ok := fw.Step(lake, lake); !ok || speed != DefaultWalkRules.SwimSpeed {
		t.Fatalf("standing in the lake region: speed %d, defined %v; want the swim speed", speed, ok)
	}
	if _, ok := fw.Step(shore, lake); ok {
		t.Fatal("from 420m ground up to the 450m lake surface (30m) is walkable")
	}
	if _, ok := fw.Step(lake, shore); ok {
		t.Fatal("from the 450m lake surface down to 420m ground (30m drop) is walkable")
	}
	setRaw(t, flat, map[[2]int64]uint16{{63, 10}: 4495})
	if _, ok := fw.Step(lake, shore); !ok {
		t.Fatal("from the 450m lake surface onto a 449.5m shore is not walkable")
	}
	if speed, ok := fw.Step(shore, lake); !ok || speed != DefaultWalkRules.SwimSpeed {
		t.Fatalf("from a 449.5m shore into the 450m lake: speed %d, %v", speed, ok)
	}
}

// 取出的地图参数是副本（RV-20261110-03）：区域水位只随原始地形包换版，调用方改 Pack.Meta() / View.Meta() 返回值里的区域
// 不改到包与视图里。改过之后包头、视图的参数、视图按块算好的水位表与查询答的仍是同一条水位线。
func TestMetaIsACopyAtEveryPublicBoundary(t *testing.T) {
	p, _ := writeTestPack(t, waterMeta, flatSource(4200))
	v := NewView(p)
	loadAll(t, v)
	lake := ChunkKey{X: 1, Y: 0} // waterMeta 的第 0 片区域：水位 450m
	for name, get := range map[string]func() Meta{"Pack.Meta": p.Meta, "View.Meta": v.Meta} {
		got := get()
		got.WaterRegions[0].Level = 99999
		if level := get().WaterLevelOf(lake); level != 45000 {
			t.Errorf("%s: writing to the returned water regions changed the next %s to level %d, want 45000", name, name, level)
		}
	}
	x, y := lake.Origin().X+10*SampleSpacing, lake.Origin().Y+10*SampleSpacing
	g, err := v.Probe(x, y)
	if err != nil {
		t.Fatal(err)
	}
	if a, b, c := p.Meta().WaterLevelOf(lake), v.Meta().WaterLevelOf(lake), v.WaterLevelOf(lake); a != 45000 || b != 45000 || c != 45000 || g.WaterLevel != 45000 {
		t.Errorf("water level of %v: pack meta %d, view meta %d, view table %d, probe %d; all must stay 45000", lake, a, b, c, g.WaterLevel)
	}
	if !p.Meta().Equal(waterMeta) || !v.Meta().Equal(waterMeta) {
		t.Errorf("metadata after the writes: pack %+v, view %+v, want %+v", p.Meta(), v.Meta(), waterMeta)
	}
}

// 水深是水位线与地面高度两个 int32 之差，按 int64 给出（RV-20261110-04）：高度基准可以为负、水位线是独立配置的 int32，
// 合法的地图参数下差值可以超出 int32（最大 2³² − 1）。全局水位与区域水位同一规则；“无水”（math.MinInt32）的块永远
// 不是水、水深 0。客户端 TerrainGround.Depth 用同一组数（client/dotnet/Roost.Client.Terrain.Tests/ViewTests.cs 的
// TestWaterDepthRange）。
func TestWaterDepthCoversTheWholeLevelRange(t *testing.T) {
	const none = math.MinInt32
	cases := []struct {
		name                   string
		base                   int32 // HeightBase；地面原始值都是 0，所以地面高度就是它
		global, lake           int32 // 全局水位与块 (1,0) 的区域水位
		globalWet, lakeWet     bool  // 块 (0,0)（全局水位）、块 (1,0)（区域水位）是不是水
		globalDepth, lakeDepth int64 // 两处的水深
	}{
		{"ordinary levels", -10000, 0, 500, true, true, 10000, 10500},
		{"审查的复现：负基准、全局水位取 int32 最大值", -10000, math.MaxInt32, none, true, false, math.MaxInt32 + 10000, 0},
		{"区域水位取 int32 最大值、全局无水", -10000, none, math.MaxInt32, false, true, 0, math.MaxInt32 + 10000},
		{"基准取 int32 最小值：差值到 2³² − 1", math.MinInt32, math.MaxInt32, math.MinInt32 + 1, true, true, math.MaxUint32, 1},
		{"基准取 int32 最小值、两处都无水", math.MinInt32, none, none, false, false, 0, 0},
		{"水位线等于地面不算水", -10000, -10000, -9999, false, true, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			meta := Meta{BaseRevision: 1, ChunksX: 2, ChunksY: 1, HeightBase: c.base, WaterLevel: c.global,
				WaterRegions: []WaterRegion{{MinX: 1, MinY: 0, MaxX: 2, MaxY: 1, Level: c.lake}}}
			if err := meta.Validate(); err != nil {
				t.Fatal(err)
			}
			v := newWalkView(t, meta, 0)
			for _, at := range []struct {
				key   ChunkKey
				water bool
				depth int64
			}{{ChunkKey{X: 0, Y: 0}, c.globalWet, c.globalDepth}, {ChunkKey{X: 1, Y: 0}, c.lakeWet, c.lakeDepth}} {
				x, y := at.key.Origin().X+SampleSpacing, at.key.Origin().Y+SampleSpacing
				g, err := v.Probe(x, y)
				if err != nil {
					t.Fatal(err)
				}
				d, err := v.WaterDepth(x, y)
				if err != nil {
					t.Fatal(err)
				}
				if g.Water != at.water || g.Depth() != at.depth || d != at.depth {
					t.Errorf("chunk %v (ground %d, level %d): water %v, Ground.Depth %d, WaterDepth %d; want %v, %d",
						at.key, g.Height, g.WaterLevel, g.Water, g.Depth(), d, at.water, at.depth)
				}
			}
		})
	}
}
