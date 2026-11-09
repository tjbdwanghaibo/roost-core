package terrain

import (
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 地面行走规则（可编辑地形方案 P3，§20）：移动与寻路共用的一套坡度 / 台阶 / 下落 / 水的判定。
//
// 判定的单位是“节点”：1m 采样网格上的采样点 (gx, gy)。世界坐标 p 属于离它最近的采样点（四舍五入，Walker.Cell），
// 节点的地面高度就是该采样点的高度（View.Height 在采样点正上方的值，只读本块、没有插值误差）。走路时位置每从一个
// 节点的范围进入相邻节点的范围，按两个节点判定一次能不能走（Walker.Step）；寻路以同样的节点为图、同样的判定为边
// （Walker.FindPath），所以寻路给出的路线，移动逐节点检查时一定走得通（同一时刻的地形下）。
//
// 为什么按采样点而不是按位置连续地算坡度：高度场 1m 一个采样，坡度在采样之间是线性的，按采样点判定已经是地形本身
// 能表达的全部精度；连续判定要选一个“多远算一次”的基线，基线随心跳步长变（5Hz、60 单位 / 秒时一拍只走 12 厘米，
// 厘米级取整的高差会把缓坡算成 100% 的坡），按节点则与心跳、速度无关，移动与寻路天然一致。代价：一面采样间距内的
// 陡崖与 1m 宽的坡在判定上没有区别（客户端按同一份高度场生成的 mesh 也是 1m 宽的坡），见 WalkRules。

// WalkRules 是地面行走规则，全部是整数（厘米 / 千分比），服务端与客户端可逐位复现。DefaultWalkRules 是首版初值
// （方案 §8.1）。零值不是可用的规则，用 Validate 检查。
//
// 相邻两个节点 a → b 的判定（run 是水平距离：正交 100、对角 141 厘米；dh = 站立高度(b) − 站立高度(a)）：
//
//   - 上行（dh > 0）：dh·1000 ≤ MaxSlope·run（坡度不超过上限），或 dh ≤ MaxStep（台阶：比坡度上限陡、但不高于
//     MaxStep 的高差也能迈上去）。
//   - 下行（dh < 0）：|dh|·1000 ≤ MaxSlope·run（正常下坡），或 |dh| ≤ max(MaxStep, MaxDrop)（比坡度上限陡的下行
//     按“跳下 / 滑下”处理，落差不超过 MaxDrop）。
//   - 对角（gx、gy 都变）：除了对角本身，两条“先横后竖 / 先竖后横”的正交路线也都要走得通（不切角），移动在拐角
//     附近无论先进入哪个正交节点都不会被挡。
//
// 站立高度：普通节点是地面高度；水深（WaterLevel − 地面）超过 WadeDepth 的节点是“游泳”节点，站立高度取水面
// WaterLevel（浮在水面上），所以水里节点之间没有高差、上岸按“水面 → 岸上地面”的高差判定。
//
// 1m 采样下的取值说明：正交一步的坡度上限是 MaxSlope·100/1000 厘米；MaxStep 只在它大于这个值时起作用（缺省 45° 时
// 1m 内 100 厘米的高差都算坡，MaxStep 50 不起作用，保留给调低坡度上限的玩法）。陡崖在采样间距内是一面“坡”：MaxDrop
// 300 指的是相邻采样点的落差，不是整面悬崖的高度。
type WalkRules struct {
	// MaxSlope 是可走坡度的上限，千分比（高差 × 1000 / 水平距离）：1000 = 45°，577 ≈ 30°。
	MaxSlope int32
	// MaxStep 是台阶上限（厘米）：比坡度上限陡、但高差不超过它的一步仍可走（上下都算）。
	MaxStep int32
	// MaxDrop 是比坡度上限陡的下行允许的最大落差（厘米）；0 表示陡于坡度上限的下行只按 MaxStep 放行。
	MaxDrop int32
	// WadeDepth 是涉水深度（厘米）：水深不超过它的节点照常行走（速度不变），更深的是游泳节点。
	WadeDepth int32
	// SwimSpeed 是游泳速度相对行走速度的千分比，(0, 1000]；0 表示不能游泳：游泳节点不可进入（站在里面的可以走出来）。
	SwimSpeed int32
	// UphillCost、DownhillCost 是寻路的坡度代价（千分比）：每升高 / 降低 1 厘米折合多走多少厘米平路 × 1000。
	// 只影响寻路选哪条路，不影响移动速度；2000 表示爬 1 米高相当于多走 2 米平路。
	UphillCost, DownhillCost int32
	// SearchWeight 是寻路估价的放大倍数（千分比，加权 A*）：0 或 1000 是最优路线；大于 1000 时找到的路线代价不超过
	// 最优的 SearchWeight / 1000 倍，换来展开的节点少得多。原因：估价只算水平距离（爬升多少事先不知道），坡度代价
	// 越大、地面越起伏，最优 A* 要证明“没有更省的路”展开的节点越多——起伏地面上 200m 的路线，上坡代价 2 时最优搜索
	// 展开约 1.4 万个节点，远超 4096 的上限；放大 1.5 倍时约 700 个（方案 §20 的实测）。
	SearchWeight int32
}

// DefaultWalkRules 是首版初值（方案 §8.1，按常见 MMO / 开放世界取值）：坡度 45°（Unreal 缺省 44.8°、Unity 45°）、
// 台阶 50 厘米（Unreal 45、Unity 30）、陡降 3 米、涉水 50 厘米、游泳速度 60%（常见 50%～70%）、上坡代价 2（爬 1 米
// 折合 2 米平路），下坡不加代价。
//
// 寻路估价放大 1.5 倍（SearchWeight 1500）：路线代价至多比最优多 50%，起伏地面上 200m 的路线能在 4096 个节点内找到。
var DefaultWalkRules = WalkRules{MaxSlope: 1000, MaxStep: 50, MaxDrop: 300, WadeDepth: 50, SwimSpeed: 600, UphillCost: 2000, SearchWeight: 1500}

// ErrWalkRules 表示行走规则的参数不合法。
var ErrWalkRules = errors.New("terrain: invalid walk rules")

// Validate 检查参数范围：都不为负，SwimSpeed 不超过 1000（寻路的估价按行走速度算，游泳更快会让估价不再是下界），
// SearchWeight 为 0 或不小于 1000（小于 1000 只会多展开节点、结果不变，没有意义）。
func (r WalkRules) Validate() error {
	switch {
	case r.MaxSlope < 0, r.MaxStep < 0, r.MaxDrop < 0, r.WadeDepth < 0, r.UphillCost < 0, r.DownhillCost < 0:
		return fmt.Errorf("%w: negative value in %+v", ErrWalkRules, r)
	case r.SwimSpeed < 0 || r.SwimSpeed > 1000:
		return fmt.Errorf("%w: swim speed %d‰ must be in [0, 1000]", ErrWalkRules, r.SwimSpeed)
	case r.SearchWeight != 0 && r.SearchWeight < 1000:
		return fmt.Errorf("%w: search weight %d‰ must be 0 or at least 1000", ErrWalkRules, r.SearchWeight)
	}
	return nil
}

// climb 报告高差 dh 在水平距离 run 上是否走得过去（规则见 WalkRules）。
// 高差按 int64 传入：站立高度是 int32（HeightBase 可以为负，水面是独立配置的 WaterLevel），判定不依赖它们的取值范围。
func (r WalkRules) climb(dh, run int64) bool {
	d := dh
	if d < 0 {
		d = -d
	}
	if d*1000 <= int64(r.MaxSlope)*run {
		return true
	}
	if dh > 0 {
		return dh <= int64(r.MaxStep)
	}
	return d <= int64(max(r.MaxStep, r.MaxDrop))
}

// 节点间的水平距离（厘米）：对角取 141（≈ 100·√2，寻路代价与估价都用它，估价仍是下界）。
const (
	runStraight = SampleSpacing
	runDiagonal = 141
)

// Walker 把行走规则用在一个地形视图上：移动逐节点问 Step，寻路用 FindPath。只读视图、不取锁、不分配（Step），
// 可以在 Nest short 池里调用；值很小，按值或指针传都行，字段在使用期间不要改。
type Walker struct {
	View  *View
	Rules WalkRules
	// Bounds 非空时只在其中走：节点的世界位置（采样点）在 Bounds 外的不可进入（场景的世界边界）。空表示整张地图。
	Bounds spatial.Rect
	// Occupied 是建筑占位钩子（方案 §6，P4 才有建筑）：返回 true 的采样点不可进入。签名与 terrainmap.Rules.Occupied
	// 相同（块 + 块内下标），只能读内存、不能阻塞；nil 表示没有占位。
	Occupied func(key ChunkKey, index int32) bool
}

// footing 是一个节点对行走的意义。
type footing struct {
	stand int32 // 站立高度（厘米）：地面，游泳节点是水面
	speed int32 // 速度千分比
	known bool  // 在地图内且块已装入（站在这里有定义）
	enter bool  // 可以进入
}

// nodeRange 返回可用节点的范围（含两端）：地图内的采样点，Bounds 非空时再限在 Bounds 里。
func (w *Walker) nodeRange() (minX, minY, maxX, maxY int64) {
	meta := w.View.meta
	minX, minY = 0, 0
	maxX, maxY = int64(meta.ChunksX)*ChunkSamples-1, int64(meta.ChunksY)*ChunkSamples-1
	if !w.Bounds.Empty() {
		b := spatial.NormalizeRect(w.Bounds)
		minX = max(minX, -floorDiv(-b.Min.X, SampleSpacing)) // 向上取整
		minY = max(minY, -floorDiv(-b.Min.Y, SampleSpacing))
		maxX = min(maxX, floorDiv(b.Max.X-1, SampleSpacing))
		maxY = min(maxY, floorDiv(b.Max.Y-1, SampleSpacing))
	}
	return minX, minY, maxX, maxY
}

// Cell 返回世界坐标 p 所属的节点（最近的采样点，坐标是采样网格上的 (gx, gy)，不是世界坐标）。p 在地图（与 Bounds）
// 内时结果夹到可用节点的范围里：地图右 / 上边缘最后半米、Bounds 边上不足半米的位置归最近的可用节点。p 在外面时不夹，
// 得到的节点不可用。实现 movement.Surface。
func (w *Walker) Cell(p spatial.Point) spatial.Point {
	gx := floorDiv(p.X+SampleSpacing/2, SampleSpacing)
	gy := floorDiv(p.Y+SampleSpacing/2, SampleSpacing)
	if w.inArea(p) {
		minX, minY, maxX, maxY := w.nodeRange()
		gx, gy = min(max(gx, minX), maxX), min(max(gy, minY), maxY)
	}
	return spatial.Point{X: gx, Y: gy}
}

// inArea 报告世界坐标是否在地图内（且在 Bounds 内，Bounds 非空时）。
func (w *Walker) inArea(p spatial.Point) bool {
	if !w.View.meta.Bounds().Contains(p) {
		return false
	}
	return w.Bounds.Empty() || spatial.NormalizeRect(w.Bounds).Contains(p)
}

// Step 报告能否从节点 from 走进节点 to（Cell 的结果），以及在 to 里的速度千分比（1000 = 正常行走）。实现
// movement.Surface：
//
//   - from == to：站在这里，只要求节点在地图内、块已装入；速度是这个节点的（游泳节点是 SwimSpeed；不能游泳却站在
//     深水里时按 1000，让它能走出来）。
//   - 相邻（八邻域）：按 WalkRules 判定；对角还要求不切角。
//   - 其余（不相邻、地图外、块没装入、Bounds 外、被占位、不能游泳的深水）：不能走。不猜测没装入的块。
func (w *Walker) Step(from, to spatial.Point) (speed int32, ok bool) {
	a := w.footingAt(nil, from.X, from.Y)
	if from == to {
		return a.speed, a.known
	}
	dx, dy := to.X-from.X, to.Y-from.Y
	if dx < -1 || dx > 1 || dy < -1 || dy > 1 {
		return 0, false
	}
	b := w.footingAt(nil, to.X, to.Y)
	if !w.pass(a, b, dx, dy) {
		return 0, false
	}
	if dx != 0 && dy != 0 {
		// 不切角：两条正交路线都要走得通。
		for _, c := range [2]spatial.Point{{X: to.X, Y: from.Y}, {X: from.X, Y: to.Y}} {
			cf := w.footingAt(nil, c.X, c.Y)
			if !w.pass(a, cf, c.X-from.X, c.Y-from.Y) || !w.pass(cf, b, to.X-c.X, to.Y-c.Y) {
				return 0, false
			}
		}
	}
	return b.speed, true
}

// pass 是相邻节点 a → b（偏移 dx, dy）这一条边本身的判定；对角的拐角由调用方另查。
func (w *Walker) pass(a, b footing, dx, dy int64) bool {
	if !a.known || !b.enter {
		return false
	}
	if dx == 0 || dy == 0 {
		return w.Rules.climb(int64(b.stand)-int64(a.stand), runStraight)
	}
	return w.Rules.climb(int64(b.stand)-int64(a.stand), runDiagonal)
}

// Standable 报告世界坐标 p 所属的节点能否进入（在地图与 Bounds 内、块已装入、没被占位、不是不能游泳的深水）。
// 用于“能不能站在这里”（传送落点、出生点、寻路终点），不判断从哪里走过来。
func (w *Walker) Standable(p spatial.Point) bool {
	if !w.inArea(p) {
		return false
	}
	c := w.Cell(p)
	return w.footingAt(nil, c.X, c.Y).enter
}

// Swimming 报告世界坐标 p 所属的节点是不是游泳节点（水深超过 WadeDepth）。地图外返回 ErrOutOfMap，块没装入返回
// ErrNotLoaded。客户端按同一规则自己算，不需要同步状态。
func (w *Walker) Swimming(p spatial.Point) (bool, error) {
	ground, err := w.nodeGround(p)
	if err != nil {
		return false, err
	}
	return w.swims(ground), nil
}

// StandZ 返回站在世界坐标 p 的角色的高度（厘米）：游泳节点上是水面 WaterLevel，否则是 p 处的地面高度
// （View.Height，双线性插值）。服务端位置只有 X / Y（cellworld EntityState、DAO 都没有 Z），高度由它推出，
// 客户端同算。错误同 View.Height。
func (w *Walker) StandZ(p spatial.Point) (int32, error) {
	ground, err := w.nodeGround(p)
	if err != nil {
		return 0, err
	}
	if w.swims(ground) {
		return w.View.meta.WaterLevel, nil
	}
	return w.View.Height(p.X, p.Y)
}

// nodeGround 返回 p 所属节点的地面高度。
func (w *Walker) nodeGround(p spatial.Point) (int32, error) {
	if err := w.View.inMap(p.X, p.Y); err != nil {
		return 0, err
	}
	c := w.Cell(p)
	return w.View.Height(c.X*SampleSpacing, c.Y*SampleSpacing)
}

// swims 报告地面高度 ground 的节点是不是游泳节点。
func (w *Walker) swims(ground int32) bool {
	water := w.View.meta.WaterLevel
	return ground < water && int64(water)-int64(ground) > int64(w.Rules.WadeDepth)
}

// footingAt 计算节点 (gx, gy) 的 footing。s 非 nil 时块取这次搜索的快照，否则直接读视图的槽位（移动每次判定只读
// 两到六个节点，不需要快照）。
func (w *Walker) footingAt(s *search, gx, gy int64) footing {
	minX, minY, maxX, maxY := w.nodeRange()
	if gx < 0 || gy < 0 || gx > int64(w.View.meta.ChunksX)*ChunkSamples-1 || gy > int64(w.View.meta.ChunksY)*ChunkSamples-1 {
		return footing{}
	}
	key := ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}
	var c *viewChunk
	if s != nil {
		c = s.chunk(key)
	} else {
		c = w.View.load(key)
	}
	if c == nil {
		return footing{}
	}
	index := int32((gy%ChunkSamples)*ChunkSamples + gx%ChunkSamples)
	ground := w.View.meta.WorldHeight(c.heights[index])
	f := footing{stand: ground, speed: 1000, known: true, enter: true}
	if w.swims(ground) {
		f.stand = w.View.meta.WaterLevel
		if w.Rules.SwimSpeed > 0 {
			f.speed = w.Rules.SwimSpeed
		} else {
			f.enter = false
		}
	}
	if gx < minX || gy < minY || gx > maxX || gy > maxY {
		f.enter = false
	}
	if f.enter && w.Occupied != nil && w.Occupied(key, index) {
		f.enter = false
	}
	return f
}

