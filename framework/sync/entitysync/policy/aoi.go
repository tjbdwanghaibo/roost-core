package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Interest management errors.
var (
	ErrInterestConfig  = errors.New("policy: invalid interest config")
	ErrInterestUnknown = errors.New("policy: unknown interest id")
	// ErrInterestBudget: one observer's leave-radius box would cover more
	// blocks than the configuration allows. It is a refusal at construction
	// because the cost it guards is invisible at run time — nothing fails,
	// the manager just gets slower in proportion to a ratio nobody looked at.
	ErrInterestBudget = errors.New("policy: one observer would subscribe to more blocks than the budget allows")
	// ErrObserverView：请求的 LOD 层级不在配置的层级表里。视野大小越界不算错误，服务端夹取。
	ErrObserverView = errors.New("policy: observer view not configured")
)

// InterestEventKind classifies one visibility transition.
type InterestEventKind uint8

const (
	// InterestEnter: the subject became visible to the observer.
	InterestEnter InterestEventKind = iota
	// InterestLeave: the subject stopped being visible to the observer.
	InterestLeave
	// InterestBandChanged: the subject stayed visible but crossed into a
	// different distance band (LOD tier).
	InterestBandChanged
)

// InterestEvent is one incremental visibility change. Band carries the
// distance-band index for Enter and BandChanged, and -1 for Leave.
// Observer 是观察者（一个空间视野）的编号，Subject 是目标的实体 ID。Interest 用内部视野编号当 AOI 观察者，
// 再按挂接关系把事件扇出给玩家；单独使用 AOI 时观察者编号由调用方决定。
type InterestEvent struct {
	Observer int64
	Subject  int64
	Kind     InterestEventKind
	Band     int
}

// AOIConfig shapes an AOI.
type AOIConfig struct {
	Bounds    spatial.Rect
	BlockSize int64
	// EnterRadius admits a subject into view; LeaveRadius keeps it there.
	// EnterRadius < LeaveRadius gives the hysteresis that stops a subject
	// oscillating on the boundary from spamming Enter/Leave pairs.
	EnterRadius int64
	LeaveRadius int64
	// Bands lists distance-band outer edges in ascending order; a visible
	// subject's band is the first edge its distance fits under (subjects
	// beyond the last edge use len(Bands)). Empty means a single band 0.
	// 配置了 LODs 时 Bands 必须为空：Bands 只是“只有一层 LOD”的写法。
	Bands []int64
	// LODs 是观察者可选的 LOD 层级表，下标即 ObserverView.LOD，每层是一组距离分档（规则同 Bands）。
	// 为空时只有层级 0，分档取 Bands。每层分档递增、为正、不超过 MaxLeaveRadius，启动期校验。
	LODs [][]int64
	// 观察者视野大小（ObserverView.Radius，即进入半径）的允许范围与量化步长。
	// 离开半径 = 视野大小 + 滞回量，滞回量取缺省视野的 LeaveRadius − EnterRadius；
	// 请求的视野大小先夹到 [MinViewRadius, MaxLeaveRadius − 滞回量]，再从 MinViewRadius 起按 ViewRadiusStep 向下取整。
	// 零值：MinViewRadius 取 EnterRadius、MaxLeaveRadius 取 LeaveRadius（只允许缺省视野）、ViewRadiusStep 取 1。
	// EnterRadius 必须落在量化网格上，客户端报出缺省视野时得到的正是缺省视野。
	// 单个观察者的块预算（MaxObserverBlocks）按 MaxLeaveRadius 计算，启动期超出即拒绝。
	MinViewRadius  int64
	MaxLeaveRadius int64
	ViewRadiusStep int64
	// MaxObserverBlocks caps how many blocks ONE observer may subscribe to.
	// The count is (⌈2·MaxLeaveRadius/BlockSize⌉+1)² — the widest leave-radius box
	// discretized — and it is the cost of every observer move, twice over
	// (the observer's own block set and the reverse table). Zero takes
	// DefaultMaxObserverBlocks; a deployment that really wants a wide view
	// names a bigger number rather than discovering the cost in production.
	//
	// It is deliberately separate from MaxVisible: that one bounds how many
	// SUBJECTS an observer ends up seeing, this one bounds the index work
	// done to find them. A small view over a fine grid is cheap by the first
	// measure and expensive by this one.
	MaxObserverBlocks int
	// MaxObservedBlocks 限制单个实体额外覆盖块数；零值取1024。
	MaxObservedBlocks int
	// MaxVisible, when positive, caps an observer's visible set: an entering
	// subject closer than the current farthest evicts it, a farther one is
	// ignored. This is a broadcast-storm gate with approximate semantics —
	// an ignored subject re-qualifies on its next movement, not the moment
	// capacity frees up.
	MaxVisible int
}

