package terrain

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"testing"
)

const goldenBrushPath = "../../client/spec/terrain-brush.json"

// brushMeta 是笔刷测试的地图：4 × 3 块的程序化地形。
func brushMeta() Meta { return Meta{BaseRevision: 3, ChunksX: 4, ChunksY: 3} }

// brushChunks 读出 meta 地图里 keys 的原始块（程序化，确定性）。
func brushChunks(t *testing.T, meta Meta, keys []ChunkKey) map[ChunkKey]Chunk {
	t.Helper()
	gen := Generated(meta, goldenSeed)
	out := map[ChunkKey]Chunk{}
	for _, key := range keys {
		c, err := gen(key)
		if err != nil {
			t.Fatal(err)
		}
		out[key] = c
	}
	return out
}

func TestISqrtIsTheFloorOfTheSquareRoot(t *testing.T) {
	for n := int64(0); n < 200000; n++ {
		r := ISqrt(n)
		if r*r > n || (r+1)*(r+1) <= n {
			t.Fatalf("ISqrt(%d) = %d", n, r)
		}
	}
	for _, n := range []int64{1 << 40, 1<<40 - 1, 1<<40 + 1, math.MaxInt64, math.MaxInt64 - 1, 999999999999} {
		r := ISqrt(n)
		// (r+1)² 可能溢出：用 r ≤ n/r 的等价形式核对。
		if r > n/r || (r+1) <= n/(r+1) {
			t.Fatalf("ISqrt(%d) = %d", n, r)
		}
	}
	if ISqrt(-5) != 0 {
		t.Fatal("ISqrt of a negative number")
	}
}

func TestFalloffWeights(t *testing.T) {
	const r = 500
	for _, f := range []Falloff{FalloffConstant, FalloffLinear, FalloffSmooth} {
		if w := f.Weight(0, r); w != WeightScale {
			t.Fatalf("%v at the centre = %d", f, w)
		}
		prev := int64(WeightScale)
		for d := int64(0); d <= r+1; d++ {
			w := f.Weight(d*d, r)
			if w < 0 || w > WeightScale || w > prev {
				t.Fatalf("%v at d=%d gives %d after %d: not a non-increasing weight in range", f, d, w, prev)
			}
			prev = w
		}
		if w := f.Weight((r+1)*(r+1), r); w != 0 {
			t.Fatalf("%v outside the radius = %d", f, w)
		}
	}
	if FalloffConstant.Weight(r*r, r) != WeightScale {
		t.Fatal("a constant brush covers its rim")
	}
	if FalloffLinear.Weight(r*r, r) != 0 || FalloffSmooth.Weight(r*r, r) != 0 {
		t.Fatal("linear and smooth weights vanish on the rim")
	}
	if got, want := FalloffLinear.Weight(250*250, r), int64(WeightScale/2); got != want {
		t.Fatalf("linear at half the radius = %d, want %d", got, want)
	}
	// (1 − 1/4)² = 9/16
	if got, want := FalloffSmooth.Weight(250*250, r), int64(WeightScale*9/16); got != want {
		t.Fatalf("smooth at half the radius = %d, want %d", got, want)
	}
}

