package goroutine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
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

// LocalMap 的每个 goroutine 只看到自己 ID 下的登记：同一片上的两个 ID 互不覆盖、删一个不影响另一个；
// 多个 goroutine 并发先存后删（Nest worker 每条消息的用法）时，各自读回的始终是自己的值。
// 变异核对：Delete 改成清空整片（clear(s.data)）时，同片的 67 被一起删掉，本用例变红。
func TestLocalMapKeepsEachGoroutineEntrySeparate(t *testing.T) {
	var m LocalMap[string]
	if _, ok := m.Load(3); ok {
		t.Fatal("zero LocalMap reports an entry")
	}
	m.Store(3, "a")
	m.Store(3+localMapShards, "b") // 与 3 同一片
	m.Delete(3)
	if _, ok := m.Load(3); ok {
		t.Fatal("deleted entry is still visible")
	}
	if v, ok := m.Load(3 + localMapShards); !ok || v != "b" {
		t.Fatalf("same-shard entry = %q, %v after deleting its neighbour; want \"b\", true", v, ok)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gid := GoID()
			for i := range 2000 {
				want := fmt.Sprint(gid, ":", i)
				m.Store(gid, want)
				if got, ok := m.Load(gid); !ok || got != want {
					t.Errorf("goroutine %d read %q, %v; want %q", gid, got, ok, want)
					return
				}
				m.Delete(gid)
			}
		}()
	}
	wg.Wait()
}
