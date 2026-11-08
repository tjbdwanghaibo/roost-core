package terrain

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 世界坐标 → 块与块内下标：块边界两侧、负坐标（向负无穷取整）、共享边归右 / 上块。
func TestPointToChunkAndIndex(t *testing.T) {
	cases := []struct {
		x, y  int64
		key   ChunkKey
		index int
	}{
		{0, 0, ChunkKey{0, 0}, 0},
		{99, 99, ChunkKey{0, 0}, 0},                                     // 同一 1m 格的左下角采样点
		{100, 0, ChunkKey{0, 0}, 1},                                     // 下一个采样点
		{ChunkSize - 1, ChunkSize - 1, ChunkKey{0, 0}, ChunkPoints - 1}, // 块内最后一个 1m 格
		{ChunkSize, 0, ChunkKey{1, 0}, 0},                               // 第 64 列采样点是右块的第 0 列（共享边归右块）
		{0, ChunkSize, ChunkKey{0, 1}, 0},                               // 第 64 行归上块
		{ChunkSize + 250, 2*ChunkSize + 130, ChunkKey{1, 2}, 1*ChunkSamples + 2},
		{-1, -1, ChunkKey{-1, -1}, ChunkPoints - 1}, // 负坐标落在负块的最后一格
		{-ChunkSize, -100, ChunkKey{-1, -1}, (ChunkSamples-1)*ChunkSamples + 0},
		{-ChunkSize - 1, 0, ChunkKey{-2, 0}, ChunkSamples - 1},
		{-50, 50, ChunkKey{-1, 0}, ChunkSamples - 1},
	}
	for _, c := range cases {
		if got := ChunkOf(c.x, c.y); got != c.key {
			t.Errorf("ChunkOf(%d, %d) = %v, want %v", c.x, c.y, got, c.key)
		}
		key, index := PointIndex(c.x, c.y)
		if key != c.key || index != c.index {
			t.Errorf("PointIndex(%d, %d) = %v, %d; want %v, %d", c.x, c.y, key, index, c.key, c.index)
		}
		if o := key.Origin(); c.x < o.X || c.x >= o.X+ChunkSize || c.y < o.Y || c.y >= o.Y+ChunkSize {
			t.Errorf("(%d, %d) outside the bounds of its chunk %v (origin %v)", c.x, c.y, key, o)
		}
	}
}

// 远在地图外的坐标饱和到 int32 的块坐标，不会回绕成地图内的块。
func TestFarCoordinatesDoNotWrapIntoTheMap(t *testing.T) {
	meta := Meta{ChunksX: 4, ChunksY: 4}
	for _, x := range []int64{math.MaxInt64, math.MinInt64, int64(1) << 32 * ChunkSize, -(int64(1) << 32 * ChunkSize)} {
		k := ChunkOf(x, 0)
		pk, _ := PointIndex(x, 0)
		if meta.Contains(k) || meta.Contains(pk) {
			t.Errorf("x = %d maps to chunk %v / %v inside the map", x, k, pk)
		}
	}
}

// 块坐标 ↔ uint64 ID 是双射；地图内的块 ID 能无损放进 int64。
func TestChunkKeyIDRoundTrip(t *testing.T) {
	for _, k := range []ChunkKey{{0, 0}, {1, 0}, {0, 1}, {156, 156}, {-1, -1}, {math.MaxInt32, math.MinInt32}, {math.MinInt32, math.MaxInt32}} {
		if got := KeyFromID(k.ID()); got != k {
			t.Errorf("KeyFromID(%v.ID()) = %v", k, got)
		}
	}
	if (ChunkKey{1, 0}).ID() == (ChunkKey{0, 1}).ID() {
		t.Fatal("(1,0) and (0,1) share an ID")
	}
	if id := (ChunkKey{MaxChunksPerSide - 1, MaxChunksPerSide - 1}).ID(); int64(id) < 0 {
		t.Fatalf("in-map ID %d is negative as int64", id)
	}
}

func TestChunksInRect(t *testing.T) {
	r := spatial.Rect{Min: spatial.Point{X: -1, Y: ChunkSize - 1}, Max: spatial.Point{X: ChunkSize + 1, Y: ChunkSize + 1}}
	got := slices.Collect(ChunksIn(r))
	want := []ChunkKey{{-1, 0}, {0, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("ChunksIn = %v, want %v", got, want)
	}
	// 恰好一个格（48 块见方）：Max 是开区间，不多出一圈。
	cell := spatial.Rect{Max: spatial.Point{X: 48 * ChunkSize, Y: 48 * ChunkSize}}
	if n := len(slices.Collect(ChunksIn(cell))); n != 48*48 {
		t.Fatalf("a 48-chunk cell covers %d chunks", n)
	}
	if n := len(slices.Collect(ChunksIn(spatial.Rect{}))); n != 0 {
		t.Fatalf("empty rect covers %d chunks", n)
	}
}

func TestMetaValidate(t *testing.T) {
	good := Meta{ChunksX: 157, ChunksY: 157}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Meta{
		{ChunksX: 0, ChunksY: 1},
		{ChunksX: 1, ChunksY: MaxChunksPerSide + 1},
		{ChunksX: 1, ChunksY: 1, HeightBase: math.MaxInt32 - 100},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidMeta) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidMeta", bad, err)
		}
	}
	if b := good.Bounds(); b.Max.X != 157*6400 || b.Max.Y != 157*6400 {
		t.Fatalf("bounds %v", b)
	}
}
