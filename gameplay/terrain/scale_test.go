package terrain

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 1w × 1w 地图（157 × 157 块）的规模测量，环境变量门控：
//
//	ROOST_TERRAIN_SCALE=1 GOWORK=off go test -count=1 -timeout 0 -run TestScale10k -v ./gameplay/terrain
//
// ROOST_TERRAIN_SCALE_DIR 指定写包的目录（缺省 t.TempDir()，包约 70MB，结束时删除）。量的是：整图生成并写包、
// 打开（只读索引）、单块加载 P50 / P99、装满一个格（48 × 48 块）的耗时与堆内存、已装块上的查询耗时。
// 包刚写完，读块时在页缓存里（热读）；冷盘读没有测。
func TestScale10k(t *testing.T) {
	if os.Getenv("ROOST_TERRAIN_SCALE") == "" {
		t.Skip("set ROOST_TERRAIN_SCALE=1 to run the 10k × 10k terrain measurement")
	}
	dir := os.Getenv("ROOST_TERRAIN_SCALE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	path := filepath.Join(dir, "terrain-10k.pack")
	defer os.Remove(path)
	meta := Meta{BaseRevision: 1, ChunksX: 157, ChunksY: 157, WaterLevel: 5500 * HeightStep}
	t.Logf("env: %s/%s, GOMAXPROCS %d, %s", runtime.GOOS, runtime.GOARCH, runtime.GOMAXPROCS(0), runtime.Version())

	start := time.Now()
	if err := WritePackFile(path, meta, Generated(meta, 20261108)); err != nil {
		t.Fatal(err)
	}
	t.Logf("generate + write: %v (%d chunks)", time.Since(start).Round(time.Millisecond), meta.Chunks())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pack size: %d bytes (%.1f MiB), %.0f bytes/chunk including header and index",
		info.Size(), float64(info.Size())/(1<<20), float64(info.Size())/float64(meta.Chunks()))

	var opens []time.Duration
	var p *Pack
	for range 7 {
		start := time.Now()
		q, err := OpenPack(path)
		if err != nil {
			t.Fatal(err)
		}
		opens = append(opens, time.Since(start))
		if p == nil {
			p = q
		} else {
			q.Close()
		}
	}
	defer p.Close()
	slices.Sort(opens)
	t.Logf("open (header + %d-entry index, SHA-256 over it): median %v, min %v, max %v (7 opens)",
		meta.Chunks(), opens[3].Round(time.Microsecond), opens[0].Round(time.Microsecond), opens[6].Round(time.Microsecond))

	rng := rand.New(rand.NewPCG(1, 2))
	var loads []time.Duration
	for range 5000 {
		key := ChunkKey{X: rng.Int32N(meta.ChunksX), Y: rng.Int32N(meta.ChunksY)}
		start := time.Now()
		if _, err := p.ReadChunk(key); err != nil {
			t.Fatal(err)
		}
		loads = append(loads, time.Since(start))
	}
	slices.Sort(loads)
	pct := func(d []time.Duration, q float64) time.Duration {
		return d[int(q*float64(len(d)-1))].Round(100 * time.Nanosecond)
	}
	t.Logf("single chunk ReadChunk (ReadAt + SHA-256 + inflate + decode, page cache warm), 5000 random chunks: P50 %v, P99 %v, max %v",
		pct(loads, 0.5), pct(loads, 0.99), loads[len(loads)-1].Round(time.Microsecond))

	cell := spatial.Rect{Min: spatial.Point{X: 48 * ChunkSize, Y: 48 * ChunkSize}, Max: spatial.Point{X: 96 * ChunkSize, Y: 96 * ChunkSize}}
	keys := slices.Collect(ChunksIn(cell))
	for _, workers := range []int{1, 8} {
		heapBefore := heapInUse()
		v := NewView(p)
		start := time.Now()
		var wg sync.WaitGroup
		next := make(chan ChunkKey)
		errs := make(chan error, workers)
		for range workers {
			wg.Go(func() {
				for key := range next {
					if err := v.LoadBase(key); err != nil {
						errs <- err
						return
					}
				}
			})
		}
		for _, key := range keys {
			next <- key
		}
		close(next)
		wg.Wait()
		elapsed := time.Since(start)
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		heapAfter := heapInUse()
		t.Logf("fill one cell (48 × 48 = %d chunks, LoadBase, %d workers): %v, heap +%.1f MiB (%.0f bytes/chunk)",
			v.Len(), workers, elapsed.Round(time.Millisecond), float64(heapAfter-heapBefore)/(1<<20),
			float64(heapAfter-heapBefore)/float64(v.Len()))
		if workers == 8 {
			measureQueries(t, v, cell)
		}
		runtime.KeepAlive(v)
	}
}

