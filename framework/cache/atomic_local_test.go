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

// AtomicLocalStore 的时钟记录 shard.order 随存活键数有界（否则永不淘汰的缓存会随写入次数线性增长）：
//   - 覆盖写、Delete、过期与写入交替时，len(order) ≤ 4×存活键数 + 常量；
//   - 压缩只丢弃已被取代的记录，不改变“覆盖写 = 重新插入”的淘汰顺序。
// 历史：RR-20260930-03。

// orderBound 是回归使用的上限（刻意不引用实现常量，基线上也能编译出红）：修复把未淘汰部分
// 限制在 2×存活键数 + 1024（压缩触发线），已淘汰前缀 hand 由既有压缩限制在 max(1024, len/2)，
// 合起来不超过 4×live + 2048。
func orderBound(live int) int { return 4*live + 2048 }

func checkOrderBounded[K comparable, V any](t *testing.T, store *AtomicLocalStore[K, V], stage string) {
	t.Helper()
	for i := range store.shards {
		shard := &store.shards[i]
		shard.mu.RLock()
		order, live, hand := len(shard.order), len(shard.items), shard.hand
		shard.mu.RUnlock()
		if order > orderBound(live) {
			t.Fatalf("%s: shard %d clock order unbounded: live=%d order=%d hand=%d bound=%d", stage, i, live, order, hand, orderBound(live))
		}
	}
}

