package policy

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"sort"
	"sync"
)

// Cluster errors.
var (
	ErrAreaOverlap   = errors.New("policy: room bounds overlap")
	ErrAreaUnknown   = errors.New("policy: point outside every room")
	ErrAreaDuplicate = errors.New("policy: duplicate room id")
)

// AreaID identifies one room (world-plane shard) inside a cluster. The id is
// semantic only — it deliberately does not encode process placement, so a
// future cross-process handover can reuse the same topology (that migration
// path additionally needs remote-entity ownership fences and cross-process
// subscription replication, and is out of scope here).
type AreaID int64

// AOICluster stitches per-room InterestManagers into one seamless
// interest space over a SHARED world coordinate system: rooms are
// non-overlapping rectangles of the same plane, adjacency follows from the
// geometry, and ids are cluster-global.
//
// Seamlessness rests on two mechanisms:
//
//   - Boundary mirroring: an observer whose leave-radius box crosses into a
//     neighboring room is transparently registered there too, so entities in
//     the neighbor's border strip are visible — room seams cast no shadow.
//   - Net-change flushing: per-room events update a per-pair room->band table
//     and Flush emits only the difference against the last emitted state. A
//     make-before-break migration (add to the destination room, then remove
//     from the source, inside one MoveSubject call) therefore produces zero
//     events when visibility is in fact unchanged — downstream subscriptions
//     never blink across a border crossing.
//
// Concurrency: rooms tick from their own scene handlers concurrently, so the
// cluster serializes all access behind one mutex (room ticks are 10–30Hz and
// operations are microsecond-scale; see the package benchmark before
// sharding this lock).
type AOICluster struct {
	mu     sync.Mutex
	config AOIConfig
	rooms  map[AreaID]*clusterArea
	// order keeps deterministic room iteration (ascending AreaID).
	order     []AreaID
	subjects  map[int64]clusterPlacement
	observers map[int64]clusterPlacement
	// visible tracks, per (observer, subject) pair, the band contributed by
	// each room and the state last emitted downstream.
	visible map[interestPair]*clusterVisibility
	// observerPairs indexes visible by observer for capacity settlement.
	observerPairs map[int64]map[int64]*clusterVisibility
	pending       []InterestEvent
}

type clusterArea struct {
	id      AreaID
	bounds  spatial.Rect
	manager *AOI
}

type clusterPlacement struct {
	at spatial.Point
	// home is the room containing the position; observers are additionally
	// mirrored into every room their leave radius touches.
	home AreaID
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
	// visible to some room manager, but not surfaced downstream.
	suppressed bool
}

func NewAOICluster(config AOIConfig) (*AOICluster, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.BlockSize <= 0 {
		return nil, spatial.ErrInvalidBounds
	}
	return &AOICluster{
		config:        config,
		rooms:         make(map[AreaID]*clusterArea),
		subjects:      make(map[int64]clusterPlacement),
		observers:     make(map[int64]clusterPlacement),
		visible:       make(map[interestPair]*clusterVisibility),
		observerPairs: make(map[int64]map[int64]*clusterVisibility),
	}, nil
}

// AddArea registers a world-plane rectangle as a room. Bounds must not
// overlap any existing room. Adding a room after observers exist retrofits
// their boundary mirrors immediately — without that, a stationary observer
// near the new seam would have an unbounded visibility hole into the new
// room (its mirrors are otherwise only rebuilt when it moves).
func (c *AOICluster) AddArea(id AreaID, bounds spatial.Rect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.rooms[id]; exists {
		return ErrAreaDuplicate
	}
	bounds = spatial.NormalizeRect(bounds)
	for _, room := range c.rooms {
		if rectsOverlap(room.bounds, bounds) {
			return fmt.Errorf("%w: %d and %d", ErrAreaOverlap, room.id, id)
		}
	}
	roomConfig := c.config
	roomConfig.Bounds = bounds
	// Rooms run uncapped: a border observer is mirrored into several rooms,
	// and per-room caps would multiply its budget by the number of touched
	// rooms exactly where entities are densest. The cluster enforces
	// MaxVisible globally in collect instead.
	roomConfig.MaxVisible = 0
	manager, err := NewAOI(roomConfig)
	if err != nil {
		return err
	}
	c.rooms[id] = &clusterArea{id: id, bounds: bounds, manager: manager}
	c.order = append(c.order, id)
	sort.Slice(c.order, func(i, j int) bool { return c.order[i] < c.order[j] })
	observers := make([]int64, 0, len(c.observers))
	for observer := range c.observers {
		observers = append(observers, observer)
	}
	sort.Slice(observers, func(i, j int) bool { return observers[i] < observers[j] })
	for _, observer := range observers {
		if err := c.placeObserverLocked(observer, c.observers[observer].at); err != nil {
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
		if room := c.rooms[id]; room.bounds.Contains(at) {
			return room, true
		}
	}
	return nil, false
}

// AddSubject indexes a subject in the room containing its position.
func (c *AOICluster) AddSubject(id int64, at spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.subjects[id]; exists {
		return c.moveSubjectLocked(id, at)
	}
	room, ok := c.areaAt(at)
	if !ok {
		return ErrAreaUnknown
	}
	if err := room.manager.AddSubject(id, at); err != nil {
		return err
	}
	c.subjects[id] = clusterPlacement{at: at, home: room.id}
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
		if err := c.rooms[placement.home].manager.RemoveSubject(id); err != nil {
			return err
		}
	}
	c.subjects[id] = clusterPlacement{at: to, home: destination.id}
	c.collect()
	return nil
}

