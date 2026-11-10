package terrain

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// testMeta 是单测用的小地图：3 × 2 块，水位线在生成地形的中段。
var testMeta = Meta{BaseRevision: 3, ChunksX: 3, ChunksY: 2, HeightBase: -12345, WaterLevel: -12345 + 6000*HeightStep}

const testSeed = 42

// writeTestPack 用生成器写一个包并打开，测试结束时关闭。
func writeTestPack(t testing.TB, meta Meta, src ChunkSource) (*Pack, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "map.pack")
	if err := WritePackFile(path, meta, src); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPack(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, path
}

func equalChunk(a, b Chunk) bool {
	return slices.Equal(a.Heights, b.Heights) && slices.Equal(a.Materials, b.Materials)
}

// 写出的包打开后：包头参数原样、每块解码后与来源逐点相同、索引里的 SHA-256 与块数据一致。
func TestPackRoundTrip(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	if !p.Meta().Equal(testMeta) {
		t.Fatalf("meta %+v, want %+v", p.Meta(), testMeta)
	}
	for key := range ChunksIn(testMeta.Bounds()) {
		want, _ := src(key)
		got, err := p.ReadChunk(key)
		if err != nil {
			t.Fatal(err)
		}
		if !equalChunk(got, want) {
			t.Fatalf("chunk %v differs after the round trip", key)
		}
	}
	if _, err := p.ReadChunk(ChunkKey{3, 0}); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("read outside the map: %v", err)
	}
}

