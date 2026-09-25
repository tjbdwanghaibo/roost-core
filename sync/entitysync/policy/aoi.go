package policy

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/tjbdwanghaibo/roost-core/spatial"
)

// Interest management errors.
var (
	ErrInterestConfig  = errors.New("policy: invalid interest config")
	ErrInterestUnknown = errors.New("policy: unknown interest id")
	// ErrInterestBudget: one observer's leave-radius box would cover more
	// blocks than the configuration allows. It is a refusal at construction
	// because the cost it guards is invisible at run time — nothing fails,
	// the manager just gets slower in proportion to a ratio nobody looked at
	// (RR-20260918-08).
	ErrInterestBudget = errors.New("policy: one observer would subscribe to more blocks than the budget allows")
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
	Bands []int64
	// MaxObserverBlocks caps how many blocks ONE observer may subscribe to.
	// The count is (⌈2·LeaveRadius/BlockSize⌉+1)² — the leave-radius box
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

func (c AOIConfig) validate() error {
	if c.EnterRadius <= 0 || c.LeaveRadius < c.EnterRadius || c.LeaveRadius >= maxInterestRadius {
		return ErrInterestConfig
	}
	for index, edge := range c.Bands {
		if edge <= 0 || index > 0 && edge <= c.Bands[index-1] {
			return ErrInterestConfig
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
		return fmt.Errorf("%w: leave radius %d over block size %d covers %d blocks, budget is %d",
			ErrInterestBudget, c.LeaveRadius, c.BlockSize, worst, budget)
	}
	return nil
}

// worstObserverBlocks is the largest number of blocks one observer's box can
// cover, computed rather than sampled: a check that only fired once a badly
// placed observer arrived would pass every start-up check and refuse in
// production. The arithmetic saturates, so a radius near the int64 ceiling
// answers "enormous" instead of wrapping to "small".
func (c AOIConfig) worstObserverBlocks() int64 {
	blockSize := c.BlockSize
	span := spatial.SaturatingAdd(spatial.SaturatingAdd(c.LeaveRadius, c.LeaveRadius), 1)
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

type interestObserver struct {
	id      int64
	at      spatial.Point
	blocks  map[int64]struct{}
	visible map[int64]int // subject id -> current band
}

// AOI is the incremental interest (AOI) layer over spatial.BlockIndex:
// observers subscribe to the blocks their leave radius covers, subject and
// observer movement re-evaluates only the affected neighborhood, and Flush
// drains the resulting Enter/Leave/BandChanged events deterministically.
//
// Concurrency: unlike spatial.BlockIndex, an AOI is NOT safe for
// concurrent use. It is scene-private state, owned and ticked serially by
// the scene's handler; paying for locks here would buy nothing. For multiple
// concurrently ticking rooms use AOICluster, which adds its own lock.
type AOI struct {
	config    AOIConfig
	index     *spatial.BlockIndex
	subjects  map[int64]spatial.Point
	observers map[int64]*interestObserver
	// blockObservers is the nine-grid subscription table: block index ->
	// observers whose leave-radius box covers it.
	blockObservers    map[int64]map[int64]*interestObserver
	pending           []InterestEvent
	blocksScratch     []int64
	queryScratch      []int64
	candidatesScratch []entryCandidate
	seenScratch       map[int64]struct{}
}

func NewAOI(config AOIConfig) (*AOI, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	index, err := spatial.NewBlockIndex(config.Bounds, config.BlockSize)
	if err != nil {
		return nil, err
	}
	return &AOI{
		config:         config,
		index:          index,
		subjects:       make(map[int64]spatial.Point),
		observers:      make(map[int64]*interestObserver),
		blockObservers: make(map[int64]map[int64]*interestObserver),
	}, nil
}

// Bounds returns the managed area.
func (m *AOI) Bounds() spatial.Rect { return m.config.Bounds }

func (m *AOI) band(distanceFrom, to spatial.Point) int {
	for index, edge := range m.config.Bands {
		if spatial.WithinDistance(distanceFrom, to, edge) {
			return index
		}
	}
	return len(m.config.Bands)
}

func (m *AOI) emit(observer, subject int64, kind InterestEventKind, band int) {
	m.pending = append(m.pending, InterestEvent{Observer: observer, Subject: subject, Kind: kind, Band: band})
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
	m.evaluateSubjectFor(m.observersAt(at), id, at, false)
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
	affected := m.observersAt(from, to)
	m.evaluateSubjectFor(affected, id, to, false)
	return nil
}

// RemoveSubject drops a subject, emitting Leave to every observer that saw it.
func (m *AOI) RemoveSubject(id int64) error {
	at, exists := m.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	m.index.Remove(id, at)
	delete(m.subjects, id)
	m.evaluateSubjectFor(m.observersAt(at), id, at, true)
	return nil
}

// AddObserver registers an observer and evaluates its initial visible set.
// An id may be both an observer and a subject; it never observes itself.
func (m *AOI) AddObserver(id int64, at spatial.Point) error {
	if id == 0 {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(at) {
		return spatial.ErrInvalidBounds
	}
	return m.addObserverUnbounded(id, at)
}

// addObserverUnbounded registers an observer whose position may lie outside
// this manager's bounds: AOICluster mirrors border observers into
// neighboring rooms at their true world position, and block subscription
// clamps to this room's bounds on its own.
func (m *AOI) addObserverUnbounded(id int64, at spatial.Point) error {
	if _, exists := m.observers[id]; exists {
		return m.moveObserverUnbounded(id, at)
	}
	observer := &interestObserver{id: id, at: at, blocks: make(map[int64]struct{}), visible: make(map[int64]int)}
	m.observers[id] = observer
	m.resubscribe(observer)
	m.evaluateObserver(observer)
	return nil
}

// MoveObserver relocates an observer and re-evaluates its whole visible set.
func (m *AOI) MoveObserver(id int64, to spatial.Point) error {
	if _, exists := m.observers[id]; !exists {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(to) {
		return spatial.ErrInvalidBounds
	}
	return m.moveObserverUnbounded(id, to)
}

func (m *AOI) moveObserverUnbounded(id int64, to spatial.Point) error {
	observer, exists := m.observers[id]
	if !exists {
		return ErrInterestUnknown
	}
	observer.at = to
	m.resubscribe(observer)
	m.evaluateObserver(observer)
	return nil
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
// its leave-radius box currently covers.
func (m *AOI) resubscribe(observer *interestObserver) {
	reach := spatial.SaturatingAdd(m.config.LeaveRadius, 1)
	box := spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(observer.at.X, m.config.LeaveRadius), Y: spatial.SaturatingSub(observer.at.Y, m.config.LeaveRadius)},
		Max: spatial.Point{X: spatial.SaturatingAdd(observer.at.X, reach), Y: spatial.SaturatingAdd(observer.at.Y, reach)},
	}
	next, _ := m.index.BlockRects(box)
	for block := range observer.blocks {
		if _, keep := next[block]; !keep {
			m.unsubscribeBlock(observer, block)
		}
	}
	for block := range next {
		if _, subscribed := observer.blocks[block]; !subscribed {
			observer.blocks[block] = struct{}{}
			table := m.blockObservers[block]
			if table == nil {
				table = make(map[int64]*interestObserver)
				m.blockObservers[block] = table
			}
			table[observer.id] = observer
		}
	}
}

func (m *AOI) unsubscribeBlock(observer *interestObserver, block int64) {
	delete(observer.blocks, block)
	if table := m.blockObservers[block]; table != nil {
		delete(table, observer.id)
		if len(table) == 0 {
			delete(m.blockObservers, block)
		}
	}
}

// observersAt 合并旧、新格子的观察者，只排序一次。同格移动仍需评估距离变化。
func (m *AOI) observersAt(at spatial.Point, other ...spatial.Point) []*interestObserver {
	first := m.index.BlockIndex(at)
	table := m.blockObservers[first]
	result := make([]*interestObserver, 0, len(table))
	for _, observer := range table {
		result = append(result, observer)
	}
	if len(other) > 0 {
		second := m.index.BlockIndex(other[0])
		if second != first {
			for id, observer := range m.blockObservers[second] {
				if _, exists := table[id]; !exists {
					result = append(result, observer)
				}
			}
		}
	}
	slices.SortFunc(result, func(a, b *interestObserver) int { return cmp.Compare(a.id, b.id) })
	return result
}

// evaluateSubjectFor runs the visibility state machine for one subject
// against each candidate observer.
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
// members against the leave radius, subscription-neighborhood subjects
// against the enter radius.
func (m *AOI) evaluateObserver(observer *interestObserver) {
	for _, subject := range sortedVisible(observer.visible) {
		at, exists := m.subjects[subject]
		m.evaluatePair(observer, subject, at, !exists)
	}
	blocks := m.blocksScratch[:0]
	for block := range observer.blocks {
		blocks = append(blocks, block)
	}
	slices.Sort(blocks)
	// Admit new candidates nearest-first: block-order admission could let a
	// farther subject enter and be evicted by a nearer one within the same
	// evaluation, emitting a transient Enter+Leave pair downstream.
	candidates := m.candidatesScratch[:0]
	seen := m.seenScratch
	if seen == nil {
		seen = make(map[int64]struct{})
	}
	defer func() {
		if cap(m.queryScratch) > 4096 {
			m.queryScratch = nil
		}
		if cap(blocks) <= 4096 {
			m.blocksScratch = blocks[:0]
		} else {
			m.blocksScratch = nil
		}
		if cap(candidates) <= 4096 {
			m.candidatesScratch = candidates[:0]
		} else {
			m.candidatesScratch = nil
		}
		if len(seen) <= 4096 {
			clear(seen)
			m.seenScratch = seen
		} else {
			m.seenScratch = nil
		}
	}()
	for _, block := range blocks {
		m.queryScratch = m.index.AppendBlockIDs(m.queryScratch[:0], block)
		for _, subject := range m.queryScratch {
			if _, alreadyVisible := observer.visible[subject]; alreadyVisible {
				continue
			}
			if _, duplicate := seen[subject]; duplicate {
				continue
			}
			seen[subject] = struct{}{}
			if at, exists := m.subjects[subject]; exists {
				candidates = append(candidates, entryCandidate{subject: subject, at: at, distance: spatial.DistanceSquared(observer.at, at)})
			}
		}
	}
	slices.SortFunc(candidates, func(a, b entryCandidate) int {
		if order := cmp.Compare(a.distance, b.distance); order != 0 {
			return order
		}
		return cmp.Compare(a.subject, b.subject)
	})
	for _, candidate := range candidates {
		m.evaluatePair(observer, candidate.subject, candidate.at, false)
	}
}

func (m *AOI) evaluatePair(observer *interestObserver, subject int64, at spatial.Point, removed bool) {
	if observer.id == subject {
		return
	}
	previousBand, wasVisible := observer.visible[subject]
	if removed {
		if wasVisible {
			delete(observer.visible, subject)
			m.emit(observer.id, subject, InterestLeave, -1)
		}
		return
	}
	if wasVisible {
		if !spatial.WithinDistance(observer.at, at, m.config.LeaveRadius) {
			delete(observer.visible, subject)
			m.emit(observer.id, subject, InterestLeave, -1)
			return
		}
		if band := m.band(observer.at, at); band != previousBand {
			observer.visible[subject] = band
			m.emit(observer.id, subject, InterestBandChanged, band)
		}
		return
	}
	if !spatial.WithinDistance(observer.at, at, m.config.EnterRadius) {
		return
	}
	if m.config.MaxVisible > 0 && len(observer.visible) >= m.config.MaxVisible {
		if !m.evictFarther(observer, at) {
			return
		}
	}
	band := m.band(observer.at, at)
	observer.visible[subject] = band
	m.emit(observer.id, subject, InterestEnter, band)
}

// evictFarther makes room for a subject at the given distance by evicting
// the farthest current member if it is strictly farther (ties keep the
// incumbent; the farthest scan breaks distance ties by id for determinism).
func (m *AOI) evictFarther(observer *interestObserver, at spatial.Point) bool {
	farthest, farthestDistance := int64(-1), int64(-1)
	for subject := range observer.visible {
		position, exists := m.subjects[subject]
		if !exists {
			continue
		}
		distance := spatial.DistanceSquared(observer.at, position)
		if distance > farthestDistance || (distance == farthestDistance && subject < farthest) {
			farthest, farthestDistance = subject, distance
		}
	}
	if farthest < 0 || spatial.DistanceSquared(observer.at, at) >= farthestDistance {
		return false
	}
	delete(observer.visible, farthest)
	m.emit(observer.id, farthest, InterestLeave, -1)
	return true
}

func sortedVisible(visible map[int64]int) []int64 {
	result := make([]int64, 0, len(visible))
	for id := range visible {
		result = append(result, id)
	}
	slices.Sort(result)
	return result
}