// maxInterestRadius bounds LeaveRadius so subscription-box arithmetic can
// never wrap (at ± (radius+1) must stay well inside int64).
const maxInterestRadius = int64(1) << 62

// DefaultMaxObserverBlocks is the budget when the configuration does not name
// one. It is generous for the shape this index is built for — a block about
// the size of the view radius gives nine to sixteen — and still refuses the
// configurations that quietly turn an incremental AOI into a full-map scan.
const DefaultMaxObserverBlocks = 1024

// ObserverView 是一个观察者的视野：LOD 层级（AOIConfig.LODs 的下标）与视野大小（进入半径）。
// 零值是缺省视野：层级 0、Radius 取 AOIConfig.EnterRadius。
type ObserverView struct {
	LOD    int
	Radius int64
}

// withViewDefaults 补齐视野范围的零值；validate、NewAOI、NewAOICluster 都在补齐后的配置上工作。
func (c AOIConfig) withViewDefaults() AOIConfig {
	if c.MinViewRadius == 0 {
		c.MinViewRadius = c.EnterRadius
	}
	if c.MaxLeaveRadius == 0 {
		c.MaxLeaveRadius = c.LeaveRadius
	}
	if c.ViewRadiusStep == 0 {
		c.ViewRadiusStep = 1
	}
	return c
}

func (c AOIConfig) validate() error {
	if c.MaxObservedBlocks < 0 {
		return ErrInterestConfig
	}
	if c.EnterRadius <= 0 || c.LeaveRadius < c.EnterRadius || c.LeaveRadius >= maxInterestRadius {
		return ErrInterestConfig
	}
	c = c.withViewDefaults()
	if c.MinViewRadius <= 0 || c.MinViewRadius > c.EnterRadius || c.MaxLeaveRadius < c.LeaveRadius || c.MaxLeaveRadius >= maxInterestRadius {
		return fmt.Errorf("%w: view radius range [%d, leave %d] must contain the default view (enter %d, leave %d)",
			ErrInterestConfig, c.MinViewRadius, c.MaxLeaveRadius, c.EnterRadius, c.LeaveRadius)
	}
	if c.ViewRadiusStep <= 0 || (c.EnterRadius-c.MinViewRadius)%c.ViewRadiusStep != 0 {
		return fmt.Errorf("%w: enter radius %d is not on the view radius grid (min %d, step %d)",
			ErrInterestConfig, c.EnterRadius, c.MinViewRadius, c.ViewRadiusStep)
	}
	if len(c.LODs) > 0 && len(c.Bands) > 0 {
		return fmt.Errorf("%w: Bands and LODs both set", ErrInterestConfig)
	}
	for level := range max(len(c.LODs), 1) {
		bands, _ := c.lodBands(level)
		for index, edge := range bands {
			if edge <= 0 || index > 0 && edge <= bands[index-1] || edge > c.MaxLeaveRadius {
				return fmt.Errorf("%w: LOD %d band %d edge %d (ascending, positive, at most max leave radius %d)",
					ErrInterestConfig, level, index, edge, c.MaxLeaveRadius)
			}
		}
	}
	if c.BlockSize <= 0 {
		// Not this check's business: spatial.NewBlockIndex refuses it, and reporting
		// a budget for a grid that cannot exist would hide the real reason.
		return nil
	}
	budget := c.MaxObserverBlocks
	if budget <= 0 {
		budget = DefaultMaxObserverBlocks
	}
	if worst := c.worstObserverBlocks(); worst > int64(budget) {
		return fmt.Errorf("%w: max leave radius %d over block size %d covers %d blocks, budget is %d",
			ErrInterestBudget, c.MaxLeaveRadius, c.BlockSize, worst, budget)
	}
	return nil
}

// lodBands 返回某个 LOD 层级的距离分档；层级不存在时 ok 为 false。
func (c AOIConfig) lodBands(level int) (bands []int64, ok bool) {
	if len(c.LODs) == 0 {
		return c.Bands, level == 0
	}
	if level < 0 || level >= len(c.LODs) {
		return nil, false
	}
	return c.LODs[level], true
}

// maxBands 是所有层级里分档最多的那一层的分档数；距离档号的取值是 0..maxBands。
func (c AOIConfig) maxBands() int {
	most := len(c.Bands)
	for _, bands := range c.LODs {
		most = max(most, len(bands))
	}
	return most
}

// hysteresis 是离开半径比视野大小多出的量，取缺省视野的差。
func (c AOIConfig) hysteresis() int64 { return c.LeaveRadius - c.EnterRadius }

