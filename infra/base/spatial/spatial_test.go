package spatial

import (
	"errors"
	"math"
	"math/rand/v2"
	"runtime"
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

// The race detector is only half the requirement: concurrent SetBlocked /
// Blocked must also stay consistent and must not swallow errors. The previous
// version discarded both return values, so a terrain that rejected every write
// would have passed.
func TestGridTerrainConcurrentAccessKeepsWritesConsistent(t *testing.T) {
	terrain, err := NewGridTerrain(32, 32)
	if err != nil {
		t.Fatal(err)
	}
	// Each worker owns one column, so the final state is deterministic even
	// though the writes interleave.
	const workers, rounds = 8, 1000
	var group sync.WaitGroup
	failures := make([]error, workers)
	for worker := int64(0); worker < workers; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			for index := int64(0); index < rounds; index++ {
				point := Point{X: worker, Y: index % 32}
				blocked := (index/32)%2 == 0
				if err := terrain.SetBlocked(point, blocked); err != nil {
					failures[worker] = err
					return
				}
				// Reading another worker's column concurrently is the part
				// the race detector is here for.
				_ = terrain.Blocked(Point{X: (worker + 1) % workers, Y: index % 32})
			}
		}()
	}
	group.Wait()
	for worker, err := range failures {
		if err != nil {
			t.Fatalf("worker %d: SetBlocked failed: %v", worker, err)
		}
	}
	// The expected value is derived from this test's own write schedule, not
	// hardcoded: for row y the last write is the largest index < rounds with
	// index%32 == y.
	for worker := int64(0); worker < workers; worker++ {
		for y := int64(0); y < 32; y++ {
			lastIndex := y + 32*((rounds-1-y)/32)
			want := (lastIndex/32)%2 == 0
			if got := terrain.Blocked(Point{X: worker, Y: y}); got != want {
				t.Fatalf("cell (%d,%d) = %v, want %v (last write at index %d)",
					worker, y, got, want, lastIndex)
			}
		}
	}
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

// NewBlockIndex 接受的合法边界上，BlockRect 必须是落在 bounds 内、包含其块内全部点的半开矩形：在 int64 上界附近
// 计算最后一块的边界不能溢出成倒置矩形。
func TestBlockRectsStayInsideBoundsAtTheInt64Edge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bounds    Rect
		blockSize int64
		probes    []Point
	}{
		{
			name:      "upper edge, two huge blocks",
			bounds:    Rect{Max: Point{X: math.MaxInt64, Y: 1}},
			blockSize: 1 << 62,
			probes:    []Point{{X: 0}, {X: 1<<62 - 1}, {X: 1 << 62}, {X: math.MaxInt64 - 1}},
		},
		{
			name:      "upper edge on both axes",
			bounds:    Rect{Min: Point{X: 1, Y: 1}, Max: Point{X: math.MaxInt64, Y: math.MaxInt64}},
			blockSize: 1 << 62,
			probes:    []Point{{X: 1, Y: 1}, {X: math.MaxInt64 - 1, Y: math.MaxInt64 - 1}, {X: math.MaxInt64 / 2, Y: math.MaxInt64 - 1}},
		},
		{
			name:      "lower edge control",
			bounds:    Rect{Min: Point{X: math.MinInt64 + 1}, Max: Point{X: 0, Y: 1}},
			blockSize: 1 << 62,
			probes:    []Point{{X: math.MinInt64 + 1}, {X: -1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, err := NewBlockIndex(tc.bounds, tc.blockSize)
			if err != nil {
				t.Fatalf("NewBlockIndex refused a configuration this test relies on: %v", err)
			}
			for block := int64(0); block < index.cols*index.rows; block++ {
				rect := index.BlockRect(block)
				if rect.Min.X >= rect.Max.X || rect.Min.Y >= rect.Max.Y {
					t.Fatalf("block %d rect %+v is empty or inverted", block, rect)
				}
				if rect.Min.X < tc.bounds.Min.X || rect.Min.Y < tc.bounds.Min.Y || rect.Max.X > tc.bounds.Max.X || rect.Max.Y > tc.bounds.Max.Y {
					t.Fatalf("block %d rect %+v leaves bounds %+v", block, rect, tc.bounds)
				}
			}
			for _, point := range tc.probes {
				block := index.BlockIndex(point)
				if block < 0 {
					t.Fatalf("point %+v inside bounds has no block", point)
				}
				if rect := index.BlockRect(block); !rect.Contains(point) {
					t.Fatalf("point %+v is in block %d but BlockRect(%d) = %+v does not contain it", point, block, block, rect)
				}
				blocks, ok := index.BlockRects(Rect{Min: point, Max: Point{X: point.X + 1, Y: point.Y + 1}})
				if !ok || len(blocks) != 1 || !blocks[block].Contains(point) {
					t.Fatalf("BlockRects around %+v = %+v (ok %v), want block %d containing it", point, blocks, ok, block)
				}
			}
		})
	}
}

func TestPathBudgetCountsExpandedCells(t *testing.T) {
	rng := rand.New(rand.NewPCG(71, 19))
	for trial := 0; trial < 200; trial++ {
		grid, _ := NewGridTerrain(15, 15)
		for x := int64(0); x < 15; x++ {
			for y := int64(0); y < 15; y++ {
				if rng.IntN(5) == 0 {
					grid.SetBlocked(Point{x, y}, true)
				}
			}
		}
		grid.SetBlocked(Point{0, 0}, false)
		grid.SetBlocked(Point{14, 14}, false)
		grid.SetBlocked(Point{13, 14}, true)
		grid.SetBlocked(Point{14, 13}, true)
		_, err := FindPath(grid, Point{0, 0}, Point{14, 14}, PathOptions{MaxVisited: 225 - len(grid.obstacles)})
		if !errors.Is(err, ErrNoPath) {
			t.Fatalf("trial=%d budget covers every free cell but got %v", trial, err)
		}
	}
}

func TestGridSearchSeesOneObstacleView(t *testing.T) {
	grid, _ := NewGridTerrain(100, 1)
	a, b := Point{25, 0}, Point{75, 0}
	grid.SetBlocked(a, true)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			grid.mu.Lock()
			if _, ok := grid.obstacles[a]; ok {
				delete(grid.obstacles, a)
				grid.obstacles[b] = struct{}{}
			} else {
				delete(grid.obstacles, b)
				grid.obstacles[a] = struct{}{}
			}
			grid.mu.Unlock()
			runtime.Gosched()
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	for range 5000 {
		if path, err := FindPath(grid, Point{0, 0}, Point{99, 0}, PathOptions{}); !errors.Is(err, ErrNoPath) {
			t.Fatalf("crossed a corridor that is always blocked: path=%v err=%v", path, err)
		}
	}
}
