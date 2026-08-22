package spatial

import (
	"errors"
	"sync"
)

var (
	ErrInvalidBounds = errors.New("spatial: invalid terrain bounds")
	ErrInvalidPoint  = errors.New("spatial: point is outside terrain bounds")
	ErrBlocked       = errors.New("spatial: point is blocked")
	ErrTooManyBlocks = errors.New("spatial: block index exceeds production limit")
)

// GridTerrain is a concurrent sparse obstacle grid with immutable bounds.
type GridTerrain struct {
	width, height int64
	mu            sync.RWMutex
	obstacles     map[Point]struct{}
}

func NewGridTerrain(width, height int64) (*GridTerrain, error) {
	if width <= 0 || height <= 0 {
		return nil, ErrInvalidBounds
	}
	return &GridTerrain{width: width, height: height, obstacles: make(map[Point]struct{})}, nil
}

func (t *GridTerrain) Bounds() Rect {
	if t == nil {
		return Rect{}
	}
	return Rect{Max: Point{X: t.width, Y: t.height}}
}

func (t *GridTerrain) InBounds(point Point) bool {
	return t != nil && point.X >= 0 && point.Y >= 0 && point.X < t.width && point.Y < t.height
}

func (t *GridTerrain) Blocked(point Point) bool {
	if !t.InBounds(point) {
		return true
	}
	t.mu.RLock()
	_, blocked := t.obstacles[point]
	t.mu.RUnlock()
	return blocked
}

func (t *GridTerrain) SetBlocked(point Point, blocked bool) error {
	if !t.InBounds(point) {
		return ErrInvalidPoint
	}
	t.mu.Lock()
	if blocked {
		t.obstacles[point] = struct{}{}
	} else {
		delete(t.obstacles, point)
	}
	t.mu.Unlock()
	return nil
}

func (t *GridTerrain) TryBlock(point Point) error {
	if !t.InBounds(point) {
		return ErrInvalidPoint
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.obstacles[point]; exists {
		return ErrBlocked
	}
	t.obstacles[point] = struct{}{}
	return nil
}

func (t *GridTerrain) TryMoveBlock(oldPoint, newPoint Point) error {
	if !t.InBounds(oldPoint) || !t.InBounds(newPoint) {
		return ErrInvalidPoint
	}
	if oldPoint == newPoint {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.obstacles[newPoint]; exists {
		return ErrBlocked
	}
	delete(t.obstacles, oldPoint)
	t.obstacles[newPoint] = struct{}{}
	return nil
}

func (t *GridTerrain) RectBlocked(rect Rect) bool {
	if t == nil {
		return true
	}
	rect = NormalizeRect(rect)
	if rect.Empty() || !t.InBounds(rect.Min) || !t.InBounds(Point{X: rect.Max.X - 1, Y: rect.Max.Y - 1}) {
		return true
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for point := range t.obstacles {
		if rect.Contains(point) {
			return true
		}
	}
	return false
}

func (t *GridTerrain) Clear() {
	if t == nil {
		return
	}
	t.mu.Lock()
	clear(t.obstacles)
	t.mu.Unlock()
}
