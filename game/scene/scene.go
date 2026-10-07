package scene

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/spatial"
)

// Package scene is the contract between the Scene Entity and the systems that
// make it up. Both sides import this package and neither imports the other,
// which is what keeps the Entity and its runtime in separate packages without
// an import cycle.
//
// Every system here is declared as an interface for two reasons. The Scene
// Entity hands these out to whoever asks (`scene.Terrain()`), so callers
// depend on what a system promises rather than on how it is built; and each
// implementation is free to carry its own lock, which is the rule for this
// runtime: **a system is safe to call from any goroutine**. The Entity's own
// lock orders transactions over entity STATE; it says nothing about a
// neighbour query arriving from a timer, so the systems do not lean on it.

// Config shapes a scene. Width and Height are the map in world units,
// BlockSize is the AOI grid's cell, and Spawn is where a player with no
// remembered position starts.
type Config struct {
	Width     int64
	Height    int64
	BlockSize int64
	Spawn     spatial.Point
}

// DefaultConfig is the demo's one map: a 1000×1000 field on a 100-unit grid.
func DefaultConfig() Config {
	return Config{Width: 1000, Height: 1000, BlockSize: 100, Spawn: spatial.Point{X: 500, Y: 500}}
}

func (c Config) Normalize() Config {
	if c.Width <= 0 {
		c.Width = DefaultConfig().Width
	}
	if c.Height <= 0 {
		c.Height = DefaultConfig().Height
	}
	if c.BlockSize <= 0 {
		c.BlockSize = DefaultConfig().BlockSize
	}
	return c
}

// Terrain is the map itself: where the edges are, what can be stood on, and
// who is standing where in the sense of "this point is taken".
//
// Occupancy is here rather than in the AOI index because it is a property of
// the GROUND, not of who is looking: two objects cannot share a point whether
// or not anybody can see them.
type Terrain interface {
	Bounds() spatial.Rect
	// Walkable reports whether a point is inside the map and not blocked.
	Walkable(point spatial.Point) bool
	// Occupy takes a point for an object, failing if it is out of bounds or
	// already taken. Release gives it back.
	Occupy(point spatial.Point) error
	Release(point spatial.Point)
	// MoveOccupied is Occupy(to) + Release(from) as one step, so a failed
	// move cannot lose the object's own point.
	MoveOccupied(from, to spatial.Point) error
}

// PathFind answers "can I get there" and "where can I stand".
type PathFind interface {
	// Path is the walkable route between two points, start and goal
	// included. It fails when no route exists within the search budget.
	Path(from, to spatial.Point) ([]spatial.Point, error)
	// Place finds somewhere to stand at or near preferred: the spawn point
	// of a player whose remembered position is now blocked, the landing spot
	// of a respawned monster. It searches outward in rings, so the result is
	// the nearest free point, not merely a free one.
	Place(preferred spatial.Point) (spatial.Point, error)
}

// Entity is what the runtime needs from the Scene that owns it. It is
// deliberately tiny: a system that wants more of the Entity is a system that
// is about to create an import cycle.
type Entity interface {
	ID() int64
}

// Who receives whose state is not part of this contract any more: the
// area-of-interest policy lives in roost-core (`entitysync/policy.Interest`)
// and is assembled by the replication bridge, which is also the only place
// that knows about sessions. The scene runtime is the MAP — where things are
// and where they may stand — and the policy asks it nothing; it keeps its own
// spatial index from the same Config.

// --- 刷新：场景该有多少东西活着 ---

// SpawnRequest is one thing the scene wants created. The refresh system does
// not create it: building an Entity needs the lifecycle, the lifecycle needs
// the entity packages, and the entity packages hold this runtime — so the
// system says what it wants and the assembly does it.
type SpawnRequest struct {
	// Group is the spawn table row this came from; the assembly hands it back
	// with Spawned so the system can count the population per row.
	Group    int32
	Template int32
	HP       int64
	At       spatial.Point
}

// Refresh keeps the scene's population at what the spawn table says.
type Refresh interface {
	// Due returns what should be created now: one request per missing
	// member whose respawn delay has elapsed. Calling it twice without
	// Spawned in between returns the same requests again — the system counts
	// what it has been TOLD exists, not what it has asked for, because a
	// spawn that failed must be asked for again.
	Due(now time.Time) []SpawnRequest
	// Spawned reports a created member. Until it is called the member does
	// not count towards the group.
	Spawned(group int32, id int64)
	// Died reports a member gone and queues its slot for the group's respawn
	// delay.
	Died(id int64, now time.Time)
	// Alive lists every member the system believes exists, for the assembly
	// to clean up on shutdown.
	Alive() []int64
}
