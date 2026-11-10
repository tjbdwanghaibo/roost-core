package terrain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

const (
	shorePackPath    = "../../client/spec/terrain-shore.pack"
	shoreQueriesPath = "../../client/spec/terrain-shore.json"
)

// shoreSource 是客户端岸线（T03，方案 §31）一致性金样的地图：3 × 3 块，平地高出水位 1m，上面挖出几处只用整数算出的水坑，
// 让岸线经过 Marching Squares 的各种情形，并跨过块边、块的交点与区域水位边界：
//   - 圆形池塘，圆心在四块交点 (64m, 64m) 附近（偏 37cm / −23cm，不对称）；
//   - 椭圆池塘横跨两块的公共边 x = 128m；
//   - 跨区域水位边界的水坑：块 (1,2) 用全局水位 0、块 (2,2) 是区域水位 +50cm，公共边两侧水面不连续；
//   - 跨“无水区域”边界的水坑：块 (0,1) 有水、块 (0,2) 是 Level = MinInt32 的区域；
//   - 块 (2,0) 里一片正负相间的采样（大量鞍点格，两种连通判定都有）；
//   - 块 (0,0) 里一片恰好等于水位的平台（等于水位按陆地算，整边等高没有交点），中间一小块低 10cm 的水。
func shoreSource() (Meta, ChunkSource) {
	meta := Meta{BaseRevision: 3, ChunksX: 3, ChunksY: 3, HeightBase: -10000, WaterLevel: 0,
		WaterRegions: []WaterRegion{
			{MinX: 2, MinY: 2, MaxX: 3, MaxY: 3, Level: 50},
			{MinX: 0, MinY: 2, MaxX: 1, MaxY: 3, Level: math.MinInt32},
		}}
	return meta, func(key ChunkKey) (Chunk, error) {
		c := NewChunk()
		for k := range c.Heights {
			gx := int64(key.X)*ChunkSamples + int64(k%ChunkSamples)
			gy := int64(key.Y)*ChunkSamples + int64(k/ChunkSamples)
			c.Heights[k] = uint16(1000 + floorDiv(shoreHeight(gx, gy), 10))
		}
		return c, nil
	}
}

// shoreHeight 是世界采样 (gx, gy) 的世界高度（厘米，HeightBase −10000 时原始值 1000 对应 0）。
func shoreHeight(gx, gy int64) int64 {
	x, y := gx*SampleSpacing, gy*SampleSpacing
	h := int64(100)
	// bowl 是以 (cx, cy) 为心、半径 r 的碗：中心 bottom、半径处 rim（cm），外面是 rim 以上被平地截平。
	bowl := func(cx, cy, r, bottom, rim int64) {
		d2 := (x-cx)*(x-cx) + (y-cy)*(y-cy)
		if v := bottom + (rim-bottom)*d2/(r*r); v < h {
			h = v
		}
	}
	bowl(6437, 6377, 950, -150, 0)
	bowl(12800-150, 16000, 1000, -250, 100)
	bowl(3200, 12840, 800, -200, 100)
	// 椭圆：q = dx²/rx² + dy²/ry²，高度 (q − 1)·180。
	const rx, ry = 1200, 700
	dx, dy := x-12810, y-9600
	if v := (dx*dx*ry*ry + dy*dy*rx*rx - rx*rx*ry*ry) * 180 / (rx * rx * ry * ry); v < h {
		h = v
	}
	// 鞍点区：正负相间，幅度 10～30cm，按位置伪随机。
	if gx >= 140 && gx < 148 && gy >= 20 && gy < 28 {
		amp := 10 * (1 + (gx*7+gy*13)%3)
		if (gx+gy)%2 == 0 {
			return amp
		}
		return -amp
	}
	// 等于水位的平台，中间低 10cm。
	if gx >= 10 && gx < 20 && gy >= 10 && gy < 20 {
		if gx >= 13 && gx < 17 && gy >= 13 && gy < 16 {
			return -10
		}
		return 0
	}
	return h
}

type shoreGoldenFile struct {
	Description string        `json:"description"`
	Pack        string        `json:"pack"`
	PackSHA256  string        `json:"pack_sha256"`
	Meta        goldenMeta    `json:"meta"`
	Windows     []shoreWindow `json:"windows"`
}

// shoreWindow 是一片规则格点上的权威“是水”结果：点 (x0 + c·step, y0 + r·step)，c ∈ [0, columns)、r ∈ [0, rows)；
// Rows[r] 是这一行的位串，十六进制，每个字符 4 位、高位在前：第 c 列是第 c/4 个字符的第 (3 − c%4) 位，1 = 水。
type shoreWindow struct {
	Name    string   `json:"name"`
	X0      int64    `json:"x0"`
	Y0      int64    `json:"y0"`
	Step    int64    `json:"step"`
	Columns int      `json:"columns"`
	Water   int      `json:"water"`
	Rows    []string `json:"rows"`
}

