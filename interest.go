package spatial

import (
	"errors"
	"sort"
)

// Interest management errors.
var (
	ErrInterestConfig  = errors.New("spatial: invalid interest config")
	ErrInterestUnknown = errors.New("spatial: unknown interest id")
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

// InterestConfig shapes an InterestManager.
type InterestConfig struct {
	Bounds    Rect
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
	// MaxVisible, when positive, caps an observer's visible set: an entering
	// subject closer than the current farthest evicts it, a farther one is
	// ignored. This is a broadcast-storm gate with approximate semantics —
	// an ignored subject re-qualifies on its next movement, not the moment
	// capacity frees up.
	MaxVisible int
}

func (c InterestConfig) validate() error {
	if c.EnterRadius <= 0 || c.LeaveRadius < c.EnterRadius {
		return ErrInterestConfig
	}
	for index, edge := range c.Bands {
		if edge <= 0 || index > 0 && edge <= c.Bands[index-1] {
			return ErrInterestConfig
		}
	}
	return nil
}

type interestObserver struct {
	id      int64
	at      Point
	blocks  map[int64]struct{}
	visible map[int64]int // subject id -> current band
}

// InterestManager is the incremental interest (AOI) layer over BlockIndex:
// observers subscribe to the blocks their leave radius covers, subject and
// observer movement re-evaluates only the affected neighborhood, and Flush
// drains the resulting Enter/Leave/BandChanged events deterministically.
//
// Concurrency: unlike BlockIndex, an InterestManager is NOT safe for
// concurrent use. It is scene-private state, owned and ticked serially by
// the scene's handler; paying for locks here would buy nothing. For multiple
// concurrently ticking rooms use InterestCluster, which adds its own lock.
type InterestManager struct {
	config    InterestConfig
	index     *BlockIndex
	subjects  map[int64]Point
	observers map[int64]*interestObserver
	// blockObservers is the nine-grid subscription table: block index ->
	// observers whose leave-radius box covers it.
	blockObservers map[int64]map[int64]*interestObserver
	pending        []InterestEvent
}

func NewInterestManager(config InterestConfig) (*InterestManager, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	index, err := NewBlockIndex(config.Bounds, config.BlockSize)
	if err != nil {
		return nil, err
	}
	return &InterestManager{
		config:         config,
		index:          index,
		subjects:       make(map[int64]Point),
		observers:      make(map[int64]*interestObserver),
		blockObservers: make(map[int64]map[int64]*interestObserver),
	}, nil
}

// Bounds returns the managed area.
func (m *InterestManager) Bounds() Rect { return m.config.Bounds }

func (m *InterestManager) band(distanceFrom, to Point) int {
	for index, edge := range m.config.Bands {
		if WithinDistance(distanceFrom, to, edge) {
			return index
		}
	}
	return len(m.config.Bands)
}

func (m *InterestManager) emit(observer, subject int64, kind InterestEventKind, band int) {
	m.pending = append(m.pending, InterestEvent{Observer: observer, Subject: subject, Kind: kind, Band: band})
}

// AddSubject indexes a subject and evaluates it against the neighborhood's
// observers.
func (m *InterestManager) AddSubject(id int64, at Point) error {
	if !m.config.Bounds.Contains(at) {
		return ErrInvalidBounds
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
func (m *InterestManager) MoveSubject(id int64, to Point) error {
	from, exists := m.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(to) {
		return ErrInvalidBounds
	}
	m.index.Move(id, from, to)
	m.subjects[id] = to
	affected := m.observersAt(from)
	affected = m.observersAt(to, affected...)
	m.evaluateSubjectFor(affected, id, to, false)
	return nil
}

// RemoveSubject drops a subject, emitting Leave to every observer that saw it.
func (m *InterestManager) RemoveSubject(id int64) error {
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
func (m *InterestManager) AddObserver(id int64, at Point) error {
	if !m.config.Bounds.Contains(at) {
		return ErrInvalidBounds
	}
	return m.addObserverUnbounded(id, at)
}

// addObserverUnbounded registers an observer whose position may lie outside
// this manager's bounds: InterestCluster mirrors border observers into
// neighboring rooms at their true world position, and block subscription
// clamps to this room's bounds on its own.
func (m *InterestManager) addObserverUnbounded(id int64, at Point) error {
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
func (m *InterestManager) MoveObserver(id int64, to Point) error {
	if _, exists := m.observers[id]; !exists {
		return ErrInterestUnknown
	}
	if !m.config.Bounds.Contains(to) {
		return ErrInvalidBounds
	}
	return m.moveObserverUnbounded(id, to)
}

func (m *InterestManager) moveObserverUnbounded(id int64, to Point) error {
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
func (m *InterestManager) RemoveObserver(id int64) error {
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
func (m *InterestManager) Visible(observer int64) []int64 {
	registered, exists := m.observers[observer]
	if !exists {
		return nil
	}
	return sortedVisible(registered.visible)
}

// Flush drains the accumulated events. Events are ordered by (Observer,
// Subject) with same-pair events keeping their occurrence order, so the
// output is a deterministic function of the operation sequence.
func (m *InterestManager) Flush() []InterestEvent {
	if len(m.pending) == 0 {
		return nil
	}
	events := m.pending
	m.pending = nil
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Observer != events[j].Observer {
			return events[i].Observer < events[j].Observer
		}
		return events[i].Subject < events[j].Subject
	})
	return events
}

// resubscribe diffs the observer's block subscriptions against the blocks
// its leave-radius box currently covers.
func (m *InterestManager) resubscribe(observer *interestObserver) {
	box := Rect{
		Min: Point{X: saturatingSub(observer.at.X, m.config.LeaveRadius), Y: saturatingSub(observer.at.Y, m.config.LeaveRadius)},
		Max: Point{X: saturatingAdd(observer.at.X, m.config.LeaveRadius+1), Y: saturatingAdd(observer.at.Y, m.config.LeaveRadius+1)},
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

func (m *InterestManager) unsubscribeBlock(observer *interestObserver, block int64) {
	delete(observer.blocks, block)
	if table := m.blockObservers[block]; table != nil {
		delete(table, observer.id)
		if len(table) == 0 {
			delete(m.blockObservers, block)
		}
	}
}

// observersAt collects, in ascending id order, the observers subscribed to
// the block neighborhood around a point (accumulating into a previous result
// when extending across two neighborhoods).
func (m *InterestManager) observersAt(at Point, previous ...*interestObserver) []*interestObserver {
	seen := make(map[int64]struct{}, len(previous))
	result := previous
	for _, observer := range previous {
		seen[observer.id] = struct{}{}
	}
	if table := m.blockObservers[m.index.BlockIndex(at)]; table != nil {
		for _, observer := range table {
			if _, duplicate := seen[observer.id]; duplicate {
				continue
			}
			seen[observer.id] = struct{}{}
			result = append(result, observer)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result
}

// evaluateSubjectFor runs the visibility state machine for one subject
// against each candidate observer.
func (m *InterestManager) evaluateSubjectFor(observers []*interestObserver, subject int64, at Point, removed bool) {
	for _, observer := range observers {
		m.evaluatePair(observer, subject, at, removed)
	}
}

// evaluateObserver re-evaluates an observer's entire visible set: current
// members against the leave radius, subscription-neighborhood subjects
// against the enter radius.
func (m *InterestManager) evaluateObserver(observer *interestObserver) {
	for _, subject := range sortedVisible(observer.visible) {
		at, exists := m.subjects[subject]
		m.evaluatePair(observer, subject, at, !exists)
	}
	blocks := make([]int64, 0, len(observer.blocks))
	for block := range observer.blocks {
		blocks = append(blocks, block)
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
	for _, block := range blocks {
		for _, subject := range m.index.QueryBlockIndex(block) {
			if _, alreadyVisible := observer.visible[subject]; alreadyVisible {
				continue
			}
			if at, exists := m.subjects[subject]; exists {
				m.evaluatePair(observer, subject, at, false)
			}
		}
	}
}

func (m *InterestManager) evaluatePair(observer *interestObserver, subject int64, at Point, removed bool) {
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
		if !WithinDistance(observer.at, at, m.config.LeaveRadius) {
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
	if !WithinDistance(observer.at, at, m.config.EnterRadius) {
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
func (m *InterestManager) evictFarther(observer *interestObserver, at Point) bool {
	farthest, farthestDistance := int64(-1), int64(-1)
	for _, subject := range sortedVisible(observer.visible) {
		position, exists := m.subjects[subject]
		if !exists {
			continue
		}
		distance := DistanceSquared(observer.at, position)
		if distance > farthestDistance {
			farthest, farthestDistance = subject, distance
		}
	}
	if farthest < 0 || DistanceSquared(observer.at, at) >= farthestDistance {
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
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
