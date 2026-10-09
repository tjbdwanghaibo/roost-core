package spatial

import (
	"errors"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
)

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