// 块编码选择的依据（同一门控）：1w 地图上 8 个块，比较原始小端直接 DEFLATE 与三种预测（左邻、下邻、平面）的
// varint 明文再 DEFLATE 的平均字节数。正式编码用平面 + BestCompression（codec.go）。
func TestScaleCodecChoice(t *testing.T) {
	if os.Getenv("ROOST_TERRAIN_SCALE") == "" {
		t.Skip("set ROOST_TERRAIN_SCALE=1 to run the codec comparison")
	}
	src := Generated(Meta{ChunksX: 157, ChunksY: 157, WaterLevel: 5500 * HeightStep}, 20261108)
	predictors := []struct {
		name string
		f    func(h []uint16, k int) int64
	}{
		{"left", func(h []uint16, k int) int64 {
			if k%ChunkSamples == 0 {
				return 0
			}
			return int64(h[k-1])
		}},
		{"down", func(h []uint16, k int) int64 {
			if k < ChunkSamples {
				return 0
			}
			return int64(h[k-ChunkSamples])
		}},
		{"plane", predict},
	}
	deflated := func(b []byte, level int) int {
		var out bytes.Buffer
		w, _ := flate.NewWriter(&out, level)
		w.Write(b)
		w.Close()
		return out.Len()
	}
	keys := []ChunkKey{{0, 0}, {10, 20}, {78, 78}, {100, 33}, {150, 150}, {40, 120}, {120, 5}, {60, 90}}
	sums := map[string]int{}
	for _, key := range keys {
		c, err := src(key)
		if err != nil {
			t.Fatal(err)
		}
		le := make([]byte, 0, 2*ChunkPoints)
		for _, h := range c.Heights {
			le = binary.LittleEndian.AppendUint16(le, h)
		}
		sums["raw little-endian, DEFLATE default"] += deflated(append(le, c.Materials...), flate.DefaultCompression)
		for _, p := range predictors {
			var plain []byte
			for k, h := range c.Heights {
				d := int64(h) - p.f(c.Heights, k)
				plain = binary.AppendUvarint(plain, uint64(d<<1^d>>63))
			}
			sums[p.name+" varint heights only, no DEFLATE"] += len(plain)
			plain = append(plain, c.Materials...)
			sums[p.name+" + DEFLATE default"] += deflated(plain, flate.DefaultCompression)
			sums[p.name+" + DEFLATE best"] += deflated(plain, flate.BestCompression)
		}
	}
	names := slices.Sorted(maps.Keys(sums))
	for _, name := range names {
		t.Logf("%-40s %5d bytes/chunk (heights + materials unless noted)", name, sums[name]/len(keys))
	}
}

// measureQueries 在已装满的格上量随机位置的 Height / Probe 单线程耗时。
func measureQueries(t *testing.T, v *View, cell spatial.Rect) {
	rng := rand.New(rand.NewPCG(3, 4))
	const n = 1_000_000
	xs, ys := make([]int64, n), make([]int64, n)
	for i := range n {
		// 留出一米，避免查到格外未装的邻块
		xs[i] = cell.Min.X + rng.Int64N(cell.Max.X-cell.Min.X-SampleSpacing)
		ys[i] = cell.Min.Y + rng.Int64N(cell.Max.Y-cell.Min.Y-SampleSpacing)
	}
	for _, q := range []struct {
		name string
		f    func(x, y int64) error
	}{
		{"Height", func(x, y int64) error { _, err := v.Height(x, y); return err }},
		{"Probe", func(x, y int64) error { _, err := v.Probe(x, y); return err }},
	} {
		start := time.Now()
		for i := range n {
			if err := q.f(xs[i], ys[i]); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("%s on a full cell, %d random points, one goroutine: %.0f ns/op", q.name, n, float64(time.Since(start).Nanoseconds())/n)
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// 小地图上的微基准（不门控，`-bench` 时才跑）：编码、读块、查询。
func benchView(b *testing.B) (*Pack, *View) {
	meta := Meta{BaseRevision: 1, ChunksX: 8, ChunksY: 8, WaterLevel: 5500 * HeightStep}
	p, _ := writeTestPack(b, meta, Generated(meta, 1))
	v := NewView(p)
	loadAll(b, v)
	return p, v
}

func BenchmarkEncodeChunk(b *testing.B) {
	c, _ := Generated(Meta{ChunksX: 1, ChunksY: 1}, 1)(ChunkKey{})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := encodeChunk(c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadChunk(b *testing.B) {
	p, _ := benchView(b)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if _, err := p.ReadChunk(ChunkKey{X: int32(i % 8), Y: int32(i / 8 % 8)}); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkHeight(b *testing.B) {
	_, v := benchView(b)
	b.ReportAllocs()
	i := int64(0)
	for b.Loop() {
		if _, err := v.Height(i*7919%(8*ChunkSize-SampleSpacing), i*104729%(8*ChunkSize-SampleSpacing)); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkHeightParallel(b *testing.B) {
	_, v := benchView(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := int64(rand.Int32())
		for pb.Next() {
			if _, err := v.Height(i*7919%(8*ChunkSize-SampleSpacing), i*104729%(8*ChunkSize-SampleSpacing)); err != nil {
				b.Error(err)
				return
			}
			i++
		}
	})
}