// ChunkVersion 是一块与读到的版本（0 = 原始块）。
type ChunkVersion struct {
	Key     ChunkKey
	Version int64
}

// Route 是 FindPath 的结果。
type Route struct {
	// Points 是要走的世界坐标：起点、起点所属节点、途经的节点（采样点的世界坐标）、终点所属节点、终点，相邻重复的
	// 去掉。首尾各多一个节点位置，是为了每一段都只跨一个节点边界（起点到它所属节点、节点到节点、终点节点到终点），
	// 移动逐节点检查时与寻路判定的是同一组相邻节点。
	Points []spatial.Point
	// Chunks 是路线判定读过的块（路线上的节点与对角的拐角节点所在块）与搜索时读到的版本，按 (Y, X) 排序。
	Chunks []ChunkVersion
	// Visited 是展开的节点数（≤ maxVisited）。
	Visited int
}

// Fresh 报告路线依赖的块在视图里是否仍是同一版本（都还装着、版本没变）。块被改过（版本变了）、卸下了，路线就作废，
// 要重新寻路（方案 D9“按块版本失效”）：改出一道墙、挖出一条沟都会让经过那块的路线失效。只判断路线还走不走得通
// 的前提，不判断是否出现了更短的路（没经过的块变了不算）。占位钩子（P4）的变化不在版本里，由上层另行失效。
func (r Route) Fresh(v *View) bool {
	for _, dep := range r.Chunks {
		version, loaded := v.Version(dep.Key)
		if !loaded || version != dep.Version {
			return false
		}
	}
	return true
}

