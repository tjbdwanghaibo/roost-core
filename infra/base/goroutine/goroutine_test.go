package goroutine

import (
	"sync/atomic"
	"testing"
)

func TestParallelSlice(t *testing.T) {
	data := []int{1, 2, 3, 4, 5, 6, 7, 8}
	var sum atomic.Int64

	ParallelSlice(data, func(i int, v int) {
		sum.Add(int64(v))
	})

	if sum.Load() != 36 {
		t.Fatalf("Expected sum 36, got %d", sum.Load())
	}
}

func TestParallelSliceCollect(t *testing.T) {
	data := []int{1, 2, 3, 4}
	results := ParallelSliceCollect(data, func(i int, v int) int {
		return v * v
	})

	expected := []int{1, 4, 9, 16}
	for i, v := range results {
		if v != expected[i] {
			t.Fatalf("Index %d: expected %d, got %d", i, expected[i], v)
		}
	}
}
