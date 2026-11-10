package terrain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
)

// 原始地形包（base pack）格式，版本 2。单文件，全部整数小端：
//
//	偏移 0    包头 128 字节
//	  0  magic         [8]byte  "RTERRAIN"
//	  8  formatVersion uint32   = 2
//	 12  headerSize    uint32   = 128
//	 16  baseRevision  int64
//	 24  chunksX       uint32
//	 28  chunksY       uint32
//	 32  chunkSamples  uint32   = 64
//	 36  sampleSpacing uint32   = 100（世界单位）
//	 40  heightStep    uint32   = 10（世界单位）
//	 44  heightBase    int32
//	 48  waterLevel    int32    全局水位线
//	 52  codec         uint32   = 1（codec.go）
//	 56  indexOffset   uint64   = 128
//	 64  indexEntry    uint32   = 44
//	 68  waterRegions  uint32   区域水位的个数（≤ MaxWaterRegions）
//	 72  保留          [24]byte 全零
//	 96  metaSHA256    [32]byte = SHA-256(包头前 96 字节 ‖ 索引表 ‖ 区域表)
//	偏移 128  索引表 chunksX·chunksY 项，按块行优先（下标 = y·chunksX + x），每项 44 字节：
//	  0  offset uint64   块数据在文件里的偏移
//	  8  length uint32   块数据字节数
//	 12  sha256 [32]byte 块数据（存储字节，即压缩后）的 SHA-256
//	之后      区域表 waterRegions 项，每项 20 字节：minX、minY、maxX、maxY（uint32，块坐标，半开）、level（int32）
//	之后      块数据，codec 1 的编码见 codec.go
//
// 包头、索引表与区域表由 metaSHA256 整体校验（打开时一次），每块由自己的 SHA-256 校验（每次读块）。
// 版本 2（2026-11-10，可编辑地形 P4）在版本 1 上加了区域水位（D3）；版本 1 的包按 ErrPackVersion 拒绝，用新工具重新生成。
const (
	packMagic       = "RTERRAIN"
	packVersion     = 2
	packHeaderSize  = 128
	packIndexEntry  = 44
	packRegionEntry = 20
	packMetaHashOff = 96
	// maxStoredChunk 是单块存储字节的上限：明文最多 16KB，DEFLATE 对不可压数据只增加很少的块头；超过即视为损坏。
	maxStoredChunk = 64 << 10
)

var (
	// ErrBadPack 表示包头或索引表不合法（magic、尺寸、校验和、越界偏移），整个包不可用。
	ErrBadPack = errors.New("terrain: bad base pack")
	// ErrPackVersion 表示包的格式版本或几何常量与本程序不一致：需要用匹配的工具重新生成，不尝试兼容。
	ErrPackVersion = errors.New("terrain: unsupported base pack version")
	// ErrCorruptChunk 表示单块数据损坏（SHA-256 不符、解压或解码失败）。只影响这一块，其他块照常可读；
	// 调用方必须把它当作错误处理，本包不会用默认值代替。
	ErrCorruptChunk = errors.New("terrain: corrupt chunk")
)

// indexEntry 是索引表的一项。
type indexEntry struct {
	offset uint64
	length uint32
	sum    [sha256.Size]byte
}

// Pack 是打开的原始地形包：打开时只读入并校验包头与索引表（1w 地图约 1MB），块数据按需 ReadChunk。
//
// 并发：打开后只读，ReadChunk 可以被任意多个 goroutine 并发调用（底层 os.File.ReadAt 允许并发）。
// Close 要在不再有 ReadChunk 之后调用；Close 之后的 ReadChunk 返回 os.ErrClosed。
type Pack struct {
	f     *os.File
	meta  Meta
	index []indexEntry
}