func buildShoreGolden(t *testing.T, meta Meta, src ChunkSource) []byte {
	t.Helper()
	raw, err := os.ReadFile(shorePackPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPack(shorePackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Meta().Equal(meta) {
		t.Fatalf("shore pack meta %+v, want %+v", p.Meta(), meta)
	}
	v := NewView(p)
	for key := range ChunksIn(meta.Bounds()) {
		c, err := p.ReadChunk(key)
		if err != nil {
			t.Fatal(err)
		}
		if want, _ := src(key); !equalChunk(c, want) {
			t.Fatalf("shore pack chunk %v does not hold the golden data", key)
		}
		if err := v.LoadBase(key); err != nil {
			t.Fatal(err)
		}
	}
	out := shoreGoldenFile{
		Description: "Client shoreline golden (FEATURE-2026-11-08-editable-terrain.md §31). World units are centimetres. " +
			"Each window lists the authoritative water flag (gameplay/terrain View.IsWater: height < water level of the point's chunk) " +
			"on a regular grid crossing a shoreline: a pond centred near a four-chunk corner, a pond across a two-chunk edge, " +
			"pools across a regional water-level boundary and across a no-water region, a field of saddle cells and a plateau exactly at the water level. " +
			"A client shoreline must classify every point the same way except within its stated deviation of the shoreline.",
		Pack:       "terrain-shore.pack",
		PackSHA256: hexSum(raw),
		Meta: goldenMeta{BaseRevision: meta.BaseRevision, ChunksX: meta.ChunksX, ChunksY: meta.ChunksY, ChunkSamples: ChunkSamples,
			SampleSpacing: SampleSpacing, HeightStep: HeightStep, HeightBase: meta.HeightBase, WaterLevel: meta.WaterLevel},
	}
	for _, r := range meta.WaterRegions {
		out.Meta.WaterRegions = append(out.Meta.WaterRegions, goldenRegion{MinX: r.MinX, MinY: r.MinY, MaxX: r.MaxX, MaxY: r.MaxY, Level: r.Level})
	}
	windows := []struct {
		name               string
		x0, y0, step, size int64
	}{
		{"pond across the x=64m chunk edge near the four-chunk corner", 6250, 7176, 3, 300},
		{"pond across the y=64m chunk edge near the four-chunk corner", 7237, 6250, 3, 300},
		{"pond across the two-chunk edge x=128m", 12650, 10150, 3, 300},
		{"pool across the regional water-level boundary x=128m (0 | +50cm)", 12650, 16750, 3, 300},
		{"pool across the no-water region boundary y=128m", 3700, 12650, 3, 300},
		{"saddle cells", 14000, 2000, 8, 800},
		{"plateau at the water level", 1000, 1000, 10, 1000},
	}
	for _, w := range windows {
		n := int(w.size / w.step)
		win := shoreWindow{Name: w.name, X0: w.x0, Y0: w.y0, Step: w.step, Columns: n}
		for r := range n {
			row := make([]byte, (n+7)/8)
			for c := range n {
				water, err := v.IsWater(w.x0+int64(c)*w.step, w.y0+int64(r)*w.step)
				if err != nil {
					t.Fatal(err)
				}
				if water {
					row[c/8] |= 0x80 >> (c % 8)
					win.Water++
				}
			}
			win.Rows = append(win.Rows, hex.EncodeToString(row)[:(n+3)/4])
		}
		if win.Water == 0 || win.Water == n*n {
			t.Fatalf("shore window %q has %d water points out of %d: no shoreline", w.name, win.Water, n*n)
		}
		out.Windows = append(out.Windows, win)
	}
	content, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

// 客户端岸线一致性金样：client/spec/terrain-shore.pack 与 terrain-shore.json。C# 的 TerrainShoreBuilder 读同一个包，
// 逐点核对权威查询与这里一致，并核对岸线的视觉分类只在误差带内与它不同（client/dotnet/Roost.Client.Terrain.Tests）。
// 与 TestClientTerrainGolden 相同：包只在 -update 时重写，平时从提交的包重新算 JSON 并逐字节比较。
func TestClientShoreGolden(t *testing.T) {
	meta, src := shoreSource()
	if *updateGolden {
		if err := WritePackFile(shorePackPath, meta, src); err != nil {
			t.Fatal(err)
		}
	}
	content := buildShoreGolden(t, meta, src)
	if *updateGolden {
		if err := os.WriteFile(shoreQueriesPath, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(shoreQueriesPath)
	if err != nil {
		t.Fatal(err)
	}
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(content, actual) {
		t.Fatal("client shore golden changed: regenerate deliberately with -update and re-check the client implementations")
	}
}
