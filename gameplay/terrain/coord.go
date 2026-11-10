package terrain

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 块与采样的几何常量。它们决定 DAO 的形状（Heights / Materials 各 ChunkPoints 个元素）和客户端算法，
// 所以写成常量而不是配置：服务端、客户端与原始地形包必须完全一致，配置化只会多出一组必须互相校验的参数。
// 包头仍记录这些值，读包时逐项核对（不一致即拒绝），将来真要换值时随格式版本一起改。
const (
	// ChunkSamples 是每块每边的采样点数。
	ChunkSamples = 64
	// ChunkPoints 是每块的采样点数，也是 Chunk.Heights / Chunk.Materials 的长度。
	ChunkPoints = ChunkSamples * ChunkSamples
	// SampleSpacing 是相邻采样点的间距，世界单位（1 单位 = 1 厘米，即 1m）。
	SampleSpacing = 100
	// ChunkSize 是块边长，世界单位（64m）。
	ChunkSize = ChunkSamples * SampleSpacing
	// HeightStep 是高度原始值 1 对应的世界高度，世界单位（10cm）。
	HeightStep = 10
)

var (
	// ErrOutOfMap 表示坐标或块不在地图范围内。
	ErrOutOfMap = errors.New("terrain: outside the map")
	// ErrInvalidChunk 表示块数据形状不对（长度不是 ChunkPoints）。
	ErrInvalidChunk = errors.New("terrain: invalid chunk data")
	// ErrInvalidMeta 表示地图参数不合法。
	ErrInvalidMeta = errors.New("terrain: invalid map meta")
)

// ChunkKey 是块坐标：块 (X, Y) 覆盖世界坐标 [X·ChunkSize, (X+1)·ChunkSize) × [Y·ChunkSize, (Y+1)·ChunkSize)。
// 地图从世界原点 (0, 0) 开始，地图内的块坐标都非负；负坐标是合法的键，只是不在任何地图里。
type ChunkKey struct {
	X, Y int32
}

func (k ChunkKey) String() string { return fmt.Sprintf("(%d,%d)", k.X, k.Y) }

// ID 把块坐标编码成 uint64：高 32 位是 X、低 32 位是 Y（各按 uint32 位模式）。编码是双射，KeyFromID 还原；
// 地图内的块（X、Y 非负）ID 小于 2^63，可以无损放进 int64（上层拿它拼实体 ID 时不会变负）。
func (k ChunkKey) ID() uint64 { return uint64(uint32(k.X))<<32 | uint64(uint32(k.Y)) }

// KeyFromID 是 ChunkKey.ID 的逆。
func KeyFromID(id uint64) ChunkKey { return ChunkKey{X: int32(uint32(id >> 32)), Y: int32(uint32(id))} }

// Origin 返回块的最小角（世界坐标）。
func (k ChunkKey) Origin() spatial.Point {
	return spatial.Point{X: int64(k.X) * ChunkSize, Y: int64(k.Y) * ChunkSize}
}

// Bounds 返回块覆盖的半开矩形。
func (k ChunkKey) Bounds() spatial.Rect {
	o := k.Origin()
	return spatial.Rect{Min: o, Max: spatial.Point{X: o.X + ChunkSize, Y: o.Y + ChunkSize}}
}

// ChunkOf 返回包含世界坐标 (x, y) 的块（向负无穷取整，负坐标落在负块里）。
func ChunkOf(x, y int64) ChunkKey {
	return ChunkKey{X: clampInt32(floorDiv(x, ChunkSize)), Y: clampInt32(floorDiv(y, ChunkSize))}
}

// PointIndex 返回世界坐标 (x, y) 所在 1m 格左下角的采样点：所属块与它在 Heights / Materials 里的下标
// （行优先：下标 = j·ChunkSamples + i，i 沿 X、j 沿 Y）。左下角采样点总在同一块里，不需要邻块。
func PointIndex(x, y int64) (ChunkKey, int) {
	gx, gy := floorDiv(x, SampleSpacing), floorDiv(y, SampleSpacing)
	key := ChunkKey{X: clampInt32(floorDiv(gx, ChunkSamples)), Y: clampInt32(floorDiv(gy, ChunkSamples))}
	return key, int(floorMod(gy, ChunkSamples))*ChunkSamples + int(floorMod(gx, ChunkSamples))
}

