package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Cluster errors.
var (
	ErrAreaOverlap   = errors.New("policy: area bounds overlap")
	ErrAreaUnknown   = errors.New("policy: point outside every area")
	ErrAreaDuplicate = errors.New("policy: duplicate area id")
)

// AreaID identifies one area (world-plane shard) inside a cluster. The id is
// semantic only — it deliberately does not encode process placement, so a
// future cross-process handover can reuse the same topology (that migration
// path additionally needs remote-entity ownership fences and cross-process
// subscription replication, and is out of scope here).
type AreaID int64

// AOICluster stitches per-region AOI instances into one seamless
// interest space over a SHARED world coordinate system: areas are
// non-overlapping rectangles of the same plane, adjacency follows from the
// geometry, and ids are cluster-global. 编号约定同 AOI：观察者是视野编号、目标按实体 ID，
// 观察者带自身实体（self）与自己的视野；边界镜像按观察者自己的离开半径决定登记到哪些区域。
//
// Seamlessness rests on two mechanisms:
//
//   - Boundary mirroring: an observer whose leave-radius box crosses into a
//     neighboring area is transparently registered there too, so entities in
//     the neighbor's border strip are visible — area seams cast no shadow.
//   - Net-change flushing: per-area events update a per-pair area->band table
//     and Flush emits only the difference against the last emitted state. A
//     make-before-break migration (add to the destination area, then remove
//     from the source, inside one MoveSubject call) therefore produces zero
//     events when visibility is in fact unchanged — downstream subscriptions
//     never blink across a border crossing.
//
// Concurrency: cluster membership and boundary mirroring are serialized by mu.
// 区域共用 AOI 的块索引实现；cluster 跨区域迁移仍是单一协调操作，
// 不以区域数量承诺并行写吞吐。正式 Interest 事实准入不经过这把锁。
type AOICluster struct {
	mu     sync.Mutex
	config AOIConfig
	areas  map[AreaID]*clusterArea
	// order keeps deterministic area iteration (ascending AreaID).
	order     []AreaID
	subjects  map[int64]clusterPlacement
	observers map[int64]clusterPlacement
	// visible tracks, per (observer, subject) pair, the band contributed by
	// each area and the state last emitted downstream.
	visible map[interestPair]*clusterVisibility
	// observerPairs indexes visible by observer for capacity settlement.
	observerPairs map[int64]map[int64]*clusterVisibility
	pending       []InterestEvent
	// watchers 是目标移动时容量重选的暂存（看得见旧、新位置的观察者），只在 mu 内使用。
	watchers []int64
}

type clusterArea struct {
	id      AreaID
	bounds  spatial.Rect
	manager *AOI
}

type clusterPlacement struct {
	at spatial.Point
	// home is the area containing the position; observers are additionally
	// mirrored into every area their leave radius touches.
	home AreaID
	// self / view 只对观察者有意义：自身实体 ID 与归一后的视野。
	self int64
	view ObserverView
}

type interestPair struct {
	observer int64
	subject  int64
}

type clusterVisibility struct {
	bands       map[AreaID]int
	emitted     bool
	emittedBand int
	// suppressed marks a pair held back by the cluster-level MaxVisible cap:
	// visible to some area manager, but not surfaced downstream.
	suppressed bool
}

func NewAOICluster(config AOIConfig) (*AOICluster, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.BlockSize <= 0 {
		return nil, spatial.ErrInvalidBounds
	}
	config = config.withViewDefaults()
	return &AOICluster{
		config:        config,
		areas:         make(map[AreaID]*clusterArea),
		subjects:      make(map[int64]clusterPlacement),
		observers:     make(map[int64]clusterPlacement),
		visible:       make(map[interestPair]*clusterVisibility),
		observerPairs: make(map[int64]map[int64]*clusterVisibility),
	}, nil
}

// AddArea registers a world-plane rectangle as an area. Bounds must not
// overlap any existing area. Adding an area after observers exist retrofits
// their boundary mirrors immediately — without that, a stationary observer
// near the new seam would have an unbounded visibility hole into the new
// area (its mirrors are otherwise only rebuilt when it moves).
func (c *AOICluster) AddArea(id AreaID, bounds spatial.Rect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.areas[id]; exists {
		return ErrAreaDuplicate
	}
	bounds = spatial.NormalizeRect(bounds)
	for _, area := range c.areas {
		if rectsOverlap(area.bounds, bounds) {
			return fmt.Errorf("%w: %d and %d", ErrAreaOverlap, area.id, id)
		}
	}
	areaConfig := c.config
	areaConfig.Bounds = bounds
	// Areas run uncapped: a border observer is mirrored into several areas,
	// and per-area caps would multiply its budget by the number of touched
	// areas exactly where entities are densest. The cluster enforces
	// MaxVisible globally in collect instead.
	areaConfig.MaxVisible = 0
	manager, err := NewAOI(areaConfig)
	if err != nil {
		return err
	}
	c.areas[id] = &clusterArea{id: id, bounds: bounds, manager: manager}
	c.order = append(c.order, id)
	slices.Sort(c.order)
	observers := make([]int64, 0, len(c.observers))
	for observer := range c.observers {
		observers = append(observers, observer)
	}
	slices.Sort(observers)
	for _, observer := range observers {
		placement := c.observers[observer]
		if err := c.placeObserverLocked(observer, placement.self, placement.at, placement.view); err != nil {
			return err
		}
	}
	return nil
}