// DefaultMaxVisited 是 FindPath 单次最多展开的节点数（沿用 demo 网格寻路的 4096）：约 64m × 64m 的面积，开阔地上
// 足够 200m 以上的直线路线；绕不过去的目标会扫满它，答 spatial.ErrPathBudgetExhausted（不是“没有路”）。
const DefaultMaxVisited = 4096

// FindPath 在节点图上做八方向 A*，规则与 Step 相同：边的代价 = (水平距离 × 1000 + 上升 × UphillCost + 下降 ×
// DownhillCost) × 1000 / 目标节点的速度千分比；估价是八方向距离 × SearchWeight（速度千分比 ≤ 1000、坡度代价非负，
// 距离 × 1000 是下界：SearchWeight 为 1000 时结果是代价最小的路线，更大时代价至多是最小的 SearchWeight / 1000 倍；
// 已关闭的节点不重开，这个上界仍然成立）。同代价按“估价小、先发现”的次序展开，同一视图状态下结果确定。
//
// 搜索期间每块只读一次槽位（快照）：并发的 SetOverlay 不会让同一次搜索读到同一块的两个版本，Route.Chunks 的版本就是
// 搜索用的版本。maxVisited ≤ 0 时用 DefaultMaxVisited。
//
// 错误：起点 / 终点在地图（或 Bounds）外 → ErrOutOfMap；起点所属节点的块没装入 → ErrNotLoaded；终点所属节点不可
// 进入（块没装入时是 ErrNotLoaded）或确实走不到 → spatial.ErrNoPath；展开超过上限 → spatial.ErrPathBudgetExhausted
// （可能有路，调用方可以退回直线走、由移动逐节点挡住）。
func (w *Walker) FindPath(from, to spatial.Point, maxVisited int) (Route, error) {
	if maxVisited <= 0 {
		maxVisited = DefaultMaxVisited
	}
	if !w.inArea(from) || !w.inArea(to) {
		return Route{}, fmt.Errorf("%w: route %v → %v", ErrOutOfMap, from, to)
	}
	s := newSearch(w)
	start, goal := w.Cell(from), w.Cell(to)
	si, gi := s.node(start.X, start.Y), s.node(goal.X, goal.Y)
	switch {
	case !s.nodes[si].f.known:
		return Route{}, fmt.Errorf("%w: route start %v", ErrNotLoaded, from)
	case !s.nodes[gi].f.known:
		return Route{}, fmt.Errorf("%w: route goal %v", ErrNotLoaded, to)
	case !s.nodes[gi].f.enter:
		return Route{}, fmt.Errorf("%w: goal %v cannot be stood on", spatial.ErrNoPath, to)
	}
	visited, err := s.run(si, gi, maxVisited)
	if err != nil {
		return Route{Visited: visited}, err
	}
	return s.route(from, to, gi, visited), nil
}