// 共享边：笔刷落在四块交点上时，每个覆盖到的世界采样点恰好在一块里改一次（共享边归右 / 上块），块间不重复、不遗漏。
func TestABrushOnAChunkCornerChangesEverySamplePointOnce(t *testing.T) {
	meta := brushMeta()
	b := Brush{Op: BrushPaint, X: ChunkSize, Y: ChunkSize, Radius: 750, Material: 201}
	keys := b.Chunks(meta)
	if want := []ChunkKey{{0, 0}, {1, 0}, {0, 1}, {1, 1}}; !slices.Equal(keys, want) {
		t.Fatalf("chunks %v, want %v", keys, want)
	}
	chunks := brushChunks(t, meta, keys)
	for _, c := range chunks {
		for i := range c.Materials {
			c.Materials[i] = 1
		}
	}
	changes, err := b.Apply(meta, FullRange, chunks)
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for gy := int64(64 - 8); gy <= 64+8; gy++ {
		for gx := int64(64 - 8); gx <= 64+8; gx++ {
			dx, dy := gx*SampleSpacing-b.X, gy*SampleSpacing-b.Y
			if dx*dx+dy*dy <= b.Radius*b.Radius {
				covered++
			}
		}
	}
	seen := map[[2]int64]bool{}
	for _, change := range changes {
		for _, p := range change.Points {
			gx := int64(change.Key.X)*ChunkSamples + int64(p.Index)%ChunkSamples
			gy := int64(change.Key.Y)*ChunkSamples + int64(p.Index)/ChunkSamples
			if seen[[2]int64{gx, gy}] {
				t.Fatalf("sample (%d, %d) changed twice", gx, gy)
			}
			seen[[2]int64{gx, gy}] = true
			if p.Material != 201 {
				t.Fatalf("sample (%d, %d) material %d", gx, gy, p.Material)
			}
		}
	}
	if len(seen) != covered || covered != 177 {
		t.Fatalf("%d samples changed, %d covered (want 177: the lattice points within 7.5 sample spacings)", len(seen), covered)
	}
	// 中心那一点 (64, 64) 归块 (1,1) 的下标 0。
	if p := changes[3].Points[0]; changes[3].Key != (ChunkKey{1, 1}) || p.Index != 0 {
		t.Fatalf("the corner sample is %v in %v", p, changes[3].Key)
	}
}

