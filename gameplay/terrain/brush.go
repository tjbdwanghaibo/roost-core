package terrain

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 笔刷（可编辑地形方案 §4 的编辑操作）：抬高 / 降低 / 整平 / 改材质。全部是整数运算、结果只取决于入参，
// 客户端按同一算法可逐位复现，用作本地预测（金样 client/spec/terrain-brush.json）。
//
// 本节只有纯函数：给出“哪些块被覆盖”（Brush.Chunks）与“每块哪些采样点改成什么”（Brush.Apply），不碰
// View、DAO 或锁。玩家能用多大的笔刷、频率、权限等是上层（游戏）的规则，见 Brush.Validate 的说明。

// BrushOp 是笔刷操作。零值无效。
type BrushOp uint8

const (
	// BrushRaise 抬高：中心处加 Strength，向外按衰减曲线减小。
	BrushRaise BrushOp = 1
	// BrushLower 降低：中心处减 Strength，向外按衰减曲线减小。
	BrushLower BrushOp = 2
	// BrushFlatten 整平：每点朝 Target 移动，移动比例按衰减曲线（常数衰减即整块压到 Target）。
	BrushFlatten BrushOp = 3
	// BrushPaint 改材质：半径内的点（含边上）材质改为 Material，不看衰减曲线。
	BrushPaint BrushOp = 4
)

func (op BrushOp) String() string {
	switch op {
	case BrushRaise:
		return "raise"
	case BrushLower:
		return "lower"
	case BrushFlatten:
		return "flatten"
	case BrushPaint:
		return "paint"
	}
	return fmt.Sprintf("BrushOp(%d)", uint8(op))
}

// Falloff 是衰减曲线：采样点到中心的距离 d、半径 R 时的权重 w ∈ [0, WeightScale]。零值是常数衰减。
type Falloff uint8

const (
	// FalloffConstant：d ≤ R 时 w = WeightScale，否则 0。
	FalloffConstant Falloff = 0
	// FalloffLinear：d = ISqrt(d²)（向下取整），d < R 时 w = ⌊WeightScale·(R − d) / R⌋，否则 0（圆锥）。
	FalloffLinear Falloff = 1
	// FalloffSmooth：d² < R² 时 t = ⌊WeightScale·(R² − d²) / R²⌋、w = ⌊t·t / WeightScale⌋，否则 0
	// （(1 − d²/R²)² 的钟形，边上切线为 0，不用开方）。
	FalloffSmooth Falloff = 2
)

func (f Falloff) String() string {
	switch f {
	case FalloffConstant:
		return "constant"
	case FalloffLinear:
		return "linear"
	case FalloffSmooth:
		return "smooth"
	}
	return fmt.Sprintf("Falloff(%d)", uint8(f))
}

// WeightScale 是权重的定点刻度：w = WeightScale 表示 100%。
const WeightScale = 1 << 16

// MaxBrushRadius 是本包接受的最大半径（世界单位，约 164m），保证中间值不溢出 int64、覆盖的块数有界。
// 玩家能用的半径由上层另行限制（方案 §4：16m），远小于它。
const MaxBrushRadius = 1 << 14

var (
	// ErrInvalidBrush 表示笔刷参数不合法（操作、半径、衰减、强度、目标高度、上下限）。
	ErrInvalidBrush = errors.New("terrain: invalid brush")
)

// Brush 是一次笔刷编辑的参数。坐标是世界坐标（厘米），高度是原始值（1 = 10cm）。
type Brush struct {
	Op BrushOp
	// X、Y 是中心（世界坐标），必须在地图内。
	X, Y int64
	// Radius 是半径（世界单位），1 ≤ Radius ≤ MaxBrushRadius。覆盖的采样点是与中心距离 d² ≤ Radius² 的世界采样点。
	Radius int64
	// Falloff 是衰减曲线（BrushPaint 不看它）。
	Falloff Falloff
	// Strength 是 BrushRaise / BrushLower 在中心处（w = WeightScale）的高度原始值增量，> 0。
	Strength uint16
	// Target 是 BrushFlatten 的目标高度原始值，必须在 [Limits.Bedrock, Limits.Ceiling] 内。
	Target uint16
	// Material 是 BrushPaint 写入的材质编号。
	Material byte
}

// Limits 是编辑后的高度原始值范围（方案 §4“不能低于基岩、不能高于上限”）。
//
// 钳制规则：抬高不会超过 Ceiling、降低不会低于 Bedrock；但编辑从不把一个点往范围外推，也不把原本就在范围外的点
// 拉回来（抬高只升不降、降低只降不升、整平只朝 Target 走）：原始地形里高于上限 / 低于基岩的点保持原样，直到有编辑
// 把它往范围内移动。
type Limits struct {
	Bedrock uint16
	Ceiling uint16
}

// FullRange 是不限制的范围 [0, 65535]。
var FullRange = Limits{Bedrock: 0, Ceiling: 65535}

