package spatial

import (
	"errors"
	"math"
	"sync"
	"testing"
)

func TestGridTerrainAtomicMovesAndBounds(t *testing.T) {
	terrain, err := NewGridTerrain(8, 8)
	if err != nil {
		t.Fatal(err)
	}
	_ = terrain.TryBlock(Point{X: 1, Y: 1})
	_ = terrain.TryBlock(Point{X: 2, Y: 1})
	if err := terrain.TryMoveBlock(Point{X: 1, Y: 1}, Point{X: 2, Y: 1}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("TryMoveBlock error = %v", err)
	}
	if !terrain.Blocked(Point{X: 1, Y: 1}) {
		t.Fatal("failed move must retain old obstacle")
	}
	if !terrain.RectBlocked(Rect{Min: Point{X: 1, Y: 1}, Max: Point{X: 3, Y: 2}}) {
		t.Fatal("RectBlocked missed obstacle")
	}
}

func TestDistanceOperationsDoNotOverflow(t *testing.T) {
	min := Point{X: math.MinInt64, Y: math.MinInt64}
	max := Point{X: math.MaxInt64, Y: math.MaxInt64}
	if got := DistanceSquared(min, max); got != math.MaxInt64 {
		t.Fatalf("DistanceSquared = %d, want saturation", got)
	}
	if WithinDistance(min, max, math.MaxInt64) {
		t.Fatal("diagonal extreme points must be outside MaxInt64 radius")
	}
	if !WithinDistance(Point{X: -3, Y: 4}, Point{}, 5) {
		t.Fatal("3-4-5 distance should be within radius")
	}
}

func TestGridTerrainConcurrentAccess(t *testing.T) {
	terrain, _ := NewGridTerrain(32, 32)
	var group sync.WaitGroup
	for worker := int64(0); worker < 8; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for index := int64(0); index < 1000; index++ {
				point := Point{X: (worker + index) % 32, Y: (worker*3 + index) % 32}
				_ = terrain.SetBlocked(point, index%2 == 0)
				_ = terrain.Blocked(point)
			}
		}()
	}
	group.Wait()
}