// ChunksIn 依次给出与半开矩形 r 相交的全部块（行优先，先 Y 后 X），不论是否在地图内；
// 上层用它列出一个格（或格加 Ghost 带）需要装入的块，再用 Meta.Contains 过滤地图外的。r 由调用方保证有界。
func ChunksIn(r spatial.Rect) iter.Seq[ChunkKey] {
	r = spatial.NormalizeRect(r)
	return func(yield func(ChunkKey) bool) {
		if r.Empty() {
			return
		}
		lo, hi := ChunkOf(r.Min.X, r.Min.Y), ChunkOf(r.Max.X-1, r.Max.Y-1)
		for y := int64(lo.Y); y <= int64(hi.Y); y++ { // int64 计数：hi 饱和到 MaxInt32 时也不回绕
			for x := int64(lo.X); x <= int64(hi.X); x++ {
				if !yield(ChunkKey{X: int32(x), Y: int32(y)}) {
					return
				}
			}
		}
	}
}

// Chunk 是一块的原始数据，形状与 TerrainChunkDao 相同：Heights 与 Materials 各 ChunkPoints 个元素，
// 行优先（下标 = j·ChunkSamples + i）。Heights 是高度原始值（世界高度见 Meta.WorldHeight），Materials 是材质编号。
//
// 所有权：返回 Chunk 的函数（Pack.ReadChunk、View.Chunk、ChunkSource）都给出新分配的切片，归调用方；
// 接收 Chunk 的函数（View.SetOverlay）复制入参，调用之后调用方可以继续改自己的切片。
type Chunk struct {
	Heights   []uint16
	Materials []byte
}

// NewChunk 返回全零的块（高度原始值 0、材质 0）。
func NewChunk() Chunk {
	return Chunk{Heights: make([]uint16, ChunkPoints), Materials: make([]byte, ChunkPoints)}
}

// Validate 检查形状。
func (c Chunk) Validate() error {
	if len(c.Heights) != ChunkPoints || len(c.Materials) != ChunkPoints {
		return fmt.Errorf("%w: %d heights and %d materials, want %d each", ErrInvalidChunk, len(c.Heights), len(c.Materials), ChunkPoints)
	}
	return nil
}

// Clone 返回深拷贝。
func (c Chunk) Clone() Chunk {
	return Chunk{Heights: append([]uint16(nil), c.Heights...), Materials: append([]byte(nil), c.Materials...)}
}

// Meta 是一张地图的参数，记在原始地形包的包头里。
type Meta struct {
	// BaseRevision 是原始地形的版本，美术每次重新出图加一；覆盖块记录自己基于哪一版（TerrainChunkDao.BaseRevision）。
	BaseRevision int64
	// ChunksX、ChunksY 是地图每边的块数：地图覆盖世界坐标 [0, ChunksX·ChunkSize) × [0, ChunksY·ChunkSize)。
	ChunksX, ChunksY int32
	// HeightBase 是高度原始值 0 对应的世界高度（厘米）。
	HeightBase int32
	// WaterLevel 是全局水位线（世界高度，厘米）：不在任何 WaterRegions 里的块，地面高度低于它的位置是水（方案 §5）。
	// 不要水时设 math.MinInt32。
	WaterLevel int32
	// WaterRegions 是按区域配置的水位线（方案 D3“全局 / 区域水位线”，P4）：区域里的块用区域的 Level 代替 WaterLevel。
	// 区域按块对齐、互不重叠（Validate 检查），所以每一块恰好有一条水位线（WaterLevelOf）。至多 MaxWaterRegions 个。
	// 记在原始地形包里（格式版本 2），服务端与客户端读同一份，不另外同步。
	WaterRegions []WaterRegion
}

// WaterRegion 是一片按块对齐的矩形区域的水位线：块坐标半开区间 [MinX, MaxX) × [MinY, MaxY) 里的每一块，地面高度低于
// Level（世界高度，厘米）的位置是水；Level 为 math.MinInt32 表示这片区域没有水。
//
// 为什么按块对齐：一块只有一条水位线，水面在块内是一个平面——客户端按块生成水面与岸线（Marching Squares 用这块的
// 水位，T03）、水位变化按块失效，服务端判定按节点所属的块取水位，不需要跨块计算。区域边界处两侧水位不同时水面在块边
// 上有台阶，由美术把区域边界放在高于两侧水位的岸上（地形包工具不替美术判断）。
type WaterRegion struct {
	MinX, MinY, MaxX, MaxY int32
	Level                  int32
}

// MaxWaterRegions 是一张图最多的区域水位数（包的区域表有界；打开时两两检查不重叠）。
const MaxWaterRegions = 1024