// normalizeView 把请求的视野换成服务端允许的视野：层级必须已配置；Radius 为零取缺省，
// 否则夹到允许范围并按步长向下量化。调用方传入的是补齐过零值的配置。
func (c AOIConfig) normalizeView(view ObserverView) (ObserverView, error) {
	if _, ok := c.lodBands(view.LOD); !ok {
		return ObserverView{}, fmt.Errorf("%w: LOD level %d", ErrObserverView, view.LOD)
	}
	if view.Radius == 0 {
		view.Radius = c.EnterRadius
		return view, nil
	}
	radius := min(max(view.Radius, c.MinViewRadius), c.MaxLeaveRadius-c.hysteresis())
	view.Radius = c.MinViewRadius + (radius-c.MinViewRadius)/c.ViewRadiusStep*c.ViewRadiusStep
	return view, nil
}

// worstObserverBlocks is the largest number of blocks one observer's box can
// cover, computed rather than sampled: a check that only fired once a badly
// placed observer arrived would pass every start-up check and refuse in
// production. The arithmetic saturates, so a radius near the int64 ceiling
// answers "enormous" instead of wrapping to "small".
func (c AOIConfig) worstObserverBlocks() int64 {
	blockSize := c.BlockSize
	leave := max(c.MaxLeaveRadius, c.LeaveRadius)
	span := spatial.SaturatingAdd(spatial.SaturatingAdd(leave, leave), 1)
	perSide := spatial.SaturatingAdd(spatial.DivideCeil(span, blockSize), 1)
	// The box is also clipped by the map, so a small world cannot be made
	// expensive by an enormous radius.
	if bounded := c.boundedSides(blockSize); bounded > 0 && perSide > bounded {
		perSide = bounded
	}
	if perSide >= 1<<31 {
		return int64(1) << 62
	}
	return perSide * perSide
}

func (c AOIConfig) boundedSides(blockSize int64) int64 {
	bounds := spatial.NormalizeRect(c.Bounds)
	width, widthOK := spatial.SafeSpan(bounds.Min.X, bounds.Max.X)
	height, heightOK := spatial.SafeSpan(bounds.Min.Y, bounds.Max.Y)
	if !widthOK || !heightOK || width <= 0 || height <= 0 {
		return 0
	}
	longest := width
	if height > longest {
		longest = height
	}
	return spatial.DivideCeil(longest, blockSize)
}

// interestObserver 是一个观察者（一个空间视野）。id 是视野编号；self 是视野跟随的实体 ID，
// 0 表示没有（只看不被看）。self 永远不进自己的可见集。enter / leave / bands 由 view 推出。
type interestObserver struct {
	dirty        observerDirty
	id           int64
	self         int64
	view         ObserverView
	enter, leave int64
	bands        []int64
	at           spatial.Point
	blocks       map[int64]struct{}
	visible      map[int64]int // subject id -> current band
	blockVisible map[int64]int
	pending      []InterestEvent
	blockPending []InterestEvent
	// moved 是本批次里落在这个观察者订阅块内移动过的目标（写阶段追加，可重复，读阶段归一）。
	moved []int64
	// settled：自上次完整重评或 evaluateMoved 以来，可见集没有被逐对评估改过，下面的“未入选下界”有效。
	// 逐对评估（串行路径的 evaluatePair）不补位，之后的状态可能与完整重评不同，所以它清掉 settled。
	settled bool
	// refused / refusedMin：可能存在进入半径内却未入选的目标（被容量拒绝或被淘汰），refusedMin 是它们
	// 距离平方的下界（保守，只降不升，完整重评时重算）。refused 为假表示没有这样的目标。
	refused    bool
	refusedMin int64
	// farthest*：可见集里最远的成员（同距取较小 ID）的缓存，供 evictFarther 与 evaluateMoved 共用；
	// 可见集变化或成员移动后失效（evaluatePair 一律失效，evaluateMoved 见到移动过的成员时重算）。
	farthestKnown, farthestOK         bool
	farthestSubject, farthestDistance int64
}

