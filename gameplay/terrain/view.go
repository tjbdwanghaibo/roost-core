package terrain

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var (
	// ErrNotLoaded 表示查询需要的块不在 View 里（没装入或已卸下）。查询不猜测缺失的数据：调用方先装块再查。
	ErrNotLoaded = errors.New("terrain: chunk not loaded")
	// ErrStaleOverlay 表示覆盖块的版本比 View 里已有的覆盖块旧，没有装入。
	ErrStaleOverlay = errors.New("terrain: stale overlay version")
	// ErrBaseRevision 表示覆盖块基于另一版原始地形（BaseRevision 与包头不同），没有装入。
	// 原始地形换版后旧覆盖块怎么处理（保留、重放、丢弃）是上层的决定，本包只拒绝混用。
	ErrBaseRevision = errors.New("terrain: overlay base revision differs from the pack")
)

// Overlay 是被玩家改过的块，由上层（TerrainChunk 实体的 DAO）提供，字段与 TerrainChunkDao 对应。
type Overlay struct {
	// BaseRevision 是这块基于的原始地形版本，必须等于包头的 BaseRevision。
	BaseRevision int64
	// Version 是块的提交版本（每次提交加一），必须 ≥ 1；0 留给“原始块”。
	Version int64
	// Heights、Materials 是整块数据（各 ChunkPoints 个元素），形状同 Chunk。
	Heights   []uint16
	Materials []byte
}

// Ground 是一个位置的查询结果。
type Ground struct {
	// Height 是地面的世界高度（厘米），双线性插值、整数定点，算法见 View.Height。
	Height int32
	// Material 是所在 1m 格左下角采样点的材质编号。
	Material byte
	// Water 表示地面低于所在块的水位线（Height < WaterLevel）。
	Water bool
	// WaterLevel 是所在块的水位线（Meta.WaterLevelOf：区域水位或全局水位，世界高度，厘米）。
	WaterLevel int32
}

// Depth 是水深（厘米）：水位线减地面高度；不是水时为 0，是水时为正。
//
// 返回 int64：水位线与地面高度都是 int32，但高度基准可以为负、水位线是独立配置的，Meta.Validate 不约束两者之差——
// 合法的地图参数下差值可以超出 int32（最大 2³² − 1）。与 Walker 判游泳用的是同一个 int64 差值；客户端
// TerrainGround.Depth 同为 64 位。
func (g Ground) Depth() int64 {
	if !g.Water {
		return 0
	}
	return int64(g.WaterLevel) - int64(g.Height)
}

// View 是一张地图在本进程内已装入的块：原始块（从 Pack 读出）与覆盖块（上层给的改过的块）。
// 每个块坐标至多一份数据，覆盖块优先于原始块。View 不自动淘汰：装哪些、卸哪些由上层按格 / 视野决定。
//
// 锁与所有权：
//   - 块表是按块序号（行优先）排开的原子指针数组，每块一个槽位（1w 地图约 200KB，地图上限 MaxChunks 时 8MB）。
//     查询（Height / Material / IsWater / Probe / Chunk / Version）不取锁，只做原子读与内存计算，可以被任意多个
//     goroutine 并发调用，可以在 Nest short 池里调用。用读写锁时 10 个 goroutine 并发查询的总吞吐反而低于单线程
//     （读锁计数的缓存行在核间来回），见 README 的实测。
//   - 写入口只有 LoadBase、SetOverlay、Unload，由一把互斥锁串行，各自原子地换掉一个槽位。块数据装入后不可变
//     （换版本是换一份新数据），一次查询对每个用到的块只读一次槽位，所以同一块内不会读到两个版本或改了一半的数据。
//     跨块的查询（边界上的插值）各块分别取读到时的版本；一次改多块的编辑由上层逐块 SetOverlay，在 View 里不是原子的。
//   - SetOverlay 复制入参的切片，Chunk 返回副本：View 不与调用方（如实体 DAO）共享可变内存。
//   - 同一块的装 / 卸次序由上层保证（例如格进程按块串行地处理“装原始块 / 装覆盖块 / 卸下”）；View 只保证每次调用
//     本身是原子的。并发地对同一块 Unload 与 LoadBase，结果取决于谁后执行。
type View struct {
	meta Meta
	pack *Pack
	// water 是每块的水位线（下标同块表，Meta.WaterLevelOf 的结果），NewView 时算好、之后不变：查询与 Walker.Step 每次
	// 判断水都要它，逐个比较区域会让热路径随区域数变慢。1w 地图约 100KB。
	water []int32

	writeMu sync.Mutex // 串行 LoadBase / SetOverlay / Unload 的安装步骤
	chunks  []atomic.Pointer[viewChunk]
	loaded  atomic.Int64
}