// PointChange 是一个采样点编辑后的值：Index 是块内下标（行优先，j·64 + i），Height / Material 是新值（不论两者中
// 哪一个变了，都给出两者的新值）。
type PointChange struct {
	Index    int32
	Height   uint16
	Material byte
}

// ChunkChange 是一块里被改动的采样点，Points 按 Index 升序，只含值真的变了的点。
type ChunkChange struct {
	Key    ChunkKey
	Points []PointChange
}

// HeightChanges 返回本块里高度变了的点数（整字段守卫按它判断，方案 §4）。
func (c ChunkChange) HeightChanges(before Chunk) int {
	n := 0
	for _, p := range c.Points {
		if before.Heights[p.Index] != p.Height {
			n++
		}
	}
	return n
}

// Validate 检查笔刷本身的参数（与地图、上下限的关系）。游戏规则（玩家半径上限、单次块数、最大增量、频率、权限、
// 建筑占位）不在这里，由上层在调用前另行检查。
func (b Brush) Validate(meta Meta, limits Limits) error {
	if limits.Bedrock > limits.Ceiling {
		return fmt.Errorf("%w: bedrock %d above ceiling %d", ErrInvalidBrush, limits.Bedrock, limits.Ceiling)
	}
	switch b.Op {
	case BrushRaise, BrushLower:
		if b.Strength == 0 {
			return fmt.Errorf("%w: %v needs a positive strength", ErrInvalidBrush, b.Op)
		}
	case BrushFlatten:
		if b.Target < limits.Bedrock || b.Target > limits.Ceiling {
			return fmt.Errorf("%w: flatten target %d outside [%d, %d]", ErrInvalidBrush, b.Target, limits.Bedrock, limits.Ceiling)
		}
	case BrushPaint:
	default:
		return fmt.Errorf("%w: unknown op %d", ErrInvalidBrush, uint8(b.Op))
	}
	if b.Falloff > FalloffSmooth {
		return fmt.Errorf("%w: unknown falloff %d", ErrInvalidBrush, uint8(b.Falloff))
	}
	if b.Radius < 1 || b.Radius > MaxBrushRadius {
		return fmt.Errorf("%w: radius %d outside [1, %d]", ErrInvalidBrush, b.Radius, MaxBrushRadius)
	}
	if !meta.Bounds().Contains(spatial.Point{X: b.X, Y: b.Y}) {
		return fmt.Errorf("%w: brush centre (%d, %d)", ErrOutOfMap, b.X, b.Y)
	}
	return nil
}

// sampleRange 是笔刷覆盖的世界采样网格范围（闭区间），已裁到地图内。ok 为 false 表示没有采样点被覆盖。
func (b Brush) sampleRange(meta Meta) (gx0, gx1, gy0, gy1 int64, ok bool) {
	gx0 = max(ceilDiv(b.X-b.Radius, SampleSpacing), 0)
	gy0 = max(ceilDiv(b.Y-b.Radius, SampleSpacing), 0)
	gx1 = min(floorDiv(b.X+b.Radius, SampleSpacing), int64(meta.ChunksX)*ChunkSamples-1)
	gy1 = min(floorDiv(b.Y+b.Radius, SampleSpacing), int64(meta.ChunksY)*ChunkSamples-1)
	return gx0, gx1, gy0, gy1, gx0 <= gx1 && gy0 <= gy1
}

// covers 遍历覆盖的每个采样点（d² ≤ R²）及其距离平方。
func (b Brush) covers(meta Meta, visit func(gx, gy, d2 int64)) {
	gx0, gx1, gy0, gy1, ok := b.sampleRange(meta)
	if !ok {
		return
	}
	r2 := b.Radius * b.Radius
	for gy := gy0; gy <= gy1; gy++ {
		dy := gy*SampleSpacing - b.Y
		for gx := gx0; gx <= gx1; gx++ {
			dx := gx*SampleSpacing - b.X
			if d2 := dx*dx + dy*dy; d2 <= r2 {
				visit(gx, gy, d2)
			}
		}
	}
}

// Chunks 返回覆盖到的块（至少有一个采样点 d² ≤ R²，且在地图内），按 (Y, X) 升序。每个采样点只属于一个块
// （共享边归右 / 上块），所以一次编辑要锁的块就是这些。参数先经 Validate。
func (b Brush) Chunks(meta Meta) []ChunkKey {
	seen := map[ChunkKey]struct{}{}
	b.covers(meta, func(gx, gy, _ int64) {
		seen[ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}] = struct{}{}
	})
	return slices.SortedFunc(maps.Keys(seen), compareKeys)
}

// compareKeys 按 (Y, X) 排序块坐标。
func compareKeys(a, b ChunkKey) int { return cmp.Or(cmp.Compare(a.Y, b.Y), cmp.Compare(a.X, b.X)) }

// Weight 返回距离平方 d2、半径 radius 时 falloff 的权重（见 Falloff 的公式）；radius ≥ 1。
func (f Falloff) Weight(d2, radius int64) int64 {
	r2 := radius * radius
	switch f {
	case FalloffLinear:
		d := ISqrt(d2)
		if d >= radius {
			return 0
		}
		return WeightScale * (radius - d) / radius
	case FalloffSmooth:
		if d2 >= r2 {
			return 0
		}
		t := WeightScale * (r2 - d2) / r2
		return t * t / WeightScale
	default:
		if d2 > r2 {
			return 0
		}
		return WeightScale
	}
}