func TestBrushesClampToBedrockAndCeilingButNeverPushOutward(t *testing.T) {
	meta := brushMeta()
	key := ChunkKey{1, 1}
	centre := key.Origin()
	chunks := map[ChunkKey]Chunk{key: NewChunk()}
	c := chunks[key]
	for i := range c.Heights {
		c.Heights[i] = 1000
	}
	// 中心那一点原本就高于上限、旁边一点原本就低于基岩。
	c.Heights[32*64+32] = 5000
	c.Heights[32*64+33] = 10
	limits := Limits{Bedrock: 900, Ceiling: 1050}
	at := func(changes []ChunkChange, index int32) uint16 {
		for _, change := range changes {
			for _, p := range change.Points {
				if p.Index == index {
					return p.Height
				}
			}
		}
		return c.Heights[index]
	}
	raise := Brush{Op: BrushRaise, X: centre.X + 3200, Y: centre.Y + 3200, Radius: 300, Strength: 200}
	changes, err := raise.Apply(meta, limits, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if h := at(changes, 31*64+32); h != 1050 {
		t.Fatalf("raised to %d, want the ceiling 1050", h)
	}
	if h := at(changes, 32*64+32); h != 5000 {
		t.Fatalf("a point above the ceiling became %d, want it left at 5000", h)
	}
	if h := at(changes, 32*64+33); h != 210 {
		t.Fatalf("a point below bedrock raised to %d, want 10+200", h)
	}
	lower := raise
	lower.Op = BrushLower
	changes, err = lower.Apply(meta, limits, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if h := at(changes, 31*64+32); h != 900 {
		t.Fatalf("lowered to %d, want bedrock 900", h)
	}
	if h := at(changes, 32*64+33); h != 10 {
		t.Fatalf("a point below bedrock was lowered to %d, want it left at 10", h)
	}
	if h := at(changes, 32*64+32); h != 4800 {
		t.Fatalf("a point above the ceiling lowered to %d, want 5000-200", h)
	}
}

func TestAConstantFlattenReachesTheTargetAndIsIdempotent(t *testing.T) {
	meta := brushMeta()
	b := Brush{Op: BrushFlatten, X: 9000, Y: 7000, Radius: 1200, Target: 4321}
	chunks := brushChunks(t, meta, b.Chunks(meta))
	changes, err := b.Apply(meta, FullRange, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatal("nothing flattened")
	}
	for _, change := range changes {
		for _, p := range change.Points {
			if p.Height != 4321 {
				t.Fatalf("flattened to %d", p.Height)
			}
			chunks[change.Key].Heights[p.Index] = p.Height
		}
	}
	again, err := b.Apply(meta, FullRange, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second flatten changed %d chunks", len(again))
	}
	// 平滑衰减的整平：每点在原值与目标之间，边上不动。
	smooth := b
	smooth.Falloff = FalloffSmooth
	fresh := brushChunks(t, meta, b.Chunks(meta))
	changes, err = smooth.Apply(meta, FullRange, fresh)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		for _, p := range change.Points {
			before := int64(fresh[change.Key].Heights[p.Index])
			lo, hi := min(before, 4321), max(before, 4321)
			if int64(p.Height) < lo || int64(p.Height) > hi {
				t.Fatalf("smooth flatten moved %d to %d, outside [%d, %d]", before, p.Height, lo, hi)
			}
		}
	}
}

func TestBrushApplyIsDeterministicAndLeavesItsInputAlone(t *testing.T) {
	meta := brushMeta()
	b := Brush{Op: BrushRaise, X: 12800 + 77, Y: 6400 - 33, Radius: 1600, Falloff: FalloffLinear, Strength: 500}
	chunks := brushChunks(t, meta, b.Chunks(meta))
	before := map[ChunkKey]Chunk{}
	for k, c := range chunks {
		before[k] = c.Clone()
	}
	first, err := b.Apply(meta, FullRange, chunks)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Apply(meta, FullRange, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if !equalChanges(first, second) {
		t.Fatal("two applications differ")
	}
	for k, c := range chunks {
		if !equalChunk(c, before[k]) {
			t.Fatalf("Apply changed its input chunk %v", k)
		}
	}
}

func equalChanges(a, b []ChunkChange) bool {
	return slices.EqualFunc(a, b, func(x, y ChunkChange) bool { return x.Key == y.Key && slices.Equal(x.Points, y.Points) })
}

func TestBrushRefusals(t *testing.T) {
	meta := brushMeta()
	ok := Brush{Op: BrushRaise, X: 100, Y: 100, Radius: 300, Strength: 10}
	cases := map[string]struct {
		b      Brush
		limits Limits
		want   error
	}{
		"no op":             {Brush{X: 100, Y: 100, Radius: 300}, FullRange, ErrInvalidBrush},
		"zero strength":     {Brush{Op: BrushLower, X: 100, Y: 100, Radius: 300}, FullRange, ErrInvalidBrush},
		"zero radius":       {Brush{Op: BrushPaint, X: 100, Y: 100}, FullRange, ErrInvalidBrush},
		"huge radius":       {Brush{Op: BrushPaint, X: 100, Y: 100, Radius: MaxBrushRadius + 1}, FullRange, ErrInvalidBrush},
		"unknown falloff":   {Brush{Op: BrushPaint, X: 100, Y: 100, Radius: 1, Falloff: 9}, FullRange, ErrInvalidBrush},
		"target too low":    {Brush{Op: BrushFlatten, X: 100, Y: 100, Radius: 300, Target: 5}, Limits{Bedrock: 10, Ceiling: 20}, ErrInvalidBrush},
		"inverted limits":   {ok, Limits{Bedrock: 30, Ceiling: 20}, ErrInvalidBrush},
		"centre off map":    {Brush{Op: BrushPaint, X: -1, Y: 100, Radius: 300}, FullRange, ErrOutOfMap},
		"centre past edge":  {Brush{Op: BrushPaint, X: 4 * ChunkSize, Y: 100, Radius: 300}, FullRange, ErrOutOfMap},
		"missing the chunk": {ok, FullRange, ErrNotLoaded},
	}
	for name, tc := range cases {
		if _, err := tc.b.Apply(meta, tc.limits, map[ChunkKey]Chunk{}); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	short := map[ChunkKey]Chunk{{0, 0}: {Heights: make([]uint16, 3), Materials: make([]byte, ChunkPoints)}}
	if _, err := ok.Apply(meta, FullRange, short); !errors.Is(err, ErrInvalidChunk) {
		t.Fatalf("a malformed chunk: %v", err)
	}
}

// 方案 §4：玩家笔刷半径上限 16m（1600 单位）时一次最多覆盖 4 块（直径 32m 小于块边长 64m）。
func TestAPlayerSizedBrushNeverCoversMoreThanFourChunks(t *testing.T) {
	meta := Meta{BaseRevision: 1, ChunksX: 5, ChunksY: 5}
	most := 0
	for y := int64(0); y < 3*ChunkSize; y += 97 {
		for x := int64(0); x < 3*ChunkSize; x += 89 {
			n := len(Brush{Op: BrushPaint, X: x, Y: y, Radius: 1600}.Chunks(meta))
			most = max(most, n)
		}
	}
	if most != 4 {
		t.Fatalf("a 1600 brush covers at most %d chunks, want 4", most)
	}
	// 半径 1 落在非采样点上：一个点也不覆盖。
	if n := len((Brush{Op: BrushPaint, X: 150, Y: 150, Radius: 1}).Chunks(meta)); n != 0 {
		t.Fatalf("a radius-1 brush between samples covers %d chunks", n)
	}
}

// ---- 客户端金样 client/spec/terrain-brush.json ----

type goldenBrushFile struct {
	Description string              `json:"description"`
	Pack        string              `json:"pack"`
	WeightScale int64               `json:"weight_scale"`
	ISqrt       [][2]int64          `json:"isqrt"`
	Weights     []goldenBrushWeight `json:"weights"`
	Cases       []goldenBrushCase   `json:"cases"`
}

type goldenBrushWeight struct {
	Falloff string `json:"falloff"`
	Radius  int64  `json:"radius"`
	D2      int64  `json:"d2"`
	Weight  int64  `json:"weight"`
}

type goldenBrushCase struct {
	Name     string              `json:"name"`
	Op       string              `json:"op"`
	X        int64               `json:"x"`
	Y        int64               `json:"y"`
	Radius   int64               `json:"radius"`
	Falloff  string              `json:"falloff"`
	Strength uint16              `json:"strength"`
	Target   uint16              `json:"target"`
	Material byte                `json:"material"`
	Bedrock  uint16              `json:"bedrock"`
	Ceiling  uint16              `json:"ceiling"`
	Covered  [][2]int32          `json:"covered_chunks"`
	Changes  []goldenBrushChange `json:"changes"`
}

type goldenBrushChange struct {
	X             int32 `json:"x"`
	Y             int32 `json:"y"`
	Points        int   `json:"points"`
	HeightChanges int   `json:"height_changes"`
	// SHA256 是改动列表的 SHA-256：每个改动点依次写 index（uint16 小端）、height（uint16 小端）、material（1 字节）。
	SHA256 string `json:"sha256"`
	// List 是完整改动列表，只在改动点不多（≤ 40）时给出，便于对照出错的点。
	List []goldenBrushPoint `json:"list,omitempty"`
}

type goldenBrushPoint struct {
	Index    int32  `json:"index"`
	Height   uint16 `json:"height"`
	Material byte   `json:"material"`
}

// goldenBrushCases 是金样的笔刷：三种衰减、四种操作、块交点、地图边缘、上下限钳制、边界情形块（0 / 65535 棋盘格）、
// 非采样点中心、只覆盖一个点与一个点也不覆盖。
func goldenBrushCases() []struct {
	name   string
	b      Brush
	limits Limits
} {
	edge := ChunkKey{2, 1}.Origin()
	return []struct {
		name   string
		b      Brush
		limits Limits
	}{
		{"raise_constant_inside_a_chunk", Brush{Op: BrushRaise, X: 3200, Y: 3200, Radius: 250, Strength: 37}, FullRange},
		{"raise_linear_on_a_four_chunk_corner", Brush{Op: BrushRaise, X: 6400, Y: 6400, Radius: 500, Falloff: FalloffLinear, Strength: 300}, FullRange},
		{"raise_smooth_player_max_radius", Brush{Op: BrushRaise, X: 6400, Y: 3200, Radius: 1600, Falloff: FalloffSmooth, Strength: 120}, FullRange},
		{"lower_linear_on_a_four_chunk_corner", Brush{Op: BrushLower, X: 12750, Y: 6400, Radius: 1600, Falloff: FalloffLinear, Strength: 900}, FullRange},
		{"lower_clamped_at_bedrock", Brush{Op: BrushLower, X: 4000, Y: 9000, Radius: 900, Strength: 60000}, Limits{Bedrock: 2000, Ceiling: 60000}},
		{"raise_clamped_at_ceiling", Brush{Op: BrushRaise, X: 4000, Y: 9000, Radius: 900, Falloff: FalloffSmooth, Strength: 60000}, Limits{Bedrock: 2000, Ceiling: 30000}},
		{"flatten_constant", Brush{Op: BrushFlatten, X: 8888, Y: 2222, Radius: 800, Target: 3456}, FullRange},
		{"flatten_smooth", Brush{Op: BrushFlatten, X: 8888, Y: 2222, Radius: 1200, Falloff: FalloffSmooth, Target: 3456}, FullRange},
		{"flatten_linear_across_a_shared_edge", Brush{Op: BrushFlatten, X: 6400 + 30, Y: 5000, Radius: 1000, Falloff: FalloffLinear, Target: 100}, FullRange},
		{"paint_on_a_corner", Brush{Op: BrushPaint, X: 6400, Y: 6400, Radius: 500, Material: 222}, FullRange},
		{"edge_chunk_raise_checkerboard", Brush{Op: BrushRaise, X: edge.X + 800, Y: edge.Y + 1200, Radius: 1000, Falloff: FalloffLinear, Strength: 40000}, FullRange},
		{"edge_chunk_lower_checkerboard", Brush{Op: BrushLower, X: edge.X + 800, Y: edge.Y + 1200, Radius: 1000, Falloff: FalloffSmooth, Strength: 40000}, FullRange},
		{"edge_chunk_flatten_slope", Brush{Op: BrushFlatten, X: edge.X + 2400, Y: edge.Y + 3000, Radius: 700, Falloff: FalloffLinear, Target: 30000}, FullRange},
		{"map_corner_clipped", Brush{Op: BrushRaise, X: 50, Y: 50, Radius: 700, Falloff: FalloffLinear, Strength: 1000}, FullRange},
		{"centre_between_samples", Brush{Op: BrushRaise, X: 3333, Y: 4447, Radius: 333, Falloff: FalloffSmooth, Strength: 777}, FullRange},
		{"radius_one_on_a_sample", Brush{Op: BrushRaise, X: 1200, Y: 1300, Radius: 1, Strength: 5}, FullRange},
		{"radius_one_between_samples", Brush{Op: BrushRaise, X: 1250, Y: 1300, Radius: 1, Strength: 5}, FullRange},
	}
}

func buildBrushGolden(t *testing.T) []byte {
	t.Helper()
	p, err := OpenPack(goldenPackPath)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	meta := p.Meta()
	base := map[ChunkKey]Chunk{}
	for key := range ChunksIn(meta.Bounds()) {
		if base[key], err = p.ReadChunk(key); err != nil {
			t.Fatal(err)
		}
	}
	out := goldenBrushFile{
		Description: "Terrain brush golden for client prediction (gameplay/terrain/brush.go, README). Each case applies one brush to the " +
			"base chunks of terrain-golden.pack (not to the previous case's result). World units are centimetres, heights raw (1 = 10cm). " +
			"covered_chunks are the chunks holding a sample point with d2 <= radius^2; changes lists only chunks whose points changed, " +
			"sha256 is over each changed point as index (u16 LE), height (u16 LE), material (u8), in index order.",
		Pack:        "terrain-golden.pack",
		WeightScale: WeightScale,
	}
	for _, n := range []int64{0, 1, 2, 3, 4, 15, 16, 17, 99, 100, 101, 10000, 2559999, 2560000, 2560001, 1 << 40, math.MaxInt64} {
		out.ISqrt = append(out.ISqrt, [2]int64{n, ISqrt(n)})
	}
	for _, f := range []Falloff{FalloffConstant, FalloffLinear, FalloffSmooth} {
		for _, r := range []int64{1, 333, 1600} {
			for _, d2 := range []int64{0, 1, r * r / 4, r*r/2 + 7, r*r - 1, r * r, r*r + 1} {
				out.Weights = append(out.Weights, goldenBrushWeight{Falloff: f.String(), Radius: r, D2: d2, Weight: f.Weight(d2, r)})
			}
		}
	}
	for _, tc := range goldenBrushCases() {
		changes, err := tc.b.Apply(meta, tc.limits, base)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		gc := goldenBrushCase{Name: tc.name, Op: tc.b.Op.String(), X: tc.b.X, Y: tc.b.Y, Radius: tc.b.Radius, Falloff: tc.b.Falloff.String(),
			Strength: tc.b.Strength, Target: tc.b.Target, Material: tc.b.Material, Bedrock: tc.limits.Bedrock, Ceiling: tc.limits.Ceiling,
			Covered: [][2]int32{}, Changes: []goldenBrushChange{}}
		for _, key := range tc.b.Chunks(meta) {
			gc.Covered = append(gc.Covered, [2]int32{key.X, key.Y})
		}
		for _, change := range changes {
			raw := make([]byte, 0, 5*len(change.Points))
			for _, pt := range change.Points {
				raw = binary.LittleEndian.AppendUint16(raw, uint16(pt.Index))
				raw = binary.LittleEndian.AppendUint16(raw, pt.Height)
				raw = append(raw, pt.Material)
			}
			gch := goldenBrushChange{X: change.Key.X, Y: change.Key.Y, Points: len(change.Points),
				HeightChanges: change.HeightChanges(base[change.Key]), SHA256: hexSum(raw)}
			if len(change.Points) <= 40 {
				for _, pt := range change.Points {
					gch.List = append(gch.List, goldenBrushPoint{Index: pt.Index, Height: pt.Height, Material: pt.Material})
				}
			}
			gc.Changes = append(gc.Changes, gch)
		}
		out.Cases = append(out.Cases, gc)
	}
	content, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(content, '\n')
}

// 笔刷的客户端金样：每个用例在金样包的原始块上应用一次笔刷，记录覆盖的块、每块改动点数与改动列表的 SHA-256
// （点少时附完整列表），另有整数开方与三种衰减权重的对照表。C# / C++ 的本地预测按同一算法逐项对照。
// 改了笔刷算法时用 -update 重新生成（只写 terrain-brush.json：go test ./gameplay/terrain -run TestClientBrushGolden -update），
// 并让客户端实现重新对照。
func TestClientBrushGolden(t *testing.T) {
	content := buildBrushGolden(t)
	if *updateGolden {
		if err := os.WriteFile(goldenBrushPath, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(goldenBrushPath)
	if err != nil {
		t.Fatal(err)
	}
	actual = bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(content, actual) {
		t.Fatal("client brush golden changed: regenerate deliberately with -update and re-check the client implementations")
	}
	// 金样要有内容：每种操作都有改动、钳制用例真的碰到上下限、最后一个用例一个点也不覆盖。
	var f goldenBrushFile
	if err := json.Unmarshal(actual, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		if len(c.Changes) == 0 && c.Name != "radius_one_between_samples" {
			t.Fatalf("golden case %s changes nothing", c.Name)
		}
	}
	if last := f.Cases[len(f.Cases)-1]; len(last.Covered) != 0 {
		t.Fatalf("%s covers %v", last.Name, last.Covered)
	}
}

func BenchmarkBrushApplyPlayerRadius(b *testing.B) {
	meta := brushMeta()
	for _, tc := range []struct {
		name string
		br   Brush
	}{
		{"r5m_linear_1chunk", Brush{Op: BrushRaise, X: 3200, Y: 3200, Radius: 500, Falloff: FalloffLinear, Strength: 50}},
		{"r16m_smooth_4chunks", Brush{Op: BrushRaise, X: 6400, Y: 6400, Radius: 1600, Falloff: FalloffSmooth, Strength: 50}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			gen := Generated(meta, goldenSeed)
			chunks := map[ChunkKey]Chunk{}
			for _, key := range tc.br.Chunks(meta) {
				chunks[key], _ = gen(key)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := tc.br.Apply(meta, FullRange, chunks); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
