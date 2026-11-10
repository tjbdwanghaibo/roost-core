package terrain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"os"
	"slices"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite the client terrain golden (client/spec/terrain-*)")

const (
	goldenPackPath    = "../../client/spec/terrain-golden.pack"
	goldenQueriesPath = "../../client/spec/terrain-queries.json"
)

// goldenSource 是金样地图的数据：3 × 2 块、程序化地形，块 (2,1) 的左半是边界情形——
// i < 16 的列是 0 / 65535 的棋盘格（平面预测误差最大，每点 3 字节 varint；插值时整数中间值最大），16 ≤ i < 32 是斜坡。
func goldenSource() (Meta, ChunkSource) {
	meta := Meta{BaseRevision: 7, ChunksX: 3, ChunksY: 2, HeightBase: -12345}
	// 水位线取程序化高度的中位数，保证地图里既有水也有陆地。
	var all []uint16
	for gy := range int64(meta.ChunksY) * ChunkSamples {
		for gx := range int64(meta.ChunksX) * ChunkSamples {
			all = append(all, genHeight(goldenSeed, gx, gy))
		}
	}
	slices.Sort(all)
	meta.WaterLevel = meta.WorldHeight(all[len(all)/2])
	// 区域水位（格式版本 2）：块 (1,1) 的水位高出全局 3m，块 (0,1) 没有水，用来核对客户端按块取水位线。
	meta.WaterRegions = []WaterRegion{
		{MinX: 1, MinY: 1, MaxX: 2, MaxY: 2, Level: meta.WaterLevel + 300},
		{MinX: 0, MinY: 1, MaxX: 1, MaxY: 2, Level: math.MinInt32},
	}
	gen := Generated(meta, goldenSeed)
	return meta, func(key ChunkKey) (Chunk, error) {
		c, err := gen(key)
		if err != nil || key != (ChunkKey{2, 1}) {
			return c, err
		}
		for k := range c.Heights {
			i, j := k%ChunkSamples, k/ChunkSamples
			switch {
			case i < 16 && (i+j)%2 == 0:
				c.Heights[k] = 65535
			case i < 16:
				c.Heights[k] = 0
			case i < 32:
				c.Heights[k] = uint16(i*2000 + j*7)
			}
			c.Materials[k] = byte(k)
		}
		return c, nil
	}
}

const goldenSeed = 20261108

type goldenFile struct {
	Description string        `json:"description"`
	Pack        string        `json:"pack"`
	PackSHA256  string        `json:"pack_sha256"`
	Meta        goldenMeta    `json:"meta"`
	Chunks      []goldenChunk `json:"chunks"`
	Queries     []goldenQuery `json:"queries"`
}

type goldenMeta struct {
	BaseRevision  int64 `json:"base_revision"`
	ChunksX       int32 `json:"chunks_x"`
	ChunksY       int32 `json:"chunks_y"`
	ChunkSamples  int   `json:"chunk_samples"`
	SampleSpacing int   `json:"sample_spacing"`
	HeightStep    int   `json:"height_step"`
	HeightBase    int32 `json:"height_base"`
	WaterLevel    int32 `json:"water_level"`
	// WaterRegions 是区域水位（块坐标半开区间，level 是世界高度厘米；-2147483648 表示没有水）。
	WaterRegions []goldenRegion `json:"water_regions"`
}

type goldenRegion struct {
	MinX  int32 `json:"min_x"`
	MinY  int32 `json:"min_y"`
	MaxX  int32 `json:"max_x"`
	MaxY  int32 `json:"max_y"`
	Level int32 `json:"level"`
}

type goldenChunk struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
	// StoredSHA256 是索引表里块存储字节的 SHA-256；HeightsSHA256 是解码后 4096 个高度按 uint16 小端排成 8192 字节的
	// SHA-256；MaterialsSHA256 是 4096 字节材质的 SHA-256。
	StoredSHA256    string `json:"stored_sha256"`
	HeightsSHA256   string `json:"heights_sha256"`
	MaterialsSHA256 string `json:"materials_sha256"`
}