// AOI is the incremental interest (AOI) layer over spatial.BlockIndex:
// observers subscribe to the blocks their leave radius covers, subject and
// observer movement re-evaluates only the affected neighborhood, and Flush
// drains the resulting Enter/Leave/BandChanged events deterministically.
//
// 编号：观察者是空间视野（Interest 给的内部编号），目标按实体 ID 编号，两张表互不相通；观察者登记时带上跟随的实体（self），
// 自排除比的是 self，不是观察者 ID。每个观察者有自己的视野（ObserverView），进出半径与距离分档都读观察者的值。
//
// 并发边界：公开写方法由 Interest 或 AOICluster 协调，不能由外部并发调用。
// 物理成员索引复用 BlockIndex 的块级读写锁；扩展块保存观察关系与额外覆盖。
// 批次写入结束后，不同 observer 可以并行计算，各自拥有可见集和事件缓冲。
type AOI struct {
	config       AOIConfig
	index        *spatial.BlockIndex
	subjects     map[int64]spatial.Point
	observers    map[int64]*interestObserver
	blocks       map[int64]*aoiBlock
	observed     map[int64][]int64
	pending      []InterestEvent
	blockPending []InterestEvent
	batching     bool
	// 仅 endBatch 的并行读阶段为 true；worker 只写自己的 observer 缓冲。
	parallelEvents bool
	dirty          map[int64]*interestObserver
	serialWork     aoiWorkspace
	workerWork     []aoiWorkspace
	// fullScans 累计完整候选遍历（evaluateObserver 的空间部分）次数，供测试断言重评范围。
	fullScans uint64
}

func NewAOI(config AOIConfig) (*AOI, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	index, err := spatial.NewBlockIndex(config.Bounds, config.BlockSize)
	if err != nil {
		return nil, err
	}
	config = config.withViewDefaults()
	config.Bounds = index.Bounds()
	config.Bands = slices.Clone(config.Bands)
	config.LODs = slices.Clone(config.LODs)
	for level, bands := range config.LODs {
		config.LODs[level] = slices.Clone(bands)
	}
	if config.MaxObservedBlocks == 0 {
		config.MaxObservedBlocks = 1024
	}
	return &AOI{
		config:    config,
		index:     index,
		subjects:  make(map[int64]spatial.Point),
		observers: make(map[int64]*interestObserver),
		blocks:    make(map[int64]*aoiBlock), observed: make(map[int64][]int64), dirty: make(map[int64]*interestObserver),
	}, nil
}

// Bounds returns the managed area.
func (m *AOI) Bounds() spatial.Rect { return m.config.Bounds }

func (m *AOI) band(observer *interestObserver, to spatial.Point) int {
	for index, edge := range observer.bands {
		if spatial.WithinDistance(observer.at, to, edge) {
			return index
		}
	}
	return len(observer.bands)
}

func (m *AOI) emit(observer, subject int64, kind InterestEventKind, band int) {
	event := InterestEvent{Observer: observer, Subject: subject, Kind: kind, Band: band}
	if o := m.observers[observer]; m.parallelEvents && o != nil {
		o.pending = append(o.pending, event)
	} else {
		m.pending = append(m.pending, event)
	}
}

