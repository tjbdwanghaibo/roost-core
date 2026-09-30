package cache

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type atomicTestValue struct {
	Key     int
	Version int64
	Size    int64
}

func atomicTestConfig() AtomicLocalConfig[int, atomicTestValue] {
	return AtomicLocalConfig[int, atomicTestValue]{
		StoreConfig: StoreConfig[int, atomicTestValue]{
			KeyOf: func(v atomicTestValue) int { return v.Key },
			Stale: VersionStale(func(v atomicTestValue) int64 { return v.Version }),
		},
		Shards: 1,
		SizeOf: func(v atomicTestValue) int64 { return v.Size },
	}
}

func TestAtomicLocalStoreVersionTTLAndBounds(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := atomicTestConfig()
	cfg.MaxEntries = 2
	cfg.MaxBytes = 8
	cfg.DefaultTTL = time.Second
	cfg.Now = func() time.Time { return now }
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	if err := store.Set(ctx, atomicTestValue{Key: 1, Version: 2, Size: 4}); err != nil {
		t.Fatal(err)
	}
	// The older version must be refused, and refused visibly: a nil here
	// would let a caller report success for a write that never happened.
	if err := store.Set(ctx, atomicTestValue{Key: 1, Version: 1, Size: 4}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale Set returned %v, want ErrStaleWrite", err)
	}
	got, ok, _ := store.Get(ctx, 1)
	if !ok || got.Version != 2 {
		t.Fatalf("version=%d ok=%v", got.Version, ok)
	}
	if err := store.Set(ctx, atomicTestValue{Key: 2, Version: 1, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, atomicTestValue{Key: 3, Version: 1, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Entries != 2 || stats.Bytes != 8 || stats.Evictions == 0 {
		t.Fatalf("stats=%+v", stats)
	}
	now = now.Add(2 * time.Second)
	_, ok, _ = store.Get(ctx, 3)
	if ok {
		t.Fatal("expired entry returned")
	}
	if err := store.Set(ctx, atomicTestValue{Key: 4, Version: 1, Size: 9}); !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("err=%v", err)
	}
}

func TestReadThroughStoreCoalescesMisses(t *testing.T) {
	cfg := atomicTestConfig()
	local := NewAtomicLocalStore(cfg)
	var loads atomic.Int32
	start := make(chan struct{})
	store := NewReadThroughStore[int, atomicTestValue](local, nil, func(context.Context, int) (atomicTestValue, bool, error) {
		loads.Add(1)
		<-start
		return atomicTestValue{Key: 7, Version: 1}, true, nil
	}, cfg.StoreConfig, ReadThroughOptions{MaxWaitersPerKey: 16})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, ok, err := store.Get(context.Background(), 7)
			if err != nil || !ok || value.Key != 7 {
				t.Errorf("value=%+v ok=%v err=%v", value, ok, err)
			}
		}()
	}
	// The promise is that misses in flight at the same time share one load.
	// Pin all eight there before releasing the loader: one inside the loader
	// (Loads == 1) and the other seven registered as followers (Coalesced ==
	// 7). Releasing on loads > 0 alone let a goroutine that had already
	// missed L1 reach load after the leader was done and start a load of its
	// own (RR-20260930-11).
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := store.Stats()
		if stats.Loads == 1 && stats.Coalesced == 7 {
			break
		}
		if time.Now().After(deadline) {
			close(start)
			wg.Wait()
			t.Fatalf("waiting for 1 load and 7 followers: stats=%+v", stats)
		}
		runtime.Gosched()
	}
	close(start)
	wg.Wait()
	if got := loads.Load(); got != 1 {
		t.Fatalf("loads=%d", got)
	}
	if stats := store.Stats(); stats.Loads != 1 || stats.Coalesced != 7 {
		t.Fatalf("stats=%+v, want Loads=1 Coalesced=7", stats)
	}
}