// ISqrt 返回 ⌊√n⌋（n ≥ 0；负数返回 0）。只用整数运算，结果唯一，客户端任何正确的整数开方都与它相同。
func ISqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	// 牛顿迭代从不小于真值的初值开始，单调下降到 ⌊√n⌋。
	x := n
	y := x/2 + x%2 // (x + 1) / 2，不溢出
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

// Apply 在 chunks（覆盖到的每一块编辑前的数据，键是块坐标）上计算这次编辑，返回每块真的变了的采样点，块按 (Y, X)
// 升序，块内按下标升序；没有变化的块不出现。chunks 只读，调用方的切片不会被改。
//
// 每个覆盖到的采样点（d² ≤ R²），w = Falloff.Weight(d², R)，cur 是编辑前高度：
//
//	BrushRaise   δ = ⌊(Strength·w + WeightScale/2) / WeightScale⌋，新高度 = max(cur, min(cur + δ, Ceiling))
//	BrushLower   δ 同上，新高度 = min(cur, max(cur − δ, Bedrock))
//	BrushFlatten diff = Target − cur，step = sign(diff)·⌊(|diff|·w + WeightScale/2) / WeightScale⌋，新高度 = cur + step
//	BrushPaint   材质 = Material（不看 w），高度不变
//
// 参数先经 Validate；覆盖到的块缺数据或形状不对时返回错误（ErrNotLoaded / ErrInvalidChunk），不给部分结果。
func (b Brush) Apply(meta Meta, limits Limits, chunks map[ChunkKey]Chunk) ([]ChunkChange, error) {
	if err := b.Validate(meta, limits); err != nil {
		return nil, err
	}
	for _, key := range b.Chunks(meta) {
		c, ok := chunks[key]
		if !ok {
			return nil, fmt.Errorf("%w: brush needs chunk %v", ErrNotLoaded, key)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("chunk %v: %w", key, err)
		}
	}
	byChunk := map[ChunkKey][]PointChange{}
	b.covers(meta, func(gx, gy, d2 int64) {
		key := ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}
		index := int32((gy%ChunkSamples)*ChunkSamples + gx%ChunkSamples)
		c := chunks[key]
		height, material := c.Heights[index], c.Materials[index]
		nextHeight, nextMaterial := b.point(limits, height, material, d2)
		if nextHeight != height || nextMaterial != material {
			byChunk[key] = append(byChunk[key], PointChange{Index: index, Height: nextHeight, Material: nextMaterial})
		}
	})
	out := make([]ChunkChange, 0, len(byChunk))
	for _, key := range slices.SortedFunc(maps.Keys(byChunk), compareKeys) {
		points := byChunk[key]
		slices.SortFunc(points, func(a, b PointChange) int { return cmp.Compare(a.Index, b.Index) })
		out = append(out, ChunkChange{Key: key, Points: points})
	}
	return out, nil
}

// Footprint 返回每块被覆盖的采样点数（d² ≤ R²，每个采样点只算在它所属的块里：共享边归右 / 上块），只看几何、不读
// 块数据。一次笔刷只改一个字段（抬高 / 降低 / 整平改高度，改材质改材质），所以一块的 Footprint 是 View.BrushChanges
// 在这块上（高度点数 + 材质点数）的上限：没有装入这些块的进程（分区场景转发编辑的 Base，P5）按它预扣配额，执行的格
// 答回实际改动的点数后多退。参数先经 Validate。
func (b Brush) Footprint(meta Meta) map[ChunkKey]int {
	out := map[ChunkKey]int{}
	b.covers(meta, func(gx, gy, _ int64) {
		out[ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}]++
	})
	return out
}

// point 是一个采样点的编辑结果（公式见 Apply）。
func (b Brush) point(limits Limits, height uint16, material byte, d2 int64) (uint16, byte) {
	if b.Op == BrushPaint {
		return height, b.Material
	}
	w := b.Falloff.Weight(d2, b.Radius)
	cur := int64(height)
	switch b.Op {
	case BrushRaise:
		delta := (int64(b.Strength)*w + WeightScale/2) / WeightScale
		return uint16(max(cur, min(cur+delta, int64(limits.Ceiling)))), material
	case BrushLower:
		delta := (int64(b.Strength)*w + WeightScale/2) / WeightScale
		return uint16(min(cur, max(cur-delta, int64(limits.Bedrock)))), material
	case BrushFlatten:
		diff := int64(b.Target) - cur
		if diff >= 0 {
			return uint16(cur + (diff*w+WeightScale/2)/WeightScale), material
		}
		return uint16(cur - (-diff*w+WeightScale/2)/WeightScale), material
	}
	return height, material
}

func ceilDiv(a, b int64) int64 { return -floorDiv(-a, b) }