// AddSubject indexes a subject and evaluates it against the neighborhood's
// observers. Id zero is rejected: spatial.BlockIndex silently refuses it, which
// would leave the subject half-tracked (visible through some evaluation
// paths and invisible through block scans).
func (m *AOI) AddSubject(id int64, at spatial.Point) error {
	if id == 0 {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	if _, exists := m.subjects[id]; exists {
		return m.MoveSubject(id, at)
	}
	m.subjects[id] = at
	m.index.Add(id, at)
	m.subjectChanged(id, at, false, at, at)
	return nil
}

// MoveSubject re-indexes a subject and re-evaluates the union of its old and
// new block neighborhoods.
func (m *AOI) MoveSubject(id int64, to spatial.Point) error {
	from, exists := m.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	m.index.Move(id, from, to)
	m.subjects[id] = to
	m.subjectChanged(id, to, false, from, to)
	return nil
}

// RemoveSubject drops a subject, emitting Leave to every observer that saw it.
func (m *AOI) RemoveSubject(id int64) error {
	at, exists := m.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	_ = m.SetObservedBlocks(id, nil)
	m.index.Remove(id, at)
	delete(m.subjects, id)
	m.subjectChanged(id, at, true, at, at)
	return nil
}

// AddObserver registers an observer and evaluates its initial visible set.
// id 是视野编号；self 是视野跟随的实体 ID（0 表示不跟随），它永远不出现在这个视野的可见集里。
// view 先按配置归一（层级必须存在，视野大小夹取量化）。已登记的观察者按新位置、新视野重新登记。
func (m *AOI) AddObserver(id, self int64, at spatial.Point, view ObserverView) error {
	if id == 0 {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	view, err := m.config.normalizeView(view)
	if err != nil {
		return err
	}
	m.placeObserver(id, self, at, view)
	return nil
}

// placeObserver registers or updates an observer whose position may lie
// outside this manager's bounds: AOICluster mirrors border observers into
// neighboring areas at their true world position, and block subscription
// clamps to this area's bounds on its own. view 已归一。
func (m *AOI) placeObserver(id, self int64, at spatial.Point, view ObserverView) {
	observer, exists := m.observers[id]
	reason := dirtySpatial
	if !exists {
		observer = &interestObserver{id: id, blocks: make(map[int64]struct{}), visible: make(map[int64]int)}
		m.observers[id] = observer
		reason |= dirtyCoverage
	} else if observer.self != self {
		// 自身实体换了：额外覆盖的可见集也按新的 self 重新排除。
		reason |= dirtyCoverage
	}
	observer.self, observer.at = self, at
	m.setView(observer, view)
	if m.resubscribe(observer) {
		reason |= dirtyCoverage
	}
	m.refreshObserver(observer, reason)
}

func (m *AOI) setView(observer *interestObserver, view ObserverView) {
	observer.view = view
	observer.bands, _ = m.config.lodBands(view.LOD)
	observer.enter = view.Radius
	observer.leave = view.Radius + m.config.hysteresis()
}

// MoveObserver relocates an observer and re-evaluates its whole visible set.
func (m *AOI) MoveObserver(id int64, to spatial.Point) error {
	observer, exists := m.observers[id]
	if !exists {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	m.placeObserver(id, observer.self, to, observer.view)
	return nil
}

// SetObserverView 换一个观察者的视野并完整重评：视野变大时新进入半径内的目标进入，变小时超出离开半径的离开，
// 只换层级时按新分档产生 BandChanged。返回归一后的视野。
func (m *AOI) SetObserverView(id int64, view ObserverView) (ObserverView, error) {
	observer, exists := m.observers[id]
	if !exists {
		return ObserverView{}, ErrInterestUnknown
	}
	view, err := m.config.normalizeView(view)
	if err != nil {
		return observer.view, err
	}
	m.placeObserver(id, observer.self, observer.at, view)
	return view, nil
}

// ObserverViewOf 返回观察者当前的（归一后的）视野。
func (m *AOI) ObserverViewOf(id int64) (ObserverView, bool) {
	observer, exists := m.observers[id]
	if !exists {
		return ObserverView{}, false
	}
	return observer.view, true
}

// RemoveObserver drops an observer, emitting Leave for its visible set.
func (m *AOI) RemoveObserver(id int64) error {
	observer, exists := m.observers[id]
	if !exists {
		return ErrInterestUnknown
	}
	for block := range observer.blocks {
		m.unsubscribeBlock(observer, block)
	}
	subjects := sortedVisible(observer.visible)
	for _, subject := range subjects {
		m.emit(observer.id, subject, InterestLeave, -1)
	}
	m.pending = append(m.pending, observer.pending...)
	m.blockPending = append(m.blockPending, observer.blockPending...)
	for subject := range observer.blockVisible {
		m.blockPending = append(m.blockPending, InterestEvent{Observer: id, Subject: subject, Kind: InterestLeave, Band: -1})
	}
	delete(m.dirty, id)
	delete(m.observers, id)
	return nil
}

// Visible returns the observer's current visible set in ascending subject
// order (rebuild/debugging aid).
func (m *AOI) Visible(observer int64) []int64 {
	registered, exists := m.observers[observer]
	if !exists {
		return nil
	}
	return sortedVisible(registered.visible)
}

// Flush drains the accumulated events. Events are ordered by (Observer,
// Subject) with same-pair events keeping their occurrence order, so the
// output is a deterministic function of the operation sequence.
func (m *AOI) Flush() []InterestEvent {
	// 并行批次已在 endBatch 归集；空闲 Flush 不遍历全部观察者。
	if len(m.pending) == 0 {
		return nil
	}
	events := m.pending
	m.pending = nil
	// 同一观察者与实体的事件保持产生顺序，避免把 Enter/Leave 颠倒。
	slices.SortStableFunc(events, func(a, b InterestEvent) int {
		if order := cmp.Compare(a.Observer, b.Observer); order != 0 {
			return order
		}
		return cmp.Compare(a.Subject, b.Subject)
	})
	return events
}

// resubscribe diffs the observer's block subscriptions against the blocks
// its own leave-radius box currently covers.
func (m *AOI) resubscribe(observer *interestObserver) (changed bool) {
	reach := spatial.SaturatingAdd(observer.leave, 1)
	box := spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(observer.at.X, observer.leave), Y: spatial.SaturatingSub(observer.at.Y, observer.leave)},
		Max: spatial.Point{X: spatial.SaturatingAdd(observer.at.X, reach), Y: spatial.SaturatingAdd(observer.at.Y, reach)},
	}
	next, _ := m.index.BlockRects(box)
	for block := range observer.blocks {
		if _, keep := next[block]; !keep {
			changed = true
			m.unsubscribeBlock(observer, block)
		}
	}
	for block := range next {
		if _, subscribed := observer.blocks[block]; !subscribed {
			changed = true
			observer.blocks[block] = struct{}{}
			b := m.block(block)
			b.mu.Lock()
			b.observers[observer.id] = observer
			b.mu.Unlock()
		}
	}
	return changed
}

func (m *AOI) unsubscribeBlock(observer *interestObserver, block int64) {
	delete(observer.blocks, block)
	if b := m.blocks[block]; b != nil {
		b.mu.Lock()
		delete(b.observers, observer.id)
		b.mu.Unlock()
	}
}

// appendObserverIDsAt 把这些位置所在块登记的观察者 ID 追加到 dst 并返回，可能重复、不排序；
// 不建中间集合，供 AOICluster 在自己的协调锁内复用暂存切片做容量重选。
func (m *AOI) appendObserverIDsAt(dst []int64, points ...spatial.Point) []int64 {
	for _, p := range points {
		if b := m.blocks[m.index.BlockIndex(p)]; b != nil {
			b.mu.RLock()
			for id := range b.observers {
				dst = append(dst, id)
			}
			b.mu.RUnlock()
		}
	}
	return dst
}

// observersAt 合并旧、新格子的观察者，只排序一次。同格移动仍需评估距离变化。
func (m *AOI) observersAt(at spatial.Point, other ...spatial.Point) []*interestObserver {
	seen := make(map[int64]*interestObserver)
	positions := append([]spatial.Point{at}, other...)
	for _, p := range positions {
		if b := m.blocks[m.index.BlockIndex(p)]; b != nil {
			b.mu.RLock()
			for id, o := range b.observers {
				seen[id] = o
			}
			b.mu.RUnlock()
		}
	}
	result := make([]*interestObserver, 0, len(seen))
	for _, o := range seen {
		result = append(result, o)
	}
	slices.SortFunc(result, func(a, b *interestObserver) int { return cmp.Compare(a.id, b.id) })
	return result
}

// subjectChanged 处理一个目标的登记、移动或移除：from / to 是旧、新位置（登记与移除时相同），两处所在块的观察者受影响。
// 批次写阶段只把目标记到这些观察者上（不建临时集合，重复由读阶段归一），读阶段再决定逐对评估还是完整重评；
// 否则立即逐对评估（evaluateSubjectFor）。
func (m *AOI) subjectChanged(subject int64, at spatial.Point, removed bool, from, to spatial.Point) {
	if !m.batching {
		if from == to {
			m.evaluateSubjectFor(m.observersAt(from), subject, at, removed)
		} else {
			m.evaluateSubjectFor(m.observersAt(from, to), subject, at, removed)
		}
		return
	}
	first := m.index.BlockIndex(from)
	m.markMovedIn(first, subject)
	if second := m.index.BlockIndex(to); second != first {
		m.markMovedIn(second, subject)
	}
}

// markMovedIn 把移动的目标记到登记在块 block 上的每个观察者；已要完整重评的观察者不必再记。
func (m *AOI) markMovedIn(block, subject int64) {
	b := m.blocks[block]
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, observer := range b.observers {
		if observer.dirty&dirtySpatial == 0 {
			observer.moved = append(observer.moved, subject)
		}
		observer.dirty |= dirtyMoved
		m.dirty[observer.id] = observer
	}
}

// evaluateSubjectFor runs the visibility state machine for one subject
// against each candidate observer (outside a batch).
func (m *AOI) evaluateSubjectFor(observers []*interestObserver, subject int64, at spatial.Point, removed bool) {
	for _, observer := range observers {
		m.evaluatePair(observer, subject, at, removed)
	}
}

type entryCandidate struct {
	subject  int64
	at       spatial.Point
	distance int64
}

// evaluateObserver re-evaluates an observer's entire visible set: current
// members against its leave radius, subscription-neighborhood subjects
// against its enter radius.
func (m *AOI) evaluateObserver(observer *interestObserver, work *aoiWorkspace, reason observerDirty) {
	if reason&dirtyCoverage != 0 {
		m.evaluateObserved(observer)
	}
	moved := observer.moved
	observer.moved = observer.moved[:0]
	if cap(observer.moved) > aoiWorkspaceLimit {
		observer.moved = nil
	}
	if reason&dirtySpatial == 0 {
		if reason&dirtyMoved == 0 || m.evaluateMoved(observer, work, moved) {
			return
		}
	}
	work.fullScans++
	observer.refused, observer.refusedMin, observer.farthestKnown = false, 0, false
	work.visible = work.visible[:0]
	for id := range observer.visible {
		work.visible = append(work.visible, id)
	}
	slices.Sort(work.visible)
	for _, subject := range work.visible {
		at, exists := m.subjects[subject]
		m.evaluatePair(observer, subject, at, !exists)
	}
	blocks := work.blocks[:0]
	for block := range observer.blocks {
		blocks = append(blocks, block)
	}
	slices.Sort(blocks)
	work.blocks = blocks
	// Admit new candidates nearest-first: block-order admission could let a
	// farther subject enter and be evicted by a nearer one within the same
	// evaluation, emitting a transient Enter+Leave pair downstream.
	candidates := work.candidates[:0]
	if work.seen == nil {
		work.seen = make(map[int64]struct{})
	}
	seen := work.seen
	clear(seen)
	query := work.query[:0]
	for _, block := range blocks {
		query = m.index.AppendBlockIDs(query[:0], block)
		for _, subject := range query {
			if _, alreadyVisible := observer.visible[subject]; alreadyVisible {
				continue
			}
			if _, duplicate := seen[subject]; duplicate {
				continue
			}
			seen[subject] = struct{}{}
			// 自身和进入半径外的实体不会入场，排序前排除；距离判断保留溢出安全语义。
			if at, exists := m.subjects[subject]; exists && subject != observer.self && spatial.WithinDistance(observer.at, at, observer.enter) {
				candidates = append(candidates, entryCandidate{subject: subject, at: at, distance: spatial.DistanceSquared(observer.at, at)})
			}
		}
	}
	work.query, work.candidates = query, candidates
	work.seenPeak = max(work.seenPeak, len(seen))
	m.admitNearest(observer, candidates)
	observer.settled = true
}

// admitNearest 按距离从近到远（同距按 ID）让候选入选，满额时只淘汰严格更远的成员。
// 最近的剩余候选都无法替换时，后续候选也不会改变可见集；这个被拒的距离记作未入选下界。
// 等距离仍保留原成员；成功腾出位置后 evaluatePair 不会再次淘汰。
func (m *AOI) admitNearest(observer *interestObserver, candidates []entryCandidate) {
	slices.SortFunc(candidates, func(a, b entryCandidate) int {
		if order := cmp.Compare(a.distance, b.distance); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})
	for _, candidate := range candidates {
		if m.config.MaxVisible > 0 && len(observer.visible) >= m.config.MaxVisible && !m.evictFarther(observer, candidate.at) {
			noteRefused(observer, candidate.distance)
			break
		}
		m.evaluatePair(observer, candidate.subject, candidate.at, false)
	}
}

// movedMember 报告 ids（升序）里是否有当前可见成员。
func movedMember(observer *interestObserver, ids []int64) bool {
	for _, subject := range ids {
		if _, visible := observer.visible[subject]; visible {
			return true
		}
	}
	return false
}

// noteRefused 记一个进入半径内却没入选的目标的距离平方（被拒或被淘汰），维护未入选下界。
func noteRefused(observer *interestObserver, distance int64) {
	if !observer.refused || distance < observer.refusedMin {
		observer.refused, observer.refusedMin = true, distance
	}
}

// evaluateMoved 是只有目标移动时的读阶段：观察者没动、视野没变，只评估本批移动过的目标，结果与 evaluateObserver
// 的完整重评逐项相同（事件、顺序与可见集）；不能证明相同时返回 false，由调用方完整重评。
//
// 为什么相同：完整重评先按 ID 重评可见成员，再把进入半径内的未入选目标按距离从近到远补入。没动的成员与没动的
// 未入选目标相对观察者的距离不变——成员重评不产生事件；未入选目标在上次完整重评（或上次本函数）结束时都不比
// 最远成员近（settled），距离不小于 refusedMin。所以只要本批之后可见集仍是满的、最远成员不比 refusedMin 远，
// 没动的未入选目标在完整重评里也一个都进不来，完整重评的事件就只来自移动过的目标，按同样的顺序评估它们即可。
// 没有未入选目标（!refused）时无论满不满都成立。成员离开腾出位置、或成员移远超过下界时，回到完整重评补位。
func (m *AOI) evaluateMoved(observer *interestObserver, work *aoiWorkspace, moved []int64) bool {
	if !observer.settled {
		return false
	}
	ids := append(work.visible[:0], moved...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	work.visible = ids
	if limit := m.config.MaxVisible; limit > 0 && observer.refused {
		// 本批之后（成员重评之后）可见集的人数与最远距离。没有成员移动时成员都仍在离开半径内，
		// 直接用最远成员缓存；有成员移动时按新位置数一遍。
		count, farthest := len(observer.visible), int64(-1)
		if movedMember(observer, ids) {
			count = 0
			for subject := range observer.visible {
				at, exists := m.subjects[subject]
				if !exists || !spatial.WithinDistance(observer.at, at, observer.leave) {
					continue
				}
				count++
				farthest = max(farthest, spatial.DistanceSquared(observer.at, at))
			}
		} else if _, distance, ok := m.farthestMember(observer); ok {
			farthest = distance
		}
		if count < limit || farthest > observer.refusedMin {
			return false
		}
	}
	for _, subject := range ids {
		if _, visible := observer.visible[subject]; visible {
			at, exists := m.subjects[subject]
			m.evaluatePair(observer, subject, at, !exists)
		}
	}
	candidates := work.candidates[:0]
	for _, subject := range ids {
		if _, visible := observer.visible[subject]; visible {
			continue
		}
		if at, exists := m.subjects[subject]; exists && subject != observer.self && spatial.WithinDistance(observer.at, at, observer.enter) {
			candidates = append(candidates, entryCandidate{subject: subject, at: at, distance: spatial.DistanceSquared(observer.at, at)})
		}
	}
	work.candidates = candidates
	m.admitNearest(observer, candidates)
	observer.settled = true
	return true
}

func (m *AOI) evaluatePair(observer *interestObserver, subject int64, at spatial.Point, removed bool) {
	// 单独的逐对评估不补位；完整重评与 evaluateMoved 在评估结束后重新置位。
	// 这对目标可能是移动过的成员或新成员，最远成员缓存一并失效。
	observer.settled, observer.farthestKnown = false, false
	previousBand, wasVisible := observer.visible[subject]
	// 自身实体不进可见集（self 为 0 时不会命中：实体 ID 不为零）；self 换过时旧可见里的它按离开处理。
	if removed || subject == observer.self {
		if wasVisible {
			delete(observer.visible, subject)
			m.emit(observer.id, subject, InterestLeave, -1)
		}
		return
	}
	if wasVisible {
		if !spatial.WithinDistance(observer.at, at, observer.leave) {
			delete(observer.visible, subject)
			m.emit(observer.id, subject, InterestLeave, -1)
			return
		}
		if band := m.band(observer, at); band != previousBand {
			observer.visible[subject] = band
			m.emit(observer.id, subject, InterestBandChanged, band)
		}
		return
	}
	if !spatial.WithinDistance(observer.at, at, observer.enter) {
		return
	}
	if m.config.MaxVisible > 0 && len(observer.visible) >= m.config.MaxVisible {
		if !m.evictFarther(observer, at) {
			return
		}
	}
	band := m.band(observer, at)
	observer.visible[subject] = band
	m.emit(observer.id, subject, InterestEnter, band)
}

// evictFarther makes room for a subject at the given distance by evicting
// the farthest current member if it is strictly farther (ties keep the
// incumbent; the farthest scan breaks distance ties by id for determinism).
func (m *AOI) evictFarther(observer *interestObserver, at spatial.Point) bool {
	farthestSubject, farthestDistance, hasCandidate := m.farthestMember(observer)
	if !hasCandidate || spatial.DistanceSquared(observer.at, at) >= farthestDistance {
		return false
	}
	// 被淘汰的成员可能仍在进入半径内，计入未入选下界。
	noteRefused(observer, farthestDistance)
	delete(observer.visible, farthestSubject)
	observer.farthestKnown = false
	m.emit(observer.id, farthestSubject, InterestLeave, -1)
	return true
}

// farthestMember 返回可见集里仍存在的最远成员（同距取较小 ID）及其距离平方；没有成员时 ok 为 false。
// 结果缓存在观察者上，直到可见集变化或成员移动。
func (m *AOI) farthestMember(observer *interestObserver) (subject, distance int64, ok bool) {
	if observer.farthestKnown {
		return observer.farthestSubject, observer.farthestDistance, observer.farthestOK
	}
	subject, distance = -1, -1
	for member := range observer.visible {
		position, exists := m.subjects[member]
		if !exists {
			continue
		}
		d := spatial.DistanceSquared(observer.at, position)
		// 非零负数也是合法 ID，不能用 ID 的符号表示是否找到成员。
		if !ok || d > distance || (d == distance && member < subject) {
			subject, distance, ok = member, d, true
		}
	}
	observer.farthestKnown, observer.farthestOK = true, ok
	observer.farthestSubject, observer.farthestDistance = subject, distance
	return subject, distance, ok
}

func sortedVisible(visible map[int64]int) []int64 {
	result := make([]int64, 0, len(visible))
	for id := range visible {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}