// viewChunk 是装入的一块，装入后不再修改。
type viewChunk struct {
	heights   []uint16
	materials []byte
	version   int64 // 0 = 原始块
}

// NewView 返回空的视图，原始块从 p 读取，地图参数取自 p 的包头。p 的生命周期长于 View。
func NewView(p *Pack) *View {
	meta := p.Meta()
	water := make([]int32, meta.Chunks())
	for i := range water {
		water[i] = meta.WaterLevelOf(ChunkKey{X: int32(i % int(meta.ChunksX)), Y: int32(i / int(meta.ChunksX))})
	}
	return &View{meta: meta, pack: p, water: water, chunks: make([]atomic.Pointer[viewChunk], meta.Chunks())}
}

// WaterLevelOf 返回块的水位线（区域水位或全局水位，世界高度，厘米），与 Meta.WaterLevelOf 相同、O(1)。块不在地图内时
// 返回全局水位线。不取锁，可以在 Nest short 池里调用。
func (v *View) WaterLevelOf(key ChunkKey) int32 {
	if !v.meta.Contains(key) {
		return v.meta.WaterLevel
	}
	return v.water[v.meta.index(key)]
}

// Meta 返回地图参数（WaterRegions 是副本，调用方改它不影响视图：视图按块算好的水位表 water 在 NewView 时定下，区域
// 配置只随原始地形包换版）。有区域水位时每次调用分配一份区域表，循环里用到的先取出来。
func (v *View) Meta() Meta { return v.meta.Clone() }

// slot 返回块的槽位，块不在地图内时返回 nil。
func (v *View) slot(key ChunkKey) *atomic.Pointer[viewChunk] {
	if !v.meta.Contains(key) {
		return nil
	}
	return &v.chunks[v.meta.index(key)]
}