// Contains 报告块是否在区域里。
func (r WaterRegion) Contains(k ChunkKey) bool {
	return k.X >= r.MinX && k.Y >= r.MinY && k.X < r.MaxX && k.Y < r.MaxY
}

func (r WaterRegion) overlaps(o WaterRegion) bool {
	return r.MinX < o.MaxX && o.MinX < r.MaxX && r.MinY < o.MaxY && o.MinY < r.MaxY
}

// 地图规模上限：每边 MaxChunksPerSide 块（约 65km），总块数 MaxChunks；超过的包当作损坏拒绝（索引表大小有界）。
const (
	MaxChunksPerSide = 1024
	MaxChunks        = MaxChunksPerSide * MaxChunksPerSide
)

// Validate 检查参数范围：块数在 [1, MaxChunksPerSide]，最高的地面（原始值 65535）不溢出 int32。
func (m Meta) Validate() error {
	if m.ChunksX < 1 || m.ChunksY < 1 || m.ChunksX > MaxChunksPerSide || m.ChunksY > MaxChunksPerSide {
		return fmt.Errorf("%w: %d × %d chunks, each side must be in [1, %d]", ErrInvalidMeta, m.ChunksX, m.ChunksY, MaxChunksPerSide)
	}
	if int64(m.HeightBase)+math.MaxUint16*HeightStep > math.MaxInt32 {
		return fmt.Errorf("%w: height base %d overflows int32 at the highest raw height", ErrInvalidMeta, m.HeightBase)
	}
	if len(m.WaterRegions) > MaxWaterRegions {
		return fmt.Errorf("%w: %d water regions, at most %d", ErrInvalidMeta, len(m.WaterRegions), MaxWaterRegions)
	}
	for i, r := range m.WaterRegions {
		if r.MinX < 0 || r.MinY < 0 || r.MinX >= r.MaxX || r.MinY >= r.MaxY || r.MaxX > m.ChunksX || r.MaxY > m.ChunksY {
			return fmt.Errorf("%w: water region %d %+v is empty or outside the %d × %d chunks", ErrInvalidMeta, i, r, m.ChunksX, m.ChunksY)
		}
		for j := range i {
			if r.overlaps(m.WaterRegions[j]) {
				return fmt.Errorf("%w: water regions %d %+v and %d %+v overlap", ErrInvalidMeta, j, m.WaterRegions[j], i, r)
			}
		}
	}
	return nil
}

// WaterLevelOf 返回块的水位线：所在区域的 Level，不在任何区域里时是全局 WaterLevel。区域互不重叠，结果唯一。
// 逐个比较区域（至多 MaxWaterRegions 个）；查询热路径用 View 里按块预先算好的表（View.WaterLevelOf）。
func (m Meta) WaterLevelOf(k ChunkKey) int32 {
	for _, r := range m.WaterRegions {
		if r.Contains(k) {
			return r.Level
		}
	}
	return m.WaterLevel
}

// Equal 报告两份地图参数是否完全相同（含区域水位，按顺序比较）。
func (m Meta) Equal(o Meta) bool {
	return m.BaseRevision == o.BaseRevision && m.ChunksX == o.ChunksX && m.ChunksY == o.ChunksY &&
		m.HeightBase == o.HeightBase && m.WaterLevel == o.WaterLevel && slices.Equal(m.WaterRegions, o.WaterRegions)
}

// Chunks 返回地图的总块数。
func (m Meta) Chunks() int { return int(m.ChunksX) * int(m.ChunksY) }

// Bounds 返回地图覆盖的半开矩形（世界坐标）。
func (m Meta) Bounds() spatial.Rect {
	return spatial.Rect{Max: spatial.Point{X: int64(m.ChunksX) * ChunkSize, Y: int64(m.ChunksY) * ChunkSize}}
}

// Contains 报告块是否在地图内。
func (m Meta) Contains(k ChunkKey) bool {
	return k.X >= 0 && k.Y >= 0 && k.X < m.ChunksX && k.Y < m.ChunksY
}

// WorldHeight 把高度原始值换成世界高度（厘米）。
func (m Meta) WorldHeight(raw uint16) int32 { return m.HeightBase + int32(raw)*HeightStep }

// index 返回块在包索引表里的序号（行优先），调用方先确认 Contains。
func (m Meta) index(k ChunkKey) int { return int(k.Y)*int(m.ChunksX) + int(k.X) }

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

// clampInt32 把块坐标饱和到 int32：远在地图外的世界坐标不会回绕成地图内的块。
func clampInt32(v int64) int32 { return int32(min(max(v, math.MinInt32), math.MaxInt32)) }
