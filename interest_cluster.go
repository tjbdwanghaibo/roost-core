package spatial

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Cluster errors.
var (
	ErrRoomOverlap   = errors.New("spatial: room bounds overlap")
	ErrRoomUnknown   = errors.New("spatial: point outside every room")
	ErrRoomDuplicate = errors.New("spatial: duplicate room id")
)

// RoomID identifies one room (world-plane shard) inside a cluster. The id is
// semantic only — it deliberately does not encode process placement, so a
// future cross-process handover can reuse the same topology (that migration
// path additionally needs remote-entity ownership fences and cross-process
// subscription replication, and is out of scope here).
type RoomID int64

// InterestCluster stitches per-room InterestManagers into one seamless
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
type InterestCluster struct {
	mu     sync.Mutex
	config InterestConfig
	rooms  map[RoomID]*clusterRoom
	// order keeps deterministic room iteration (ascending RoomID).
	order     []RoomID
	subjects  map[int64]clusterPlacement
	observers map[int64]clusterPlacement
	// visible tracks, per (observer, subject) pair, the band contributed by
	// each room and the state last emitted downstream.
	visible map[interestPair]*clusterVisibility
	pending []InterestEvent
}

type clusterRoom struct {
	id      RoomID
	bounds  Rect
	manager *InterestManager
}

type clusterPlacement struct {
	at Point
	// home is the room containing the position; observers are additionally
	// mirrored into every room their leave radius touches.
	home RoomID
}

type interestPair struct {
	observer int64
	subject  int64
}

type clusterVisibility struct {
	bands        map[RoomID]int
	emitted      bool
	emittedBand  int
}

func NewInterestCluster(config InterestConfig) (*InterestCluster, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if config.BlockSize <= 0 {
		return nil, ErrInvalidBounds
	}
	return &InterestCluster{
		config:    config,
		rooms:     make(map[RoomID]*clusterRoom),
		subjects:  make(map[int64]clusterPlacement),
		observers: make(map[int64]clusterPlacement),
		visible:   make(map[interestPair]*clusterVisibility),
	}, nil
}

// AddRoom registers a world-plane rectangle as a room. Bounds must not
// overlap any existing room. Rooms are expected to be added before entities;
// adding rooms later is allowed but existing boundary mirrors are not
// retrofitted until the next observer movement.
func (c *InterestCluster) AddRoom(id RoomID, bounds Rect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.rooms[id]; exists {
		return ErrRoomDuplicate
	}
	bounds = NormalizeRect(bounds)
	for _, room := range c.rooms {
		if rectsOverlap(room.bounds, bounds) {
			return fmt.Errorf("%w: %d and %d", ErrRoomOverlap, room.id, id)
		}
	}
	roomConfig := c.config
	roomConfig.Bounds = bounds
	manager, err := NewInterestManager(roomConfig)
	if err != nil {
		return err
	}
	c.rooms[id] = &clusterRoom{id: id, bounds: bounds, manager: manager}
	c.order = append(c.order, id)
	sort.Slice(c.order, func(i, j int) bool { return c.order[i] < c.order[j] })
	return nil
}

func rectsOverlap(a, b Rect) bool {
	return a.Min.X < b.Max.X && b.Min.X < a.Max.X && a.Min.Y < b.Max.Y && b.Min.Y < a.Max.Y
}

func (c *InterestCluster) roomAt(at Point) (*clusterRoom, bool) {
	for _, id := range c.order {
		if room := c.rooms[id]; room.bounds.Contains(at) {
			return room, true
		}
	}
	return nil, false
}

// AddSubject indexes a subject in the room containing its position.
func (c *InterestCluster) AddSubject(id int64, at Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.subjects[id]; exists {
		return c.moveSubjectLocked(id, at)
	}
	room, ok := c.roomAt(at)
	if !ok {
		return ErrRoomUnknown
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
func (c *InterestCluster) MoveSubject(id int64, to Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moveSubjectLocked(id, to)
}

func (c *InterestCluster) moveSubjectLocked(id int64, to Point) error {
	placement, exists := c.subjects[id]
	if !exists {
		return ErrInterestUnknown
	}
	destination, ok := c.roomAt(to)
	if !ok {
		return ErrRoomUnknown
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
func (c *InterestCluster) RemoveSubject(id int64) error {
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
func (c *InterestCluster) AddObserver(id int64, at Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.placeObserverLocked(id, at)
}

// MoveObserver relocates an observer, rebuilding its boundary mirrors.
func (c *InterestCluster) MoveObserver(id int64, to Point) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.observers[id]; !exists {
		return ErrInterestUnknown
	}
	return c.placeObserverLocked(id, to)
}

func (c *InterestCluster) placeObserverLocked(id int64, at Point) error {
	home, ok := c.roomAt(at)
	if !ok {
		return ErrRoomUnknown
	}
	reach := Rect{
		Min: Point{X: saturatingSub(at.X, c.config.LeaveRadius), Y: saturatingSub(at.Y, c.config.LeaveRadius)},
		Max: Point{X: saturatingAdd(at.X, c.config.LeaveRadius+1), Y: saturatingAdd(at.Y, c.config.LeaveRadius+1)},
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
func (c *InterestCluster) RemoveObserver(id int64) error {
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
func (c *InterestCluster) Visible(observer int64) []int64 {
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
func (c *InterestCluster) Flush() []InterestEvent {
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

// collect folds the rooms' raw events into per-pair room/band tables and
// emits only net changes against the previously emitted state.
func (c *InterestCluster) collect() {
	touched := make(map[interestPair]struct{})
	for _, roomID := range c.order {
		room := c.rooms[roomID]
		for _, event := range room.manager.Flush() {
			pair := interestPair{observer: event.Observer, subject: event.Subject}
			state := c.visible[pair]
			if state == nil {
				state = &clusterVisibility{bands: make(map[RoomID]int)}
				c.visible[pair] = state
			}
			switch event.Kind {
			case InterestEnter, InterestBandChanged:
				state.bands[roomID] = event.Band
			case InterestLeave:
				delete(state.bands, roomID)
			}
			touched[pair] = struct{}{}
		}
	}
	pairs := make([]interestPair, 0, len(touched))
	for pair := range touched {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].observer != pairs[j].observer {
			return pairs[i].observer < pairs[j].observer
		}
		return pairs[i].subject < pairs[j].subject
	})
	for _, pair := range pairs {
		state := c.visible[pair]
		nowVisible := len(state.bands) > 0
		band := 0
		if nowVisible {
			band = effectiveBand(state.bands)
		}
		switch {
		case nowVisible && !state.emitted:
			state.emitted, state.emittedBand = true, band
			c.pending = append(c.pending, InterestEvent{Observer: pair.observer, Subject: pair.subject, Kind: InterestEnter, Band: band})
		case !nowVisible && state.emitted:
			delete(c.visible, pair)
			c.pending = append(c.pending, InterestEvent{Observer: pair.observer, Subject: pair.subject, Kind: InterestLeave, Band: -1})
		case nowVisible && state.emitted && band != state.emittedBand:
			state.emittedBand = band
			c.pending = append(c.pending, InterestEvent{Observer: pair.observer, Subject: pair.subject, Kind: InterestBandChanged, Band: band})
		case !nowVisible && !state.emitted:
			delete(c.visible, pair)
		}
	}
}

// effectiveBand is the closest (smallest) band any room reports.
func effectiveBand(bands map[RoomID]int) int {
	best := -1
	for _, band := range bands {
		if best < 0 || band < best {
			best = band
		}
	}
	return best
}