// LoadBase 从原始地形包装入一块。这一块已经装着（原始或覆盖）时什么都不做、返回 nil：不会用原始块盖掉覆盖块。
// 读包在锁外进行（文件 I/O + 解压，见 Pack.ReadChunk），不要在 Nest short 池里调用。读失败（含 ErrCorruptChunk）
// 时不装入任何东西。
func (v *View) LoadBase(key ChunkKey) error {
	slot := v.slot(key)
	if slot == nil {
		return fmt.Errorf("%w: chunk %v", ErrOutOfMap, key)
	}
	if slot.Load() != nil {
		return nil
	}
	c, err := v.pack.ReadChunk(key)
	if err != nil {
		return err
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if slot.Load() == nil {
		slot.Store(&viewChunk{heights: c.Heights, materials: c.Materials})
		v.loaded.Add(1)
	}
	return nil
}

// SetOverlay 装入（或更新）一块覆盖块，替换这一块现有的原始块或更旧的覆盖块。入参切片被复制。
// 拒绝：块不在地图内（ErrOutOfMap）、形状不对（ErrInvalidChunk）、Version < 1（ErrInvalidChunk）、
// BaseRevision 不等于包头（ErrBaseRevision）、比已有覆盖块的版本旧（ErrStaleOverlay）。版本相同则替换。
func (v *View) SetOverlay(key ChunkKey, o Overlay) error {
	slot := v.slot(key)
	if slot == nil {
		return fmt.Errorf("%w: chunk %v", ErrOutOfMap, key)
	}
	if err := (Chunk{Heights: o.Heights, Materials: o.Materials}).Validate(); err != nil {
		return fmt.Errorf("overlay %v: %w", key, err)
	}
	if o.Version < 1 {
		return fmt.Errorf("%w: overlay %v has version %d, want ≥ 1", ErrInvalidChunk, key, o.Version)
	}
	if o.BaseRevision != v.meta.BaseRevision {
		return fmt.Errorf("%w: overlay %v is based on revision %d, pack is %d", ErrBaseRevision, key, o.BaseRevision, v.meta.BaseRevision)
	}
	next := &viewChunk{
		heights:   append([]uint16(nil), o.Heights...),
		materials: append([]byte(nil), o.Materials...),
		version:   o.Version,
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	cur := slot.Load()
	if cur != nil && cur.version > o.Version {
		return fmt.Errorf("%w: overlay %v version %d, loaded %d", ErrStaleOverlay, key, o.Version, cur.version)
	}
	slot.Store(next)
	if cur == nil {
		v.loaded.Add(1)
	}
	return nil
}

// Unload 卸下一块（原始或覆盖），返回它之前是否装着。之后依赖这块的查询返回 ErrNotLoaded。
func (v *View) Unload(key ChunkKey) bool {
	slot := v.slot(key)
	if slot == nil {
		return false
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	if slot.Swap(nil) == nil {
		return false
	}
	v.loaded.Add(-1)
	return true
}

// Version 返回一块是否装着，以及装着的版本（0 = 原始块，≥ 1 = 覆盖块的 Version）。
func (v *View) Version(key ChunkKey) (version int64, loaded bool) {
	c := v.load(key)
	if c == nil {
		return 0, false
	}
	return c.version, true
}

// Len 返回装着的块数。
func (v *View) Len() int { return int(v.loaded.Load()) }

// Chunk 返回一块数据的副本与版本；没装着时 loaded 为 false。上层第一次改一块时可以用它（或 Pack.ReadChunk）
// 取得初值建 DAO。
func (v *View) Chunk(key ChunkKey) (c Chunk, version int64, loaded bool) {
	vc := v.load(key)
	if vc == nil {
		return Chunk{}, 0, false
	}
	return Chunk{Heights: append([]uint16(nil), vc.heights...), Materials: append([]byte(nil), vc.materials...)}, vc.version, true
}

// BrushChanges 返回把 b 用在视图当前数据上时会改变值的高度点数与材质点数（算法同 Brush.Apply）。口径：每个覆盖到的
// 采样点只算一次（共享边归右 / 上的块，与 Apply 相同）；高度、材质各自计数，一个点两者都变了算 2，值没变的不算。
// 上层按它在事务之前估算一次编辑的改动量（例如每玩家的改动点数配额）。
//
// 不复制块数据（装入的块不可变，直接读），只读，可以并发调用。参数先经 Validate；覆盖到的块没装着时返回 ErrNotLoaded。
// 结果是读到时的快照：之后别的编辑提交了，锁内真正应用时改变的点数可能不同。
func (v *View) BrushChanges(b Brush, limits Limits) (heights, materials int, err error) {
	return v.BrushChangesIn(b, limits, nil)
}

// BrushChangesIn 是只数 part 里那几块的 BrushChanges（part 为 nil 时是覆盖到的全部块；口径同 Brush.ApplyIn）。为跨格编辑
// 拆给各格而加（P5；跨格笔刷 2026-11-10 撤回后 game-demo 没有调用，保留为通用能力）。只有 part 里覆盖到的块需要装着。
func (v *View) BrushChangesIn(b Brush, limits Limits, part []ChunkKey) (heights, materials int, err error) {
	if err := b.Validate(v.meta, limits); err != nil {
		return 0, 0, err
	}
	in := partSet(part)
	chunks := map[ChunkKey]*viewChunk{}
	for _, key := range b.Chunks(v.meta) {
		if !in.has(key) {
			continue
		}
		c := v.load(key)
		if c == nil {
			return 0, 0, fmt.Errorf("%w: brush needs chunk %v", ErrNotLoaded, key)
		}
		chunks[key] = c
	}
	b.covers(v.meta, func(gx, gy, d2 int64) {
		c := chunks[ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}]
		if c == nil {
			return
		}
		index := (gy%ChunkSamples)*ChunkSamples + gx%ChunkSamples
		height, material := c.heights[index], c.materials[index]
		nextHeight, nextMaterial := b.point(limits, height, material, d2)
		if nextHeight != height {
			heights++
		}
		if nextMaterial != material {
			materials++
		}
	})
	return heights, materials, nil
}

// load 读一块的槽位；不在地图内或没装着时返回 nil。
func (v *View) load(key ChunkKey) *viewChunk {
	if slot := v.slot(key); slot != nil {
		return slot.Load()
	}
	return nil
}

// Height 返回世界坐标 (x, y) 处地面的世界高度（厘米）。
//
// 算法（整数定点，客户端须逐位相同）：S = SampleSpacing，gx = x / S、fx = x % S（y 同理，地图内坐标非负）。
// 取四个采样点的原始值 h00 = (gx, gy)、h10 = (gx+1, gy)、h01 = (gx, gy+1)、h11 = (gx+1, gy+1)，
//
//	num    = (h00·(S−fx) + h10·fx)·(S−fy) + (h01·(S−fx) + h11·fx)·fy
//	Height = HeightBase + (num·HeightStep + S²/2) / S²      （num ≥ 0，整数除法即向下取整，结果四舍五入）
//
// 邻点的规则：
//   - 权重为 0 的邻点不读取（fx = 0 时不取 gx+1 列，fy = 0 时不取 gy+1 行），因此采样点正上方的位置只需要本块。
//   - gx+1 越出地图右边界时用 gx 列代替（边缘复制），上边界同理；地图边上的位置不需要地图外的数据。
//   - 其余情况下邻点可能在右 / 上 / 右上邻块（共享边归右 / 上块），邻块没装入时返回 ErrNotLoaded，不猜测。
//
// 坐标在地图外返回 ErrOutOfMap。
func (v *View) Height(x, y int64) (int32, error) {
	g, err := v.probe(x, y)
	return g.Height, err
}

// Material 返回世界坐标 (x, y) 所在 1m 格左下角采样点的材质（只需要本块）。地图外返回 ErrOutOfMap，
// 本块没装入返回 ErrNotLoaded。
func (v *View) Material(x, y int64) (byte, error) {
	if err := v.inMap(x, y); err != nil {
		return 0, err
	}
	key, i := PointIndex(x, y)
	c := v.load(key)
	if c == nil {
		return 0, fmt.Errorf("%w: chunk %v", ErrNotLoaded, key)
	}
	return c.materials[i], nil
}

// IsWater 报告世界坐标 (x, y) 的地面是否低于水位线（Height(x, y) < 所在块的水位线，等于不算水），错误同 Height。
// 水位线取 (x, y) 所在 1m 格左下角采样点所属的块（即包含 (x, y) 的块）：区域水位或全局水位（Meta.WaterLevelOf）。
// 挖到水位线以下自动是水、填到水位线以上自动是陆地，不存“是不是水”，客户端按同一规则算（方案 §5、D3）。
func (v *View) IsWater(x, y int64) (bool, error) {
	g, err := v.probe(x, y)
	return g.Water, err
}

// WaterDepth 返回世界坐标 (x, y) 的水深（厘米，Ground.Depth：水位线 − 地面高度，按 int64 算，不是水时为 0），错误同 Height。
func (v *View) WaterDepth(x, y int64) (int64, error) {
	g, err := v.probe(x, y)
	return g.Depth(), err
}

// Probe 一次取得高度、材质与是否为水，错误同 Height。高度与材质出自本块的同一个版本。
func (v *View) Probe(x, y int64) (Ground, error) { return v.probe(x, y) }

func (v *View) inMap(x, y int64) error {
	b := v.meta.Bounds()
	if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
		return fmt.Errorf("%w: point (%d, %d)", ErrOutOfMap, x, y)
	}
	return nil
}

// probe 按 Height 的算法计算。四个采样点最多落在四个块里，每个块的槽位只读一次（同一块不会混用两个版本）。
func (v *View) probe(x, y int64) (Ground, error) {
	if err := v.inMap(x, y); err != nil {
		return Ground{}, err
	}
	const s = SampleSpacing
	gx, gy, fx, fy := x/s, y/s, x%s, y%s
	gx1, gy1 := gx, gy
	if fx > 0 && gx+1 < int64(v.meta.ChunksX)*ChunkSamples {
		gx1 = gx + 1
	}
	if fy > 0 && gy+1 < int64(v.meta.ChunksY)*ChunkSamples {
		gy1 = gy + 1
	}
	// 四个点所在的块：列 cx0 / cx1、行 cy0 / cy1，相同时复用同一次读取。
	cx0, cx1, cy0, cy1 := int32(gx/ChunkSamples), int32(gx1/ChunkSamples), int32(gy/ChunkSamples), int32(gy1/ChunkSamples)
	var c [2][2]*viewChunk // [行][列]
	for r, cy := range [2]int32{cy0, cy1} {
		for col, cx := range [2]int32{cx0, cx1} {
			switch {
			case r == 1 && cy1 == cy0:
				c[1][col] = c[0][col]
			case col == 1 && cx1 == cx0:
				c[r][1] = c[r][0]
			default:
				key := ChunkKey{X: cx, Y: cy}
				if c[r][col] = v.load(key); c[r][col] == nil {
					return Ground{}, fmt.Errorf("%w: chunk %v", ErrNotLoaded, key)
				}
			}
		}
	}
	at := func(c *viewChunk, gx, gy int64) int64 {
		return int64(c.heights[(gy%ChunkSamples)*ChunkSamples+gx%ChunkSamples])
	}
	h00, h10, h01, h11 := at(c[0][0], gx, gy), at(c[0][1], gx1, gy), at(c[1][0], gx, gy1), at(c[1][1], gx1, gy1)
	num := (h00*(s-fx)+h10*fx)*(s-fy) + (h01*(s-fx)+h11*fx)*fy
	h := v.meta.HeightBase + int32((num*HeightStep+s*s/2)/(s*s))
	m := c[0][0].materials[(gy%ChunkSamples)*ChunkSamples+gx%ChunkSamples]
	level := v.water[v.meta.index(ChunkKey{X: cx0, Y: cy0})]
	return Ground{Height: h, Material: m, Water: h < level, WaterLevel: level}, nil
}
