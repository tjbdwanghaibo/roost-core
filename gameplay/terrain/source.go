package terrain

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// ChunkSource 给出地图里一块的原始数据，WritePack 用它逐块取数据。返回的 Chunk 归调用方（WritePack 读完即弃）。
// 实现必须可以被并发调用（WritePack 用多个 goroutine 并行取块和压缩）；返回错误时整个写包失败。
type ChunkSource func(key ChunkKey) (Chunk, error)

// RAWImport 描述美术导出的高度图（RAW16）与材质图（RAW8）：无文件头，行优先，每行 Width 个采样、共 Height 行，
// 一个像素对应一个 1m 采样点，地图左下角（世界坐标最小处）对应采样 (0, 0)。
//
// 尺寸不必正好是块的整数倍：Width、Height 不超过地图的采样数（ChunksX·64、ChunksY·64）即可，图外的采样点取最近的
// 图边像素（边缘复制）。这样 10000 × 10000 的图可以直接装进 157 × 157 块（10048 采样）的地图。
type RAWImport struct {
	// Heights 是 RAW16 高度图，每像素 2 字节。必填。
	Heights io.ReaderAt
	// Materials 是 RAW8 材质图，每像素 1 字节；nil 表示全部材质 0。
	Materials io.ReaderAt
	// Width、Height 是图的像素数（采样数）。
	Width, Height int
	// BigEndian：高度图是大端（部分工具在 macOS 上的缺省）；否则小端。
	BigEndian bool
	// FlipY：图的第一行是世界 Y 最大处（图像“上北”的惯例）。不设时第一行是世界 Y = 0。
	FlipY bool
	// MaxHeight 是像素值 65535 对应的高度（厘米，相对 Meta.HeightBase），像素值按比例换算并四舍五入成原始值；
	// 0 表示像素值本身就是原始值（10cm 单位）。必须在 [0, 65535·HeightStep]。
	MaxHeight int32
}

// Source 校验图的尺寸与参数，返回从这两张图逐块取数据的 ChunkSource。读失败（文件短、I/O 错误）在取块时返回。
func (r RAWImport) Source(meta Meta) (ChunkSource, error) {
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	if r.Heights == nil {
		return nil, fmt.Errorf("terrain: RAW import without a height map")
	}
	maxW, maxH := int(meta.ChunksX)*ChunkSamples, int(meta.ChunksY)*ChunkSamples
	if r.Width < 1 || r.Height < 1 || r.Width > maxW || r.Height > maxH {
		return nil, fmt.Errorf("terrain: RAW image %d × %d must be within [1, %d] × [1, %d] samples", r.Width, r.Height, maxW, maxH)
	}
	if r.MaxHeight < 0 || int64(r.MaxHeight) > math.MaxUint16*HeightStep {
		return nil, fmt.Errorf("terrain: RAW max height %d must be in [0, %d]", r.MaxHeight, math.MaxUint16*HeightStep)
	}
	return r.readChunk, nil
}

// readChunk 读一块：逐行读出这块覆盖的一段像素（越过图边的部分复制图边像素）。
func (r RAWImport) readChunk(key ChunkKey) (Chunk, error) {
	c := NewChunk()
	x0 := int(key.X) * ChunkSamples
	// 这一块在图里实际覆盖的列 [x0, x1]；整块都在图外时 x0 == x1 == 图的最后一列。
	first, last := min(x0, r.Width-1), min(x0+ChunkSamples-1, r.Width-1)
	n := last - first + 1
	row16 := make([]byte, 2*n)
	row8 := make([]byte, n)
	order := binary.ByteOrder(binary.LittleEndian)
	if r.BigEndian {
		order = binary.BigEndian
	}
	for j := range ChunkSamples {
		gy := min(int(key.Y)*ChunkSamples+j, r.Height-1)
		if r.FlipY {
			gy = r.Height - 1 - gy
		}
		pixel := int64(gy)*int64(r.Width) + int64(first)
		if _, err := r.Heights.ReadAt(row16, 2*pixel); err != nil {
			return Chunk{}, fmt.Errorf("terrain: read RAW heights at row %d: %w", gy, err)
		}
		if r.Materials != nil {
			if _, err := r.Materials.ReadAt(row8, pixel); err != nil {
				return Chunk{}, fmt.Errorf("terrain: read RAW materials at row %d: %w", gy, err)
			}
		}
		for i := range ChunkSamples {
			col := min(x0+i, last) - first
			c.Heights[j*ChunkSamples+i] = r.rawHeight(order.Uint16(row16[2*col:]))
			c.Materials[j*ChunkSamples+i] = row8[col]
		}
	}
	return c, nil
}