// search 是一次 A* 的状态。
type search struct {
	w      *Walker
	width  int64 // 地图每行的采样点数（节点编号 = gy·width + gx）
	nodes  []searchNode
	index  map[int64]int32
	open   openHeap
	seq    uint32
	keys   []ChunkKey // 本次搜索读过的块（快照）
	chunks []*viewChunk
	last   int
}

type searchNode struct {
	gx, gy int64
	f      footing
	g      int64 // 已知最小代价；-1 = 还没到过
	parent int32
	closed bool
}

func newSearch(w *Walker) *search {
	return &search{
		w:      w,
		width:  int64(w.View.meta.ChunksX) * ChunkSamples,
		nodes:  make([]searchNode, 0, 1024),
		index:  make(map[int64]int32, 1024),
		open:   make(openHeap, 0, 1024),
		last:   -1,
		keys:   make([]ChunkKey, 0, 16),
		chunks: make([]*viewChunk, 0, 16),
	}
}

// chunk 返回本次搜索的块快照：第一次用到时读槽位，之后都用同一份（没装着也记下 nil）。
func (s *search) chunk(key ChunkKey) *viewChunk {
	if s.last >= 0 && s.keys[s.last] == key {
		return s.chunks[s.last]
	}
	for i, k := range s.keys {
		if k == key {
			s.last = i
			return s.chunks[i]
		}
	}
	c := s.w.View.load(key)
	s.keys, s.chunks = append(s.keys, key), append(s.chunks, c)
	s.last = len(s.keys) - 1
	return c
}