func rectsOverlap(a, b spatial.Rect) bool {
	return a.Min.X < b.Max.X && b.Min.X < a.Max.X && a.Min.Y < b.Max.Y && b.Min.Y < a.Max.Y
}

func (c *AOICluster) areaAt(at spatial.Point) (*clusterArea, bool) {
	for _, id := range c.order {
		if area := c.areas[id]; area.bounds.Contains(at) {
			return area, true
		}
	}
	return nil, false
}

// AddSubject indexes a subject in the area containing its position.
func (c *AOICluster) AddSubject(id int64, at spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.subjects[id]; exists {
		return c.moveSubjectLocked(id, at)
	}
	area, ok := c.areaAt(at)
	if !ok {
		return ErrAreaUnknown
	}
	if err := area.manager.AddSubject(id, at); err != nil {
		return err
	}
	c.subjects[id] = clusterPlacement{at: at, home: area.id}
	c.collect()
	return nil
}

// MoveSubject relocates a subject; a border crossing is a make-before-break
// migration inside this one call, so downstream visibility never blinks.
func (c *AOICluster) MoveSubject(id int64, to spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moveSubjectLocked(id, to)
}

func (c *AOICluster) moveSubjectLocked(id int64, to spatial.Point) error {
	placement, exists := c.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	destination, ok := c.areaAt(to)
	if !ok {
		return ErrAreaUnknown
	}
	if destination.id == placement.home {
		if err := destination.manager.MoveSubject(id, to); err != nil {
			return err
		}
	} else {
		// Make before break: the destination sees the subject before the
		// source forgets it, and the net-change flush folds the overlap away.
		if err := destination.manager.AddSubject(id, to); err != nil {
			return err
		}
		if err := c.areas[placement.home].manager.RemoveSubject(id); err != nil {
			return err
		}
	}
	c.subjects[id] = clusterPlacement{at: to, home: destination.id}
	if c.config.MaxVisible <= 0 {
		c.collect()
		return nil
	}
	// 区域不设容量，同档移动可能没有区域事件，但距离排序已经改变。能看见该目标的观察者
	// 都登记在旧、新位置块的反向索引里（含边界镜像），区域 AOI 自己判定可见性用的也是这份索引，
	// 不扫描全部观察者。
	watchers := c.areas[placement.home].manager.appendObserverIDsAt(c.watchers[:0], placement.at)
	watchers = destination.manager.appendObserverIDsAt(watchers, to)
	slices.Sort(watchers)
	watchers = slices.Compact(watchers)
	c.watchers = watchers
	c.collectAfterMove(watchers, id, false)
	return nil
}

// RemoveSubject drops a subject from its area.
func (c *AOICluster) RemoveSubject(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	placement, exists := c.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	if err := c.areas[placement.home].manager.RemoveSubject(id); err != nil {
		return err
	}
	delete(c.subjects, id)
	c.collect()
	return nil
}

// AddObserver registers an observer in its home area and mirrors it into
// every neighboring area its leave radius touches. id 是视野编号，self 是它跟随的实体 ID（0 表示没有），
// view 按配置归一。
func (c *AOICluster) AddObserver(id, self int64, at spatial.Point, view ObserverView) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	view, err := c.config.normalizeView(view)
	if err != nil {
		return err
	}
	return c.placeObserverLocked(id, self, at, view)
}

// MoveObserver relocates an observer, rebuilding its boundary mirrors.
func (c *AOICluster) MoveObserver(id int64, to spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	placement, exists := c.observers[id]
	if !exists {
		return ErrInterestUnknown
	}
	return c.placeObserverLocked(id, placement.self, to, placement.view)
}

// SetObserverView 换观察者的视野，按新的离开半径重建边界镜像；返回归一后的视野。
func (c *AOICluster) SetObserverView(id int64, view ObserverView) (ObserverView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	placement, exists := c.observers[id]
	if !exists {
		return ObserverView{}, ErrInterestUnknown
	}
	view, err := c.config.normalizeView(view)
	if err != nil {
		return placement.view, err
	}
	return view, c.placeObserverLocked(id, placement.self, placement.at, view)
}