// RemoveSubject drops a subject from its room.
func (c *AOICluster) RemoveSubject(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	placement, exists := c.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	if err := c.rooms[placement.home].manager.RemoveSubject(id); err != nil {
		return err
	}
	delete(c.subjects, id)
	c.collect()
	return nil
}

// AddObserver registers an observer in its home room and mirrors it into
// every neighboring room its leave radius touches.
func (c *AOICluster) AddObserver(id int64, at spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.placeObserverLocked(id, at)
}

// MoveObserver relocates an observer, rebuilding its boundary mirrors.
func (c *AOICluster) MoveObserver(id int64, to spatial.Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.observers[id]; !exists {
		return ErrInterestUnknown
	}
	return c.placeObserverLocked(id, to)
}

func (c *AOICluster) placeObserverLocked(id int64, at spatial.Point) error {
	home, ok := c.areaAt(at)
	if !ok {
		return ErrAreaUnknown
	}
	span := spatial.SaturatingAdd(c.config.LeaveRadius, 1)
	reach := spatial.Rect{
		Min: spatial.Point{X: spatial.SaturatingSub(at.X, c.config.LeaveRadius), Y: spatial.SaturatingSub(at.Y, c.config.LeaveRadius)},
		Max: spatial.Point{X: spatial.SaturatingAdd(at.X, span), Y: spatial.SaturatingAdd(at.Y, span)},
	}
	for _, roomID := range c.order {
		room := c.rooms[roomID]
		inReach := rectsOverlap(room.bounds, reach)
		_, registered := room.manager.observers[id]
		switch {
		case inReach && registered:
			// The manager clamps evaluation to its own bounds; the mirror
			// keeps the observer's true position even when outside them.
			if err := room.manager.moveObserverUnbounded(id, at); err != nil {
				return err
			}
		case inReach && !registered:
			if err := room.manager.addObserverUnbounded(id, at); err != nil {
				return err
			}
		case !inReach && registered:
			if err := room.manager.RemoveObserver(id); err != nil {
				return err
			}
		}
	}
	c.observers[id] = clusterPlacement{at: at, home: home.id}
	c.collect()
	return nil
}

// RemoveObserver drops an observer and all its mirrors.
func (c *AOICluster) RemoveObserver(id int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.observers[id]; !exists {
		return ErrInterestUnknown
	}
	for _, roomID := range c.order {
		room := c.rooms[roomID]
		if _, registered := room.manager.observers[id]; registered {
			if err := room.manager.RemoveObserver(id); err != nil {
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
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
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
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Observer != events[j].Observer {
			return events[i].Observer < events[j].Observer
		}
		return events[i].Subject < events[j].Subject
	})
	return events
}

// collect folds the rooms' raw events into per-pair room/band tables, then
// settles each touched observer: cluster-level MaxVisible (nearest-N across
// ALL rooms — per-room caps would multiply a border observer's budget) picks
// the surfaced set, and only net changes against the previously emitted
// state are appended to pending.
func (c *AOICluster) collect() {
	touchedObservers := make(map[int64]struct{})
	for _, roomID := range c.order {
		room := c.rooms[roomID]
		for _, event := range room.manager.Flush() {
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
				state.bands[roomID] = event.Band
			case InterestLeave:
				delete(state.bands, roomID)
			}
			touchedObservers[pair.observer] = struct{}{}
		}
	}
	observers := make([]int64, 0, len(touchedObservers))
	for observer := range touchedObservers {
		observers = append(observers, observer)
	}
	sort.Slice(observers, func(i, j int) bool { return observers[i] < observers[j] })
	for _, observer := range observers {
		c.settleObserverLocked(observer)
	}
}

// settleObserverLocked reconciles one observer's emitted set with its
// room-visible set under the cluster-level MaxVisible cap.
func (c *AOICluster) settleObserverLocked(observer int64) {
	pairs := c.observerPairs[observer]
	subjects := make([]int64, 0, len(pairs))
	for subject := range pairs {
		subjects = append(subjects, subject)
	}
	sort.Slice(subjects, func(i, j int) bool { return subjects[i] < subjects[j] })
	// Capacity: keep the nearest MaxVisible of the room-visible subjects
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
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].distance != candidates[j].distance {
				return candidates[i].distance < candidates[j].distance
			}
			return candidates[i].subject < candidates[j].subject
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

// effectiveBand is the closest (smallest) band any room reports.
func effectiveBand(bands map[AreaID]int) int {
	best := -1
	for _, band := range bands {
		if best < 0 || band < best {
			best = band
		}
	}
	return best
}