func (r RAWImport) rawHeight(pixel uint16) uint16 {
	if r.MaxHeight == 0 {
		return pixel
	}
	const denom = math.MaxUint16 * HeightStep
	return uint16((uint64(pixel)*uint64(r.MaxHeight) + denom/2) / denom)
}

// 生成器用到的材质编号（只是生成器的约定，不是框架对材质的定义；材质表归业务）。
const (
	GenGrass   byte = 0
	GenSand    byte = 1
	GenRock    byte = 2
	GenDirt    byte = 3
	GenLakebed byte = 4
)

// Generated 返回一个确定性的程序化地形：同一 (meta, seed) 在任何机器上逐位相同，只用整数运算。
// 用于测试、压测与客户端金样，不追求美观：几层值噪声叠出 200m～1100m 的起伏，低频掩码上隆起最高约 2.3km 的山
// （原始值会超过 16384，覆盖同步编码的 3 字节档），材质按水位线、高度和一层噪声分成湖床 / 沙 / 岩石 / 泥土 / 草。
func Generated(meta Meta, seed uint64) ChunkSource {
	return func(key ChunkKey) (Chunk, error) {
		if !meta.Contains(key) {
			return Chunk{}, fmt.Errorf("%w: chunk %v", ErrOutOfMap, key)
		}
		c := NewChunk()
		for j := range ChunkSamples {
			gy := int64(key.Y)*ChunkSamples + int64(j)
			for i := range ChunkSamples {
				gx := int64(key.X)*ChunkSamples + int64(i)
				raw := genHeight(seed, gx, gy)
				c.Heights[j*ChunkSamples+i] = raw
				c.Materials[j*ChunkSamples+i] = genMaterial(meta, seed, gx, gy, raw)
			}
		}
		return c, nil
	}
}

// genHeight：4 层值噪声（周期 512 / 128 / 32 / 8 个采样，权重 8:4:2:1）归一到 [0, 65535] 后映射为 200m～1100m，
// 周期 2048 的掩码超过阈值的地方再加山，最后加逐点抖动。
func genHeight(seed uint64, gx, gy int64) uint16 {
	n := (8*valueNoise(seed, 1, gx, gy, 9) + 4*valueNoise(seed, 2, gx, gy, 7) +
		2*valueNoise(seed, 3, gx, gy, 5) + valueNoise(seed, 4, gx, gy, 3)) / 15
	h := 2000 + n*9000/65535
	if m := valueNoise(seed, 5, gx, gy, 11); m > 40000 {
		h += (m - 40000) * 12000 / 25535
	}
	// ±2（±20cm）的逐点抖动，接近美术高度图量化到 10cm 后的局部噪声；没有它地形过于光滑，压缩率会偏乐观。
	return uint16(h + lattice(seed, 7, gx, gy)%5 - 2)
}

func genMaterial(meta Meta, seed uint64, gx, gy int64, raw uint16) byte {
	ground := meta.WorldHeight(raw)
	switch {
	case ground < meta.WaterLevel:
		return GenLakebed
	case int64(ground) < int64(meta.WaterLevel)+200:
		return GenSand
	case raw > 11000:
		return GenRock
	case valueNoise(seed, 6, gx, gy, 4) > 42000:
		return GenDirt
	default:
		return GenGrass
	}
}

// valueNoise 是周期 2^shift 个采样的二维值噪声：格点取哈希值，格内用整数 smoothstep 双线性插值，结果在 [0, 65535]。
func valueNoise(seed, layer uint64, gx, gy int64, shift uint) int64 {
	ix, iy := gx>>shift, gy>>shift
	mask := int64(1)<<shift - 1
	tx, ty := smooth16((gx&mask)<<16>>shift), smooth16((gy&mask)<<16>>shift)
	v00, v10 := lattice(seed, layer, ix, iy), lattice(seed, layer, ix+1, iy)
	v01, v11 := lattice(seed, layer, ix, iy+1), lattice(seed, layer, ix+1, iy+1)
	a := v00 + (v10-v00)*tx>>16
	b := v01 + (v11-v01)*tx>>16
	return a + (b-a)*ty>>16
}

// smooth16 是 16 位定点的 3t² − 2t³（t ∈ [0, 65536)）。
func smooth16(t int64) int64 { return t * t >> 16 * (3<<16 - 2*t) >> 16 }

// lattice 是格点 (ix, iy) 在某一层的哈希值 [0, 65535]（splitmix64 的混合步骤）。
func lattice(seed, layer uint64, ix, iy int64) int64 {
	z := seed ^ uint64(ix)*0x9E3779B97F4A7C15 ^ uint64(iy)*0xC2B2AE3D27D4EB4F ^ layer*0x165667B19E3779F9
	z ^= z >> 30
	z *= 0xBF58476D1CE4E5B9
	z ^= z >> 27
	z *= 0x94D049BB133111EB
	z ^= z >> 31
	return int64(z >> 48)
}