// node 返回节点 (gx, gy) 的下标，第一次用到时建记录；地图外返回 -1。
func (s *search) node(gx, gy int64) int32 {
	if gx < 0 || gy < 0 || gx >= s.width || gy >= int64(s.w.View.meta.ChunksY)*ChunkSamples {
		return -1
	}
	id := gy*s.width + gx
	if i, ok := s.index[id]; ok {
		return i
	}
	i := int32(len(s.nodes))
	s.nodes = append(s.nodes, searchNode{gx: gx, gy: gy, f: s.w.footingAt(s, gx, gy), g: -1, parent: -1})
	s.index[id] = i
	return i
}

// 八个方向：先正交后对角，顺序固定（结果确定）。
var walkDirs = [8][2]int64{{1, 0}, {-1, 0}, {0, 1}, {0, -1}, {1, 1}, {-1, 1}, {1, -1}, {-1, -1}}

// passable 是搜索里的相邻判定（与 Walker.Step 同一规则，节点取本次搜索的快照）。
func (s *search) passable(ai, bi int32, dx, dy int64) bool {
	a, b := s.nodes[ai].f, s.nodes[bi].f
	if !s.w.pass(a, b, dx, dy) {
		return false
	}
	if dx == 0 || dy == 0 {
		return true
	}
	ax, ay := s.nodes[ai].gx, s.nodes[ai].gy
	for _, c := range [2][2]int64{{ax + dx, ay}, {ax, ay + dy}} {
		ci := s.node(c[0], c[1])
		if ci < 0 {
			return false
		}
		cf := s.nodes[ci].f
		if !s.w.pass(a, cf, c[0]-ax, c[1]-ay) || !s.w.pass(cf, b, ax+dx-c[0], ay+dy-c[1]) {
			return false
		}
	}
	return true
}