// 编码覆盖极端数据：高度在 0 与 65535 之间跳变（平面预测误差最大、每点 3 字节）、全 65535、随机材质。
func TestCodecExtremes(t *testing.T) {
	cliff := NewChunk()
	for k := range cliff.Heights {
		if (k/ChunkSamples+k%ChunkSamples)%2 == 0 {
			cliff.Heights[k] = 65535
		}
		cliff.Materials[k] = byte(k * 7)
	}
	top := NewChunk()
	for k := range top.Heights {
		top.Heights[k] = 65535
		top.Materials[k] = 255
	}
	for name, c := range map[string]Chunk{"cliff": cliff, "top": top, "zero": NewChunk()} {
		data, err := encodeChunk(c)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeChunk(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !equalChunk(got, c) {
			t.Fatalf("%s differs after the round trip", name)
		}
	}
}

// 明文多一字节、少一字节、高度越界都算损坏，不会解出半块数据。
func TestDecodeRejectsMalformedPayload(t *testing.T) {
	deflate := func(plain []byte) []byte {
		var out bytes.Buffer
		w, _ := flate.NewWriter(&out, flate.DefaultCompression)
		w.Write(plain)
		w.Close()
		return out.Bytes()
	}
	good := make([]byte, 0, 2*ChunkPoints)
	for range ChunkPoints {
		good = binary.AppendUvarint(good, 0)
	}
	good = append(good, make([]byte, ChunkPoints)...)
	if _, err := decodeChunk(deflate(good)); err != nil {
		t.Fatalf("well-formed payload rejected: %v", err)
	}
	negative := slices.Clone(good)
	negative[0] = 1 // zigzag 1 = −1：第一个点 0 − 1 越界
	for name, plain := range map[string][]byte{
		"extra byte":     append(slices.Clone(good), 0),
		"missing byte":   good[:len(good)-1],
		"negative point": negative,
	} {
		if _, err := decodeChunk(deflate(plain)); !errors.Is(err, errCorrupt) {
			t.Errorf("%s: %v, want errCorrupt", name, err)
		}
	}
	if _, err := decodeChunk([]byte("not deflate")); !errors.Is(err, errCorrupt) {
		t.Errorf("garbage: %v", err)
	}
}

// 块数据的一个字节坏了：只有那一块返回 ErrCorruptChunk，其他块照常读出。
func TestCorruptChunkIsReportedNotDefaulted(t *testing.T) {
	_, path := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	p, err := OpenPack(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := ChunkKey{1, 1}
	e := p.index[testMeta.index(bad)]
	p.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[e.offset+uint64(e.length)/2] ^= 0x40
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPack(path) // 包头与索引表没坏，照常打开
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for key := range ChunksIn(testMeta.Bounds()) {
		_, err := p.ReadChunk(key)
		switch {
		case key == bad && !errors.Is(err, ErrCorruptChunk):
			t.Fatalf("corrupt chunk %v: %v, want ErrCorruptChunk", key, err)
		case key != bad && err != nil:
			t.Fatalf("intact chunk %v: %v", key, err)
		}
	}
	v := NewView(p)
	if err := v.LoadBase(bad); !errors.Is(err, ErrCorruptChunk) {
		t.Fatalf("LoadBase of a corrupt chunk: %v", err)
	}
	if _, ok := v.Version(bad); ok {
		t.Fatal("a corrupt chunk was installed")
	}
}

// 包头或索引表任一字节坏了、文件被截短、版本不对，都在打开时拒绝。
func TestOpenRejectsBadHeaderAndIndex(t *testing.T) {
	_, path := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dataStart := packHeaderSize + testMeta.Chunks()*packIndexEntry
	mutate := func(name string, f func([]byte) []byte, want error) {
		t.Helper()
		bad := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(bad, f(slices.Clone(raw)), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := OpenPack(bad)
		if !errors.Is(err, want) {
			if p != nil {
				p.Close()
			}
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	flip := func(at int) func([]byte) []byte { return func(b []byte) []byte { b[at] ^= 1; return b } }
	mutate("magic", flip(0), ErrBadPack)
	mutate("base-revision", flip(16), ErrBadPack) // 字段本身合法，靠 metaSHA256 发现
	mutate("water-level", flip(48), ErrBadPack)
	mutate("index-sum", flip(packHeaderSize+12), ErrBadPack)
	mutate("meta-hash", flip(packMetaHashOff), ErrBadPack)
	mutate("truncated-index", func(b []byte) []byte { return b[:dataStart-1] }, ErrBadPack)
	mutate("truncated-data", func(b []byte) []byte { return b[:len(b)-1] }, ErrBadPack)                              // 最后一块越出文件
	mutate("version-1", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[8:], 1); return b }, ErrPackVersion) // 版本 1 的包（没有区域水位）不兼容
	mutate("spacing", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[36:], 50); return b }, ErrPackVersion)
}

// 多个 goroutine 并发读同一个包（-race 下跑），每块结果都与来源一致。
func TestConcurrentChunkReads(t *testing.T) {
	src := Generated(testMeta, testSeed)
	p, _ := writeTestPack(t, testMeta, src)
	keys := slices.Collect(ChunksIn(testMeta.Bounds()))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := range 8 {
		wg.Go(func() {
			for i := range 30 {
				key := keys[(g+i)%len(keys)]
				got, err := p.ReadChunk(key)
				if err != nil {
					errs <- err
					return
				}
				if want, _ := src(key); !equalChunk(got, want) {
					errs <- errors.New("chunk " + key.String() + " differs")
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

// Close 可重复；之后的读返回 os.ErrClosed。
func TestPackClose(t *testing.T) {
	p, _ := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := p.ReadChunk(ChunkKey{}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("read after Close: %v", err)
	}
}

// 来源出错或给出形状不对的块：WritePackFile 失败、不留临时文件、原文件不变。
func TestWritePackFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "map.pack")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	gen := Generated(testMeta, testSeed)
	failing := func(key ChunkKey) (Chunk, error) {
		if key == (ChunkKey{2, 1}) {
			return Chunk{Heights: make([]uint16, 10), Materials: make([]byte, ChunkPoints)}, nil
		}
		return gen(key)
	}
	if err := WritePackFile(path, testMeta, failing); !errors.Is(err, ErrInvalidChunk) {
		t.Fatalf("write with a malformed chunk: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left files behind: %v", entries)
	}
	if old, _ := os.ReadFile(path); string(old) != "old" {
		t.Fatalf("original file changed: %q", old)
	}
}

// 写包与并行度无关：同一来源两次写出逐字节相同。
func TestWritePackIsDeterministic(t *testing.T) {
	_, a := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	_, b := writeTestPack(t, testMeta, Generated(testMeta, testSeed))
	ra, _ := os.ReadFile(a)
	rb, _ := os.ReadFile(b)
	if !bytes.Equal(ra, rb) {
		t.Fatal("two writes of the same source differ")
	}
}

// RAW16 + RAW8 转换：小端 / 大端、翻转 Y、按 MaxHeight 换算、图外采样复制图边。
func TestRAWImport(t *testing.T) {
	meta := Meta{BaseRevision: 1, ChunksX: 2, ChunksY: 2}
	const w, h = 70, 65 // 不是块的整数倍：右块大部分列、上块大部分行在图外
	pixel := func(x, y int) uint16 { return uint16(x*100 + y) }
	material := func(x, y int) byte { return byte(x ^ y) }
	build := func(bigEndian, flipY bool) (heights, materials []byte) {
		for row := range h {
			y := row
			if flipY {
				y = h - 1 - row
			}
			for x := range w {
				if bigEndian {
					heights = binary.BigEndian.AppendUint16(heights, pixel(x, y))
				} else {
					heights = binary.LittleEndian.AppendUint16(heights, pixel(x, y))
				}
				materials = append(materials, material(x, y))
			}
		}
		return heights, materials
	}
	for _, mode := range []struct{ big, flip bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		hb, mb := build(mode.big, mode.flip)
		src, err := RAWImport{Heights: bytes.NewReader(hb), Materials: bytes.NewReader(mb), Width: w, Height: h,
			BigEndian: mode.big, FlipY: mode.flip}.Source(meta)
		if err != nil {
			t.Fatal(err)
		}
		for key := range ChunksIn(meta.Bounds()) {
			c, err := src(key)
			if err != nil {
				t.Fatal(err)
			}
			for j := range ChunkSamples {
				for i := range ChunkSamples {
					gx, gy := min(int(key.X)*64+i, w-1), min(int(key.Y)*64+j, h-1)
					k := j*ChunkSamples + i
					if c.Heights[k] != pixel(gx, gy) || c.Materials[k] != material(gx, gy) {
						t.Fatalf("mode %+v chunk %v (%d,%d): got %d/%d, want %d/%d", mode, key, i, j,
							c.Heights[k], c.Materials[k], pixel(gx, gy), material(gx, gy))
					}
				}
			}
		}
	}
	// MaxHeight：像素 65535 → MaxHeight 厘米，四舍五入到 10cm；没有材质图时材质为 0。
	full := binary.LittleEndian.AppendUint16(binary.LittleEndian.AppendUint16(nil, 65535), 32768)
	src, err := RAWImport{Heights: bytes.NewReader(full), Width: 2, Height: 1, MaxHeight: 100000}.Source(meta)
	if err != nil {
		t.Fatal(err)
	}
	c, err := src(ChunkKey{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Heights[0] != 10000 || c.Heights[1] != 5000 || c.Materials[0] != 0 {
		t.Fatalf("scaled heights %d, %d (want 10000, 5000), material %d", c.Heights[0], c.Heights[1], c.Materials[0])
	}
	// 图比地图大、文件比声明的短：分别在 Source 与取块时报错。
	if _, err := (RAWImport{Heights: bytes.NewReader(nil), Width: 129, Height: 1}).Source(meta); err == nil {
		t.Fatal("an image wider than the map was accepted")
	}
	short, _ := RAWImport{Heights: bytes.NewReader(make([]byte, 10)), Width: 64, Height: 64}.Source(meta)
	if _, err := short(ChunkKey{}); err == nil {
		t.Fatal("a short RAW file was accepted")
	}
}

// 平地：每块每点都是同一高度与材质，图外的块报 ErrOutOfMap；写成包后整张图处处可走（与 Generated 相反：缺省的起伏
// 地形在 45° 规则下大半的边走不通，所以 demo 的缺省包改用平地）。
func TestFlatIsTheSameEverywhereAndWalkable(t *testing.T) {
	meta := Meta{BaseRevision: 1, ChunksX: 2, ChunksY: 1, HeightBase: -10000, WaterLevel: 40000}
	src := Flat(meta, 5010, GenGrass)
	for _, key := range []ChunkKey{{X: 0, Y: 0}, {X: 1, Y: 0}} {
		c, err := src(key)
		if err != nil {
			t.Fatal(err)
		}
		for i := range c.Heights {
			if c.Heights[i] != 5010 || c.Materials[i] != GenGrass {
				t.Fatalf("chunk %v point %d: height %d material %d", key, i, c.Heights[i], c.Materials[i])
			}
		}
	}
	if _, err := src(ChunkKey{X: 2}); !errors.Is(err, ErrOutOfMap) {
		t.Fatalf("a chunk outside the map: %v", err)
	}
	pack, _ := writeTestPack(t, meta, src)
	w := Walker{View: NewView(pack), Rules: DefaultWalkRules}
	if err := w.View.LoadBase(ChunkKey{X: 0}); err != nil {
		t.Fatal(err)
	}
	if err := w.View.LoadBase(ChunkKey{X: 1}); err != nil {
		t.Fatal(err)
	}
	for gy := int64(0); gy < ChunkSamples; gy++ {
		for gx := int64(0); gx+1 < 2*ChunkSamples; gx++ {
			if _, ok := w.Step(spatial.Point{X: gx, Y: gy}, spatial.Point{X: gx + 1, Y: gy}); !ok {
				t.Fatalf("flat ground: no step from (%d, %d) east", gx, gy)
			}
		}
	}
	if g, err := w.View.Probe(5000, 3000); err != nil || g.Water || g.Height != 40100 {
		t.Fatalf("flat ground 1m above the water: %+v, %v", g, err)
	}
}