// 调查员红测试原型：10 个键各覆盖写 1 万次，远低于 MaxEntries/MaxBytes，从不淘汰。
func TestAtomicLocalOrderBoundedUnderOverwrite(t *testing.T) {
	cfg := atomicTestConfig()
	cfg.MaxEntries = 1024
	cfg.MaxBytes = 1 << 20
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	const keys, rounds = 10, 10000
	for version := int64(1); version <= rounds; version++ {
		for key := range keys {
			if err := store.Set(ctx, atomicTestValue{Key: key, Version: version, Size: 1}); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkOrderBounded(t, store, "overwrite")
	// 带 TTL 的覆盖写（Remote 快照 L1 实际走 SetWithTTL）同样有界。
	for version := int64(rounds + 1); version <= 2*rounds; version++ {
		for key := range keys {
			if err := store.SetWithTTL(ctx, atomicTestValue{Key: key, Version: version, Size: 1}, time.Hour); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkOrderBounded(t, store, "overwrite with ttl")
	if stats := store.Stats(); stats.Entries != keys || stats.Evictions != 0 {
		t.Fatalf("stats=%+v, want %d entries and no eviction", stats, keys)
	}
}

// Delete 与写入交替：每轮写入后立即删除；以及大量存活键全部删除后，order 随存活键收缩。
func TestAtomicLocalOrderBoundedUnderDeleteChurn(t *testing.T) {
	cfg := atomicTestConfig()
	cfg.MaxEntries = 1 << 20
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	for i := range 100000 {
		key := i % 50
		if err := store.Set(ctx, atomicTestValue{Key: key, Version: int64(i + 1), Size: 1}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	checkOrderBounded(t, store, "set/delete churn")

	const live = 5000
	for key := range live {
		if err := store.Set(ctx, atomicTestValue{Key: key, Version: 1 << 30, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	checkOrderBounded(t, store, "fill")
	for key := range live {
		if err := store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	checkOrderBounded(t, store, "delete all")
	if stats := store.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("stats=%+v, want empty", stats)
	}
}

// 过期与写入交替：Get 发现过期时删除条目，这条删除路径同样要收缩 order。
func TestAtomicLocalOrderBoundedUnderExpiryChurn(t *testing.T) {
	now := time.Unix(100, 0)
	cfg := atomicTestConfig()
	cfg.MaxEntries = 1 << 20
	cfg.DefaultTTL = time.Second
	cfg.Now = func() time.Time { return now }
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	for i := range 100000 {
		key := i % 50
		if err := store.Set(ctx, atomicTestValue{Key: key, Version: int64(i + 1), Size: 1}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
		if _, ok, _ := store.Get(ctx, key); ok {
			t.Fatalf("key %d not expired", key)
		}
	}
	checkOrderBounded(t, store, "set/expire churn")

	const live = 5000
	for key := range live {
		if err := store.SetWithTTL(ctx, atomicTestValue{Key: key, Version: 1 << 30, Size: 1}, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(2 * time.Second)
	for key := range live {
		if _, ok, _ := store.Get(ctx, key); ok {
			t.Fatalf("key %d not expired", key)
		}
	}
	checkOrderBounded(t, store, "expire all")
	if stats := store.Stats(); stats.Entries != 0 || stats.Expired == 0 {
		t.Fatalf("stats=%+v, want empty with expirations", stats)
	}
}

// 压缩后淘汰顺序不变：先让 X 被淘汰（hand > 0，压缩要平移未淘汰部分），再依次写 A、B、C，
// A 反复覆盖到触发多次压缩；超限时应先淘汰 B，再 C，最后才轮到最近重写过的 A。
// “只在新键时追加记录”的修法会让 A 最先被淘汰，这里会失败。
func TestAtomicLocalCompactionKeepsEvictionOrder(t *testing.T) {
	const x, a, b, c, d, e, f = 100, 1, 2, 3, 4, 5, 6
	cfg := atomicTestConfig()
	cfg.MaxEntries = 3
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	set := func(key int, version int64) {
		t.Helper()
		if err := store.Set(ctx, atomicTestValue{Key: key, Version: version, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	present := func(keys ...int) {
		t.Helper()
		for _, key := range keys {
			if _, ok, _ := store.Get(ctx, key); !ok {
				t.Fatalf("key %d evicted early", key)
			}
		}
	}
	gone := func(key int) {
		t.Helper()
		if _, ok, _ := store.Get(ctx, key); ok {
			t.Fatalf("key %d should have been evicted first", key)
		}
	}
	set(x, 1)
	set(a, 1)
	set(b, 1)
	set(c, 1)
	gone(x)
	present(a, b, c)
	for version := int64(2); version <= 5000; version++ {
		set(a, version)
	}
	checkOrderBounded(t, store, "overwrite A")

	set(d, 1)
	gone(b)
	present(a, c, d)
	set(e, 1)
	gone(c)
	present(a, d, e)
	set(f, 1)
	gone(a)
	present(d, e, f)
	if stats := store.Stats(); stats.Evictions != 4 || stats.Entries != 3 {
		t.Fatalf("stats=%+v, want 4 evictions and 3 entries", stats)
	}
}

// 多分片并发覆盖写 / 删除 / 读：配合 -race 检查压缩只在分片写锁内改 order / hand。
func TestAtomicLocalOrderBoundedConcurrent(t *testing.T) {
	cfg := atomicTestConfig()
	cfg.Shards = 4
	cfg.MaxEntries = 64
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 20000 {
				key := (worker*7 + i) % 96
				switch i % 5 {
				case 3:
					_ = store.Delete(ctx, key)
				case 4:
					_, _, _ = store.Get(ctx, key)
				default:
					_ = store.Set(ctx, atomicTestValue{Key: key, Version: int64(worker*100000 + i), Size: 1})
				}
			}
		})
	}
	wg.Wait()
	checkOrderBounded(t, store, "concurrent")
}

// 覆盖写基准：固定小键集反复覆盖（Remote 快照 L1 的形态），对比修复前后 Set 的耗时与分配。
func BenchmarkAtomicLocalStoreOverwrite(b *testing.B) {
	cfg := atomicTestConfig()
	cfg.Shards = 64
	cfg.MaxEntries = 65536
	cfg.MaxBytes = 256 << 20
	cfg.DefaultTTL = time.Minute
	store := NewAtomicLocalStore(cfg)
	ctx := context.Background()
	const keys = 20000
	version := int64(0)
	for b.Loop() {
		version++
		if err := store.Set(ctx, atomicTestValue{Key: int(version % keys), Version: version, Size: 128}); err != nil {
			b.Fatal(err)
		}
	}
}