// cost 是 a → b 的边代价。
func (s *search) cost(a, b footing, diagonal bool) int64 {
	run := int64(runStraight)
	if diagonal {
		run = runDiagonal
	}
	c := run * 1000
	if dh := int64(b.stand) - int64(a.stand); dh > 0 {
		c += dh * int64(s.w.Rules.UphillCost)
	} else {
		c -= dh * int64(s.w.Rules.DownhillCost)
	}
	return c * 1000 / int64(b.speed)
}

// estimate 是八方向距离（对角 141、正交 100）× 估价倍数（SearchWeight，0 按 1000）。
func (s *search) estimate(ax, ay, bx, by int64) int64 {
	weight := int64(s.w.Rules.SearchWeight)
	if weight == 0 {
		weight = 1000
	}
	dx, dy := ax-bx, ay-by
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	lo, hi := min(dx, dy), max(dx, dy)
	return (hi*runStraight + lo*(runDiagonal-runStraight)) * weight
}

// run 做 A*，返回展开的节点数。
func (s *search) run(si, gi int32, maxVisited int) (int, error) {
	goal := &s.nodes[gi]
	gx, gy := goal.gx, goal.gy
	s.nodes[si].g = 0
	s.push(si, 0, s.estimate(s.nodes[si].gx, s.nodes[si].gy, gx, gy))
	visited := 0
	for len(s.open) > 0 {
		e := s.open.pop()
		cur := e.node
		if s.nodes[cur].closed || e.g != s.nodes[cur].g {
			continue
		}
		visited++
		if visited > maxVisited {
			return maxVisited, spatial.ErrPathBudgetExhausted
		}
		s.nodes[cur].closed = true
		if cur == gi {
			return visited, nil
		}
		cx, cy, cg := s.nodes[cur].gx, s.nodes[cur].gy, s.nodes[cur].g
		for _, d := range walkDirs {
			ni := s.node(cx+d[0], cy+d[1])
			if ni < 0 || s.nodes[ni].closed || !s.passable(cur, ni, d[0], d[1]) {
				continue
			}
			ng := cg + s.cost(s.nodes[cur].f, s.nodes[ni].f, d[0] != 0 && d[1] != 0)
			if old := s.nodes[ni].g; old >= 0 && ng >= old {
				continue
			}
			s.nodes[ni].g, s.nodes[ni].parent = ng, cur
			s.push(ni, ng, s.estimate(cx+d[0], cy+d[1], gx, gy))
		}
	}
	return visited, fmt.Errorf("%w: from (%d, %d) to (%d, %d)", spatial.ErrNoPath, s.nodes[si].gx, s.nodes[si].gy, gx, gy)
}

