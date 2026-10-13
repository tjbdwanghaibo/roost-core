package goroutine

import (
	"errors"
	"strings"
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

func TestSafeFuncWithTryCountWrapsLastError(t *testing.T) {
	cause := errors.New("db down")
	calls := 0
	err := SafeFuncWithTryCount(3, func() error {
		calls++
		return cause
	})
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want wrapped cause", err)
	}
}

func TestSafeFuncWithTryCountRecoversPanic(t *testing.T) {
	err := SafeFuncWithTryCount(2, func() error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestSafeFuncWithTryCountZeroCountStillRunsOnce(t *testing.T) {
	calls := 0
	if err := SafeFuncWithTryCount(0, func() error { calls++; return nil }); err != nil {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (zero count used to skip the function entirely)", calls)
	}
}