// OpenPack 打开并校验包头与索引表。包头 / 索引表损坏返回 ErrBadPack，版本或几何常量不符返回 ErrPackVersion。
func OpenPack(path string) (*Pack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	p, err := openPack(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

func openPack(f *os.File) (*Pack, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	head := make([]byte, packHeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, fmt.Errorf("%w: read header: %v", ErrBadPack, err)
	}
	meta, chunks, regions, err := parseHeader(head)
	if err != nil {
		return nil, err
	}
	indexEnd := int64(packHeaderSize) + int64(chunks)*packIndexEntry
	dataStart := indexEnd + int64(regions)*packRegionEntry
	if dataStart > size {
		return nil, fmt.Errorf("%w: file of %d bytes is shorter than its index and water region tables (%d bytes)", ErrBadPack, size, dataStart)
	}
	table := make([]byte, dataStart-packHeaderSize)
	if _, err := f.ReadAt(table, packHeaderSize); err != nil {
		return nil, fmt.Errorf("%w: read index: %v", ErrBadPack, err)
	}
	if sum := metaSum(head, table); !bytes.Equal(sum[:], head[packMetaHashOff:]) {
		return nil, fmt.Errorf("%w: header / index checksum mismatch", ErrBadPack)
	}
	meta.WaterRegions = decodeWaterRegions(table[indexEnd-packHeaderSize:], regions)
	if err := meta.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPack, err)
	}
	index := make([]indexEntry, chunks)
	for i := range index {
		e := table[i*packIndexEntry:]
		index[i].offset = binary.LittleEndian.Uint64(e)
		index[i].length = binary.LittleEndian.Uint32(e[8:])
		copy(index[i].sum[:], e[12:12+sha256.Size])
		if index[i].length == 0 || index[i].length > maxStoredChunk || index[i].offset < uint64(dataStart) ||
			index[i].offset > uint64(size) || uint64(index[i].length) > uint64(size)-index[i].offset {
			return nil, fmt.Errorf("%w: index entry %d (offset %d, length %d) outside the data area [%d, %d)",
				ErrBadPack, i, index[i].offset, index[i].length, dataStart, size)
		}
	}
	return &Pack{f: f, meta: meta, index: index}, nil
}

// parseHeader 解析并校验包头的定长字段（不含 metaSHA256，它要连同索引表、区域表一起算），返回块数与区域水位个数。
// 区域本身在校验和通过之后由 openPack 读出、整体校验。
func parseHeader(head []byte) (Meta, int, int, error) {
	le := binary.LittleEndian
	if string(head[:8]) != packMagic {
		return Meta{}, 0, 0, fmt.Errorf("%w: magic %q", ErrBadPack, head[:8])
	}
	if v := le.Uint32(head[8:]); v != packVersion {
		return Meta{}, 0, 0, fmt.Errorf("%w: format version %d, this build reads %d", ErrPackVersion, v, packVersion)
	}
	if hs := le.Uint32(head[12:]); hs != packHeaderSize {
		return Meta{}, 0, 0, fmt.Errorf("%w: header size %d", ErrBadPack, hs)
	}
	samples, spacing, step, codec := le.Uint32(head[32:]), le.Uint32(head[36:]), le.Uint32(head[40:]), le.Uint32(head[52:])
	if samples != ChunkSamples || spacing != SampleSpacing || step != HeightStep || codec != codecPlaneDeltaFlate {
		return Meta{}, 0, 0, fmt.Errorf("%w: chunk samples %d, spacing %d, height step %d, codec %d; this build uses %d, %d, %d, %d",
			ErrPackVersion, samples, spacing, step, codec, ChunkSamples, SampleSpacing, HeightStep, codecPlaneDeltaFlate)
	}
	if off, entry := le.Uint64(head[56:]), le.Uint32(head[64:]); off != packHeaderSize || entry != packIndexEntry {
		return Meta{}, 0, 0, fmt.Errorf("%w: index at %d with %d-byte entries", ErrBadPack, off, entry)
	}
	cx, cy := le.Uint32(head[24:]), le.Uint32(head[28:])
	if cx > math.MaxInt32 || cy > math.MaxInt32 {
		return Meta{}, 0, 0, fmt.Errorf("%w: %d × %d chunks", ErrBadPack, cx, cy)
	}
	regions := le.Uint32(head[68:])
	if regions > MaxWaterRegions {
		return Meta{}, 0, 0, fmt.Errorf("%w: %d water regions, at most %d", ErrBadPack, regions, MaxWaterRegions)
	}
	meta := Meta{
		BaseRevision: int64(le.Uint64(head[16:])),
		ChunksX:      int32(cx), ChunksY: int32(cy),
		HeightBase: int32(le.Uint32(head[44:])),
		WaterLevel: int32(le.Uint32(head[48:])),
	}
	if err := meta.Validate(); err != nil {
		return Meta{}, 0, 0, fmt.Errorf("%w: %v", ErrBadPack, err)
	}
	return meta, meta.Chunks(), int(regions), nil
}