func TestFindPath(t *testing.T) {
	terrain, _ := NewGridTerrain(4, 4)
	_ = terrain.SetBlocked(Point{X: 1, Y: 0}, true)
	_ = terrain.SetBlocked(Point{X: 1, Y: 1}, true)
	path, err := FindPath(terrain, Point{}, Point{X: 3, Y: 0}, PathOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(path) == 0 || path[0] != (Point{}) || path[len(path)-1] != (Point{X: 3, Y: 0}) {
		t.Fatalf("path = %+v", path)
	}
}

func TestFindPathDistinguishesBudgetFromNoPath(t *testing.T) {
	// A caller reacts differently to "provably unreachable" (reject the
	// move) and "search budget exhausted" (retry with a larger budget), so
	// the two outcomes must be distinct errors.
	terrain, _ := NewGridTerrain(8, 8)
	for y := int64(0); y < 8; y++ {
		_ = terrain.SetBlocked(Point{X: 4, Y: y}, true) // full wall: unreachable
	}
	if _, err := FindPath(terrain, Point{}, Point{X: 7, Y: 0}, PathOptions{}); !errors.Is(err, ErrNoPath) {
		t.Fatalf("walled goal error = %v, want ErrNoPath", err)
	}

	open, _ := NewGridTerrain(64, 64)
	if _, err := FindPath(open, Point{}, Point{X: 63, Y: 63}, PathOptions{MaxVisited: 3}); !errors.Is(err, ErrPathBudgetExhausted) {
		t.Fatalf("tiny budget error = %v, want ErrPathBudgetExhausted", err)
	}
	if _, err := FindPath(open, Point{}, Point{X: 63, Y: 63}, PathOptions{}); err != nil {
		t.Fatalf("default budget must find the open path: %v", err)
	}
}

func TestBlockIndexQueryRadiusReturnsCandidateSuperset(t *testing.T) {
	index, err := NewBlockIndex(Rect{Max: Point{X: 100, Y: 100}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Add(1, Point{X: 50, Y: 50}) || !index.Add(2, Point{X: 58, Y: 50}) || !index.Add(3, Point{X: 90, Y: 90}) {
		t.Fatal("Add failed")
	}
	ids := index.QueryRadius(Point{X: 50, Y: 50}, 10)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("radius candidates = %v, want [1 2]", ids)
	}
	if ids := index.QueryRadius(Point{X: 50, Y: 50}, -1); ids != nil {
		t.Fatalf("negative radius must return nil, got %v", ids)
	}
	// Extreme centers must saturate instead of overflowing into a bogus rect.
	if ids := index.QueryRadius(Point{X: math.MaxInt64, Y: math.MaxInt64}, math.MaxInt64); len(ids) != 3 {
		t.Fatalf("saturated query = %v, want all ids", ids)
	}
}

func TestBlockIndexMoveVersionsAndDeterministicQueries(t *testing.T) {
	index, err := NewBlockIndex(Rect{Max: Point{X: 10, Y: 10}}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Add(20, Point{X: 1, Y: 1}) || !index.Add(10, Point{X: 2, Y: 1}) {
		t.Fatal("Add failed")
	}
	blocks, ok := index.BlockRects(Rect{Min: Point{}, Max: Point{X: 5, Y: 5}})
	if !ok {
		t.Fatal("BlockRects failed")
	}
	before := index.BlockVersions(blocks)
	if !index.Move(10, Point{X: 2, Y: 1}, Point{X: 8, Y: 8}) {
		t.Fatal("Move failed")
	}
	after := index.BlockVersions(blocks)
	if before[0] == after[0] {
		t.Fatal("source block version did not advance")
	}
	ids := index.QueryRect(Rect{Min: Point{}, Max: Point{X: 10, Y: 10}})
	if len(ids) != 2 || ids[0] != 10 || ids[1] != 20 {
		t.Fatalf("deterministic query = %v", ids)
	}
	if !index.Remove(20, Point{X: 1, Y: 1}) || len(index.QueryBlock(Point{X: 1, Y: 1})) != 0 {
		t.Fatal("Remove failed")
	}
}

func TestBlockIndexRejectsUnsafeAllocationAndDeduplicatesRange(t *testing.T) {
	if _, err := NewBlockIndex(Rect{Min: Point{X: math.MinInt64}, Max: Point{X: math.MaxInt64, Y: 1}}, 1); !errors.Is(err, ErrInvalidBounds) {
		t.Fatalf("extreme bounds error = %v, want ErrInvalidBounds", err)
	}
	if _, err := NewBlockIndex(Rect{Max: Point{X: MaxBlockCount + 1, Y: 1}}, 1); !errors.Is(err, ErrTooManyBlocks) {
		t.Fatalf("large index error = %v, want ErrTooManyBlocks", err)
	}

	index, err := NewBlockIndex(Rect{Max: Point{X: 4, Y: 2}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !index.Add(7, Point{}) || !index.Add(7, Point{X: 2}) {
		t.Fatal("Add failed")
	}
	blocks, _ := index.BlockRects(index.Bounds())
	var visited []int64
	index.RangeBlocks(blocks, func(id int64) bool {
		visited = append(visited, id)
		return true
	})
	if len(visited) != 1 || visited[0] != 7 {
		t.Fatalf("RangeBlocks visited = %v, want one deduplicated ID", visited)
	}
}

var benchmarkBlockIDs []int64

func BenchmarkBlockIndexQuery100Units(b *testing.B) {
	index, err := NewBlockIndex(Rect{Max: Point{X: 100, Y: 100}}, 10)
	if err != nil {
		b.Fatal(err)
	}
	for id := int64(1); id <= 100; id++ {
		if !index.Add(id, Point{X: (id - 1) % 10 * 10, Y: (id - 1) / 10 * 10}) {
			b.Fatalf("Add(%d) failed", id)
		}
	}
	query := index.Bounds()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		benchmarkBlockIDs = index.QueryRect(query)
	}
}