func (c *AOICluster) placeObserverLocked(id, self int64, at spatial.Point, view ObserverView) error {
	home, ok := c.areaAt(at)
	if !ok {
		return ErrAreaUnknown
	}
	leave := view.Radius + c.config.hysteresis()
	span := spatial.SaturatingAdd(leave, 1)
	reach := spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(at.X, leave), Y: spatial.SaturatingSub(at.Y, leave)},
		Max: spatial.Point{X: spatial.SaturatingAdd(at.X, span), Y: spatial.SaturatingAdd(at.Y, span)},
	}
	for _, areaID := range c.order {
		area := c.areas[areaID]
		inReach := rectsOverlap(area.bounds, reach)
		_, registered := area.manager.observers[id]
		switch {
		case inReach:
			// The manager clamps evaluation to its own bounds; the mirror
			// keeps the observer's true position even when outside them.
			area.manager.placeObserver(id, self, at, view)
		case registered:
			if err := area.manager.RemoveObserver(id); err != nil {
				return err
			}
		}
	}
	c.observers[id] = clusterPlacement{at: at, home: home.id, self: self, view: view}
	if c.config.MaxVisible > 0 {
		// 观察者自己移动，到所有目标的距离都变了，即使没有区域事件也要重排。
		c.collectAfterMove([]int64{id}, 0, true)
	} else {
		c.collect()
	}
	return nil
}

// RemoveObserver drops an observer and all its mirrors.
func (c *AOICluster) RemoveObserver(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.observers[id]; !exists {
		return ErrInterestUnknown
	}
	for _, areaID := range c.order {
		area := c.areas[areaID]
		if _, registered := area.manager.observers[id]; registered {
			if err := area.manager.RemoveObserver(id); err != nil {
				return err
			}
		}
	}
	delete(c.observers, id)
	c.collect()
	return nil
}

// Visible returns the observer's cluster-wide visible set in ascending order.
func (c *AOICluster) Visible(observer int64) []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result []int64
	for pair, state := range c.visible {
		if pair.observer == observer && state.emitted {
			result = append(result, pair.subject)
		}
	}
	slices.Sort(result)
	return result
}

// Flush drains the cluster's net visibility changes since the previous
// Flush, ordered by (Observer, Subject).
func (c *AOICluster) Flush() []InterestEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.collect()
	if len(c.pending) == 0 {
		return nil
	}
	events := c.pending
	c.pending = nil
	// 同一观察者与实体的事件保持产生顺序，避免把 Enter/Leave 颠倒。
	slices.SortStableFunc(events, func(a, b InterestEvent) int {
		if order := cmp.Compare(a.Observer, b.Observer); order != 0 {
			return order
		}
		return cmp.Compare(a.Subject, b.Subject)
	})
	return events
}

// collect folds the areas' raw events into per-pair area/band tables, then
// settles each touched observer: cluster-level MaxVisible (nearest-N across
// ALL areas — per-area caps would multiply a border observer's budget) picks
// the surfaced set, and only net changes against the previously emitted
// state are appended to pending.
func (c *AOICluster) collect() { c.collectAfterMove(nil, 0, false) }

// collectAfterMove 与 collect 相同，另外处理设了 MaxVisible 时同档移动带来的重选：
// 区域不设容量，同档移动没有区域事件，但最近 N 个的排序可能已经变了。observers 是可能
// 受影响、却未必收到区域事件的观察者。observersMoved 表示它们自己移动了，一律完整重排；
// 否则只有 subject 的距离变了，它跨过容量边界（入选的被挤出、被抑制的挤进来）时才重排，
// 每次移动不必为每个看得见它的观察者都排序一遍。
func (c *AOICluster) collectAfterMove(observers []int64, subject int64, observersMoved bool) {
	touchedObservers := make(map[int64]struct{})
	for _, areaID := range c.order {
		area := c.areas[areaID]
		for _, event := range area.manager.Flush() {
			pair := interestPair{observer: event.Observer, subject: event.Subject}
			state := c.visible[pair]
			if state == nil {
				state = &clusterVisibility{bands: make(map[AreaID]int)}
				c.visible[pair] = state
				pairs := c.observerPairs[pair.observer]
				if pairs == nil {
					pairs = make(map[int64]*clusterVisibility)
					c.observerPairs[pair.observer] = pairs
				}
				pairs[pair.subject] = state
			}
			switch event.Kind {
			case InterestEnter, InterestBandChanged:
				state.bands[areaID] = event.Band
			case InterestLeave:
				delete(state.bands, areaID)
			}
			touchedObservers[pair.observer] = struct{}{}
		}
	}
	// 收到区域事件的观察者已经要完整重排；其余的按移动判断。必须在归并完区域事件之后：
	// 跨区迁移、档位变化都会先改 bands，此时剩下的观察者里目标的 bands 没有变。
	for _, observer := range observers {
		if _, touched := touchedObservers[observer]; touched {
			continue
		}
		if observersMoved || c.crossesCapacityLocked(observer, subject) {
			touchedObservers[observer] = struct{}{}
		}
	}
	settle := make([]int64, 0, len(touchedObservers))
	for observer := range touchedObservers {
		settle = append(settle, observer)
	}
	slices.Sort(settle)
	for _, observer := range settle {
		c.settleObserverLocked(observer)
	}
}