type goldenQuery struct {
	X        int64  `json:"x"`
	Y        int64  `json:"y"`
	Height   *int32 `json:"height,omitempty"`
	Material *byte  `json:"material,omitempty"`
	Water    *bool  `json:"water,omitempty"`
	// WaterLevel 是这个位置所在块的水位线（区域水位或全局水位）。
	WaterLevel *int32 `json:"water_level,omitempty"`
	Error      string `json:"error,omitempty"`
}

// goldenPoints 是金样的采样位置：每块的角与内部、块边界两侧、四块交点、地图边缘、边界情形块、伪随机点、地图外。
func goldenPoints(meta Meta) [][2]int64 {
	var pts [][2]int64
	for key := range ChunksIn(meta.Bounds()) {
		o := key.Origin()
		for _, d := range [][2]int64{{0, 0}, {50, 50}, {6399, 6399}, {6350, 25}, {1234, 5678}} {
			pts = append(pts, [2]int64{o.X + d[0], o.Y + d[1]})
		}
	}
	for _, x := range []int64{6299, 6300, 6301, 6399, 6400, 6401, 12799, 12800} {
		for _, y := range []int64{0, 6350, 6399, 6400, 6401, 12750} {
			pts = append(pts, [2]int64{x, y})
		}
	}
	b := meta.Bounds()
	pts = append(pts, [2]int64{b.Max.X - 1, b.Max.Y - 1}, [2]int64{b.Max.X - 1, 0}, [2]int64{0, b.Max.Y - 1},
		[2]int64{b.Max.X - 37, b.Max.Y - 61})
	edge := ChunkKey{2, 1}.Origin()
	for _, d := range [][2]int64{{0, 0}, {50, 50}, {99, 99}, {1, 99}, {1550, 3333}, {1599, 100}, {1600, 50}, {2050, 4321}, {3150, 70}} {
		pts = append(pts, [2]int64{edge.X + d[0], edge.Y + d[1]})
	}
	state := uint64(goldenSeed)
	for range 150 {
		state = state*6364136223846793005 + 1442695040888963407
		pts = append(pts, [2]int64{int64(state>>33) % b.Max.X, int64(state>>13&0xFFFFF) % b.Max.Y})
	}
	return append(pts, [2]int64{-1, 0}, [2]int64{0, -1}, [2]int64{b.Max.X, 0}, [2]int64{0, b.Max.Y}, [2]int64{-ChunkSize, -ChunkSize})
}

func hexSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// buildGolden 从已写出的金样包算出 JSON：包与块的校验和、每个采样位置的查询结果。
func buildGolden(t *testing.T, meta Meta, src ChunkSource) []byte {
	t.Helper()
	raw, err := os.ReadFile(goldenPackPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := OpenPack(goldenPackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Meta().Equal(meta) {
		t.Fatalf("golden pack meta %+v, want %+v", p.Meta(), meta)
	}
	out := goldenFile{
		Description: "Terrain golden for client implementations (gameplay/terrain/README.md). World units are centimetres. " +
			"Decode every chunk of the pack and check the SHA-256 values, then load all chunks and check each query: " +
			"height = height_base + (num*height_step + S*S/2) / (S*S) with S = sample_spacing and " +
			"num = (h00*(S-fx) + h10*fx)*(S-fy) + (h01*(S-fx) + h11*fx)*fy (integer, edge samples replicated at the map border); " +
			"material is the sample at (floor(x/S), floor(y/S)); the water level of a point is the level of the water region holding its chunk " +
			"(chunk = (floor(x/6400), floor(y/6400)); regions are half-open chunk rectangles that never overlap), otherwise the global water_level; " +
			"water = (height less than that water level); points outside the map give error out_of_map.",
		Pack:       "terrain-golden.pack",
		PackSHA256: hexSum(raw),
		Meta: goldenMeta{BaseRevision: meta.BaseRevision, ChunksX: meta.ChunksX, ChunksY: meta.ChunksY, ChunkSamples: ChunkSamples,
			SampleSpacing: SampleSpacing, HeightStep: HeightStep, HeightBase: meta.HeightBase, WaterLevel: meta.WaterLevel},
	}
	for _, r := range meta.WaterRegions {
		out.Meta.WaterRegions = append(out.Meta.WaterRegions, goldenRegion{MinX: r.MinX, MinY: r.MinY, MaxX: r.MaxX, MaxY: r.MaxY, Level: r.Level})
	}
	v := NewView(p)
	for key := range ChunksIn(meta.Bounds()) {
		c, err := p.ReadChunk(key)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := src(key)
		if !equalChunk(c, want) {
			t.Fatalf("golden pack chunk %v does not hold the golden data", key)
		}
		if err := v.LoadBase(key); err != nil {
			t.Fatal(err)
		}
		stored, _ := p.ChunkSum(key)
		le := make([]byte, 0, 2*ChunkPoints)
		for _, h := range c.Heights {
			le = binary.LittleEndian.AppendUint16(le, h)
		}
		out.Chunks = append(out.Chunks, goldenChunk{X: key.X, Y: key.Y, StoredSHA256: hex.EncodeToString(stored[:]),
			HeightsSHA256: hexSum(le), MaterialsSHA256: hexSum(c.Materials)})
	}
	water := 0
	regional := map[ChunkKey]bool{}
	for _, pt := range goldenPoints(meta) {
		q := goldenQuery{X: pt[0], Y: pt[1]}
		g, err := v.Probe(pt[0], pt[1])
		switch {
		case errors.Is(err, ErrOutOfMap):
			q.Error = "out_of_map"
		case err != nil:
			t.Fatal(err)
		default:
			q.Height, q.Material, q.Water, q.WaterLevel = &g.Height, &g.Material, &g.Water, &g.WaterLevel
			if g.Water {
				water++
			}
			if key := ChunkOf(pt[0], pt[1]); g.WaterLevel != meta.WaterLevel {
				regional[key] = true
			}
		}
		out.Queries = append(out.Queries, q)
	}
	if water == 0 || water == len(out.Queries) {
		t.Fatalf("golden queries have %d water points out of %d: no contrast", water, len(out.Queries))
	}
	if len(regional) != len(meta.WaterRegions) {
		t.Fatalf("golden queries reach the regional water level in chunks %v, want one per region", regional)
	}
	content, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

// 客户端一致性金样：client/spec/terrain-golden.pack 是金样地图的原始地形包，terrain-queries.json 记录包与每块的
// 校验和以及一组位置的期望查询结果，供 C# / C++ 实现逐项对照。
//
// 本测试检查：提交的包能打开、每块解码后正是金样数据；从这个包重新算出的 JSON 与提交的逐字节相同。
// 包本身不要求与重新压缩的结果逐字节相同（DEFLATE 的输出可能随 Go 版本变化），所以不会因为升级 Go 而变红；
// 改了格式、算法或金样数据时用 -update 重新生成两份文件，并让客户端实现重新对照。
func TestClientTerrainGolden(t *testing.T) {
	meta, src := goldenSource()
	if *updateGolden {
		if err := WritePackFile(goldenPackPath, meta, src); err != nil {
			t.Fatal(err)
		}
	}
	content := buildGolden(t, meta, src)
	if *updateGolden {
		if err := os.WriteFile(goldenQueriesPath, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(goldenQueriesPath)
	if err != nil {
		t.Fatal(err)
	}
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(content, actual) {
		t.Fatal("client terrain golden changed: regenerate deliberately with -update and re-check the client implementations")
	}
}
