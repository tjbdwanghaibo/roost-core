package spatial

import (
	"container/heap"
	"errors"
)

var ErrNoPath = errors.New("spatial: no path")

type Terrain interface {
	InBounds(Point) bool
	Blocked(Point) bool
}

type PathOptions struct{ MaxVisited int }

// FindPath uses four-direction A* and includes both endpoints.
func FindPath(terrain Terrain, start, goal Point, options PathOptions) ([]Point, error) {
	if terrain == nil || !terrain.InBounds(start) || !terrain.InBounds(goal) {
		return nil, ErrInvalidPoint
	}
	if terrain.Blocked(start) || terrain.Blocked(goal) {
		return nil, ErrNoPath
	}
	if start == goal {
		return []Point{start}, nil
	}
	if options.MaxVisited <= 0 {
		options.MaxVisited = 10000
	}
	open := &pointQueue{}
	heap.Push(open, &pathNode{point: start, score: manhattan(start, goal)})
	cameFrom := make(map[Point]Point)
	cost := map[Point]int64{start: 0}
	for visited := 0; open.Len() > 0; {
		visited++
		if visited > options.MaxVisited {
			return nil, ErrNoPath
		}
		current := heap.Pop(open).(*pathNode).point
		if current == goal {
			return rebuildPath(cameFrom, start, goal), nil
		}
		for _, next := range adjacentPoints(current) {
			if !terrain.InBounds(next) || terrain.Blocked(next) {
				continue
			}
			nextCost := cost[current] + 1
			oldCost, exists := cost[next]
			if exists && nextCost >= oldCost {
				continue
			}
			cost[next] = nextCost
			cameFrom[next] = current
			heap.Push(open, &pathNode{point: next, score: nextCost + manhattan(next, goal)})
		}
	}
	return nil, ErrNoPath
}

func adjacentPoints(point Point) [4]Point {
	return [4]Point{{X: point.X + 1, Y: point.Y}, {X: point.X - 1, Y: point.Y}, {X: point.X, Y: point.Y + 1}, {X: point.X, Y: point.Y - 1}}
}

func manhattan(a, b Point) int64 {
	dx, dy := a.X-b.X, a.Y-b.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return dx + dy
}

func rebuildPath(cameFrom map[Point]Point, start, goal Point) []Point {
	path := []Point{goal}
	for current := goal; current != start; {
		current = cameFrom[current]
		path = append(path, current)
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

type pathNode struct {
	point Point
	score int64
	index int
}
type pointQueue []*pathNode

func (q pointQueue) Len() int           { return len(q) }
func (q pointQueue) Less(i, j int) bool { return q[i].score < q[j].score }
func (q pointQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *pointQueue) Push(value any) {
	node := value.(*pathNode)
	node.index = len(*q)
	*q = append(*q, node)
}
func (q *pointQueue) Pop() any {
	old := *q
	node := old[len(old)-1]
	*q = old[:len(old)-1]
	return node
}