// crossesCapacityLocked 判断 subject 移动后，observer 的最近 MaxVisible 个是否要换人。
// 前提：observer 上次重排后只有 subject 的位置变了（没有区域事件），所以其余目标的入选 /
// 抑制状态仍是按 (距离, ID) 排好的结果。入选的 subject 排到任一被抑制候选之后、或被抑制的
// subject 排到任一入选目标之前，才需要完整重排；O(候选数)，不分配、不排序。
func (c *AOICluster) crossesCapacityLocked(observer, subject int64) bool {
	pairs := c.observerPairs[observer]
	moved := pairs[subject]
	if moved == nil || len(moved.bands) == 0 {
		// 不是候选：没有 Enter 事件就仍不是，不影响这个观察者的选择。
		return false
	}
	at := c.observers[observer].at
	movedDistance := spatial.DistanceSquared(at, c.subjects[subject].at)
	before := func(aDistance int64, a int64, bDistance int64, b int64) bool {
		return aDistance < bDistance || aDistance == bDistance && a < b
	}
	for other, state := range pairs {
		if other == subject || len(state.bands) == 0 || state.suppressed == moved.suppressed {
			continue
		}
		distance := spatial.DistanceSquared(at, c.subjects[other].at)
		if moved.suppressed && before(movedDistance, subject, distance, other) {
			return true
		}
		if !moved.suppressed && before(distance, other, movedDistance, subject) {
			return true
		}
	}
	return false
}

// settleObserverLocked reconciles one observer's emitted set with its
// area-visible set under the cluster-level MaxVisible cap.
func (c *AOICluster) settleObserverLocked(observer int64) {
	pairs := c.observerPairs[observer]
	subjects := make([]int64, 0, len(pairs))
	for subject := range pairs {
		subjects = append(subjects, subject)
	}
	slices.Sort(subjects)
	// Capacity: keep the nearest MaxVisible of the area-visible subjects
	// (distance ties break by id — deterministic).
	if c.config.MaxVisible > 0 {
		type candidate struct {
			subject  int64
			distance int64
		}
		var candidates []candidate
		observerAt := c.observers[observer].at
		for _, subject := range subjects {
			if len(pairs[subject].bands) == 0 {
				continue
			}
			candidates = append(candidates, candidate{subject: subject, distance: spatial.DistanceSquared(observerAt, c.subjects[subject].at)})
		}
		slices.SortFunc(candidates, func(a, b candidate) int {
			if order := cmp.Compare(a.distance, b.distance); order != 0 {
				return order
			}
			return cmp.Compare(a.subject, b.subject)
		})
		for index, entry := range candidates {
			pairs[entry.subject].suppressed = index >= c.config.MaxVisible
		}
	}
	for _, subject := range subjects {
		state := pairs[subject]
		nowVisible := len(state.bands) > 0 && !state.suppressed
		band := 0
		if nowVisible {
			band = effectiveBand(state.bands)
		}
		switch {
		case nowVisible && !state.emitted:
			state.emitted, state.emittedBand = true, band
			c.pending = append(c.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestEnter, Band: band})
		case !nowVisible && state.emitted:
			state.emitted = false
			c.pending = append(c.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestLeave, Band: -1})
		case nowVisible && band != state.emittedBand:
			state.emittedBand = band
			c.pending = append(c.pending, InterestEvent{Observer: observer, Subject: subject, Kind: InterestBandChanged, Band: band})
		}
		if len(state.bands) == 0 {
			delete(c.visible, interestPair{observer: observer, subject: subject})
			delete(pairs, subject)
		}
	}
	if len(pairs) == 0 {
		delete(c.observerPairs, observer)
	}
}

// effectiveBand is the closest (smallest) band any area reports.
func effectiveBand(bands map[AreaID]int) int {
	best := -1
	for _, band := range bands {
		if best < 0 || band < best {
			best = band
		}
	}
	return best
}