func (s *search) push(node int32, g, h int64) {
	s.seq++
	s.open.push(openEntry{f: g + h, h: h, g: g, seq: s.seq, node: node})
}

// route 从终点沿 parent 回溯出路线，并收集依赖的块版本。
func (s *search) route(from, to spatial.Point, gi int32, visited int) Route {
	var chain []int32
	for i := gi; i >= 0; i = s.nodes[i].parent {
		chain = append(chain, i)
	}
	slices.Reverse(chain)
	points := make([]spatial.Point, 0, len(chain)+2)
	add := func(p spatial.Point) {
		if len(points) == 0 || points[len(points)-1] != p {
			points = append(points, p)
		}
	}
	add(from)
	deps := map[ChunkKey]struct{}{}
	depend := func(gx, gy int64) {
		deps[ChunkKey{X: int32(gx / ChunkSamples), Y: int32(gy / ChunkSamples)}] = struct{}{}
	}
	for k, i := range chain {
		n := s.nodes[i]
		add(spatial.Point{X: n.gx * SampleSpacing, Y: n.gy * SampleSpacing})
		depend(n.gx, n.gy)
		if k > 0 {
			p := s.nodes[chain[k-1]]
			if p.gx != n.gx && p.gy != n.gy { // 对角：拐角节点也参与了判定
				depend(n.gx, p.gy)
				depend(p.gx, n.gy)
			}
		}
	}
	add(to)
	chunks := make([]ChunkVersion, 0, len(deps))
	for key := range deps {
		var version int64
		if c := s.chunk(key); c != nil {
			version = c.version
		}
		chunks = append(chunks, ChunkVersion{Key: key, Version: version})
	}
	slices.SortFunc(chunks, func(a, b ChunkVersion) int {
		if a.Key.Y != b.Key.Y {
			return int(a.Key.Y) - int(b.Key.Y)
		}
		return int(a.Key.X) - int(b.Key.X)
	})
	return Route{Points: points, Chunks: chunks, Visited: visited}
}

// openEntry 是开放表的一项；按 (f, h, seq) 排序。g 用来识别过期项（节点后来找到了更短的路）。
type openEntry struct {
	f, h, g int64
	seq     uint32
	node    int32
}

func (a openEntry) less(b openEntry) bool {
	if a.f != b.f {
		return a.f < b.f
	}
	if a.h != b.h {
		return a.h < b.h
	}
	return a.seq < b.seq
}

// openHeap 是二叉小顶堆（不用 container/heap：它经 any 装箱，每次 push 分配一次）。
type openHeap []openEntry

func (h *openHeap) push(e openEntry) {
	*h = append(*h, e)
	q := *h
	for i := len(q) - 1; i > 0; {
		p := (i - 1) / 2
		if !q[i].less(q[p]) {
			break
		}
		q[i], q[p] = q[p], q[i]
		i = p
	}
}

func (h *openHeap) pop() openEntry {
	q := *h
	top := q[0]
	n := len(q) - 1
	q[0] = q[n]
	q = q[:n]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && q[l].less(q[m]) {
			m = l
		}
		if r < n && q[r].less(q[m]) {
			m = r
		}
		if m == i {
			break
		}
		q[i], q[m] = q[m], q[i]
		i = m
	}
	*h = q
	return top
}