// decodeWaterRegions 读区域表（n 项，每项 packRegionEntry 字节）。块坐标按 uint32 读，越出 int32 的值变成负数，由
// Meta.Validate 拒绝。
func decodeWaterRegions(table []byte, n int) []WaterRegion {
	if n == 0 {
		return nil
	}
	le := binary.LittleEndian
	regions := make([]WaterRegion, n)
	for i := range regions {
		e := table[i*packRegionEntry:]
		regions[i] = WaterRegion{
			MinX: int32(le.Uint32(e)), MinY: int32(le.Uint32(e[4:])), MaxX: int32(le.Uint32(e[8:])), MaxY: int32(le.Uint32(e[12:])),
			Level: int32(le.Uint32(e[16:])),
		}
	}
	return regions
}

// metaSum 是包头前 96 字节与索引表、区域表的 SHA-256（table 是两张表连在一起）。
func metaSum(head, table []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(head[:packMetaHashOff])
	h.Write(table)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// Meta 返回包头记录的地图参数（WaterRegions 是副本，调用方改它不影响包）。
func (p *Pack) Meta() Meta {
	m := p.meta
	m.WaterRegions = slices.Clone(m.WaterRegions)
	return m
}

// ChunkSum 返回块存储字节的 SHA-256（索引表里的值，不读块数据），客户端可以用它核对自己的资源。
func (p *Pack) ChunkSum(key ChunkKey) ([sha256.Size]byte, error) {
	if !p.meta.Contains(key) {
		return [sha256.Size]byte{}, fmt.Errorf("%w: chunk %v", ErrOutOfMap, key)
	}
	return p.index[p.meta.index(key)].sum, nil
}

// ReadChunk 读出一块：ReadAt 取存储字节、校验 SHA-256、解码。返回新分配的 Chunk，归调用方。
//
// 失败语义：块不在地图内返回 ErrOutOfMap；I/O 错误原样包装返回（含 Close 之后的 os.ErrClosed）；
// 校验和不符或解码失败返回 ErrCorruptChunk（带块坐标）。没有任何情况会返回默认数据。
// 一次调用约一次 ReadAt（约 3KB）加一次解压，属于文件 I/O：不要在 Nest short 池里调用。
func (p *Pack) ReadChunk(key ChunkKey) (Chunk, error) {
	if !p.meta.Contains(key) {
		return Chunk{}, fmt.Errorf("%w: chunk %v", ErrOutOfMap, key)
	}
	e := p.index[p.meta.index(key)]
	stored := make([]byte, e.length)
	if _, err := p.f.ReadAt(stored, int64(e.offset)); err != nil {
		if errors.Is(err, io.EOF) {
			return Chunk{}, fmt.Errorf("%w: chunk %v: short read", ErrCorruptChunk, key)
		}
		return Chunk{}, fmt.Errorf("terrain: read chunk %v: %w", key, err)
	}
	if sha256.Sum256(stored) != e.sum {
		return Chunk{}, fmt.Errorf("%w: chunk %v: SHA-256 mismatch", ErrCorruptChunk, key)
	}
	c, err := decodeChunk(stored)
	if err != nil {
		return Chunk{}, fmt.Errorf("%w: chunk %v: %v", ErrCorruptChunk, key, err)
	}
	return c, nil
}

// Close 关闭文件。重复 Close 返回 nil。
func (p *Pack) Close() error {
	if err := p.f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
