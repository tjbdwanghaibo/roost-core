// 远端快照缓存：增量与缺口、等待者有界、载荷不可变、各一致性级别的回源、L1 / L2 同版本规则、带版本删除与墓碑、过期不交出。

package entity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
)

func TestRemoteSnapshotCacheAppliesDeltaAndRejectsGap(t *testing.T) {
	const kind EntityKind = 191
	const schema uint32 = 44001
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9101, kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterRemoteSnapshotDelta(schema, func(base, delta []byte) ([]byte, error) {
		return append(base, delta...), nil
	}); err != nil {
		t.Fatal(err)
	}
	// delta 注册表同样是进程级的，重复注册返回 ErrRemoteSnapshotDecoderDuplicate；撤销本用例的登记，
	// 重复运行时每轮都从未注册开始（RR-20260926-83）。
	forgetRemoteSnapshotDeltaOnCleanup(t, schema)
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 4, MaxEntries: 32, MaxBytes: 1 << 20, TTL: time.Minute, MaxWaiters: 4}, nil, nil)
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	full := RemoteSnapshotRecord{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: schema, Codec: 1, Full: true, Data: []byte("a")}
	full.Checksum = RemoteSnapshotChecksum(full.Data)
	if err := cache.ApplyUpdate(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	delta := RemoteSnapshotRecord{Key: key, BaseVersion: 1, StateVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: schema, Codec: 1, Data: []byte("b")}
	delta.Checksum = RemoteSnapshotChecksum(delta.Data)
	if err := cache.ApplyUpdate(context.Background(), delta); err != nil {
		t.Fatal(err)
	}
	conflict := RemoteSnapshotEnvelope{Key: key, BaseVersion: 1, StateVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: schema, Codec: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("different"))}
	if err := cache.Publish(context.Background(), conflict); !errors.Is(err, ErrRemoteVersionConflict) {
		t.Fatalf("same-version conflict error=%v", err)
	}
	got, ok, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 2)
	if err != nil || !ok || string(got.Payload.BytesCopy()) != "ab" {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
	delta.BaseVersion = 1
	delta.StateVersion = 3
	if err := cache.ApplyUpdate(context.Background(), delta); !errors.Is(err, ErrRemoteSnapshotGap) {
		t.Fatalf("gap error=%v", err)
	}
}

func TestRemoteSnapshotCacheBoundsVersionWaiters(t *testing.T) {
	const kind EntityKind = 192
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9102, kind)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024, TTL: time.Minute, MaxWaiters: 1}, nil, nil)
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cache.WaitForVersion(ctx, key, 2) }()
	deadline := time.Now().Add(time.Second)
	for {
		cache.waitMu.Lock()
		count := cache.waiterCount
		cache.waitMu.Unlock()
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first waiter was not registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := cache.WaitForVersion(context.Background(), key, 2); !errors.Is(err, ErrRemoteOverloaded) {
		t.Fatalf("second waiter error=%v", err)
	}
	cancel()
	if err := awaitChan(t, done, "the waiter to observe its own cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter error=%v", err)
	}
}

func TestRemoteSnapshotPayloadCannotMutateCache(t *testing.T) {
	const kind EntityKind = 196
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9104, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024, TTL: time.Minute}, nil, nil)
	source := []byte("immutable")
	value := RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload(source)}
	if err := cache.Publish(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	source[0] = 'X'
	got, ok, err := cache.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || !ok || string(got.Payload.BytesCopy()) != "immutable" {
		t.Fatalf("snapshot=%+v ok=%v err=%v", got, ok, err)
	}
	copyOut := got.Payload.BytesCopy()
	copyOut[0] = 'Y'
	again, _, _ := cache.Get(context.Background(), key, RemoteReadCached, 0)
	if string(again.Payload.BytesCopy()) != "immutable" {
		t.Fatalf("cache payload mutated through read copy")
	}
}

func TestRemoteSnapshotLinearizableAlwaysLoadsAuthority(t *testing.T) {
	const kind EntityKind = 198
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9106, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	var calls atomic.Int32
	var observed RemoteReadConsistency
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024}, nil,
		func(_ context.Context, got RemoteSnapshotKey, consistency RemoteReadConsistency, minVersion uint64) (RemoteSnapshotEnvelope, bool, error) {
			calls.Add(1)
			observed = consistency
			if got != key || minVersion != 1 {
				t.Fatalf("loader key=%+v min=%d", got, minVersion)
			}
			return RemoteSnapshotEnvelope{Key: key, StateVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("authority"))}, true, nil
		})
	if err := cache.Publish(context.Background(), RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("cached"))}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := cache.Get(context.Background(), key, RemoteReadLinearizable, 1)
	if err != nil || !ok || calls.Load() != 1 || observed != RemoteReadLinearizable || string(got.Payload.BytesCopy()) != "authority" {
		t.Fatalf("snapshot=%+v ok=%v calls=%d consistency=%d err=%v", got, ok, calls.Load(), observed, err)
	}
}

func TestRemoteSnapshotCachedMissDoesNotLoadAuthority(t *testing.T) {
	const kind EntityKind = 200
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9108, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	var calls atomic.Int32
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{}, nil,
		func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
			calls.Add(1)
			return RemoteSnapshotEnvelope{}, false, nil
		})
	if _, ok, err := cache.Get(context.Background(), key, RemoteReadCached, 0); err != nil || ok {
		t.Fatalf("cached miss ok=%v err=%v", ok, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("cached miss performed %d authoritative loads", calls.Load())
	}
}

func TestRemoteSnapshotMonotonicStaleLoadsAreCoalesced(t *testing.T) {
	const kind EntityKind = 199
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9107, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024, MaxWaiters: 32}, nil,
		func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return RemoteSnapshotEnvelope{Key: key, StateVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("fresh"))}, true, nil
		})
	if err := cache.Publish(context.Background(), RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("old"))}); err != nil {
		t.Fatal(err)
	}
	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, ok, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 2)
			if err != nil {
				errs <- err
				return
			}
			if !ok || got.StateVersion != 2 {
				errs <- errors.New("monotonic load did not return requested version")
			}
		}()
	}
	awaitChan(t, started, "the coalesced load to start")
	deadline := time.Now().Add(time.Second)
	for {
		cache.loadMu.Lock()
		call := cache.loads[remoteSnapshotLoadKey{key: key, after: RemoteObservation{StateVersion: 2}}]
		waiters := 0
		if call != nil {
			waiters = call.waiters
		}
		cache.loadMu.Unlock()
		if waiters == readers-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("coalesced waiters=%d, want %d", waiters, readers-1)
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("authority loads=%d, want 1", calls.Load())
	}
}

func TestRemoteCommitRejectsMutationVersionDrift(t *testing.T) {
	const kind EntityKind = 197
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9105, kind)
	if err != nil {
		t.Fatal(err)
	}
	var tx RemoteTransactionID
	tx[15] = 1
	commit := RemoteCommit{
		TransactionID: tx, EntityID: id, Kind: kind,
		BaseVersion: 4, NextVersion: 5, MarkerEpoch: 1, RouteEpoch: 1,
		Mutations: []RemoteDataMutation{{Collection: "players", ID: id, Version: 6, Data: []byte("state")}},
	}
	if err := commit.Validate(); !errors.Is(err, ErrRemoteVersionConflict) {
		t.Fatalf("mutation version drift error=%v", err)
	}
}

var remoteSnapshotBenchmarkByte byte

func BenchmarkRemoteSnapshotCacheL1Get4K(b *testing.B) {
	const kind EntityKind = 193
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9103, kind)
	if err != nil {
		b.Fatal(err)
	}
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 64, MaxEntries: 1024, MaxBytes: 16 << 20, TTL: time.Minute, MaxWaiters: 64}, nil, nil)
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	value := RemoteSnapshotEnvelope{Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: TakeFrozenRemoteSnapshotPayload(make([]byte, 4<<10))}
	if err := cache.Publish(context.Background(), value); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snapshot, ok, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 1)
		if err != nil || !ok {
			b.Fatal(err)
		}
		remoteSnapshotBenchmarkByte = snapshot.Payload.data[0]
	}
}

// hangingSnapshotL2 blocks until its context is done, standing in for an
// unresponsive Redis: not refusing, just never answering.
type hangingSnapshotL2 struct {
	sets    atomic.Int32
	blocked chan struct{}
}

func (l2 *hangingSnapshotL2) Get(ctx context.Context, _ RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	<-ctx.Done()
	return RemoteSnapshotEnvelope{}, false, ctx.Err()
}

func (l2 *hangingSnapshotL2) Set(ctx context.Context, _ RemoteSnapshotEnvelope) error {
	l2.sets.Add(1)
	if l2.blocked != nil {
		close(l2.blocked)
		l2.blocked = nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (l2 *hangingSnapshotL2) Delete(ctx context.Context, _ RemoteSnapshotKey) error {
	<-ctx.Done()
	return ctx.Err()
}

// Publish holds a publish shard lock across the L2 write, because single-point
// publish is what makes the version CAS meaningful. That lock's hold time is
// therefore decided by the L2 call, so the call must be bounded: an
// unresponsive L2 has to degrade to "L1 only" instead of pinning the shard —
// and with it every entity that hashes to it — for as long as the L2 stays
// unresponsive.
func TestRemoteSnapshotPublishDoesNotPinShardOnUnresponsiveL2(t *testing.T) {
	const kind EntityKind = 193
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	first, err := BuildEntityID(9301, kind)
	if err != nil {
		t.Fatal(err)
	}
	l2 := &hangingSnapshotL2{}
	cache := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{
		Shards: 1, MaxEntries: 8, MaxBytes: 1 << 20, TTL: time.Minute,
		LoadTimeout: 100 * time.Millisecond,
	}, l2, nil)

	key := RemoteSnapshotKey{EntityID: first, Kind: kind, Scope: 1}
	envelope := RemoteSnapshotEnvelope{
		Key: key, StateVersion: 1, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Full: true, Payload: CopyFrozenRemoteSnapshotPayload([]byte("state")),
	}
	// Publish runs in its own goroutine so an unbounded L2 call fails as an
	// explicit assertion here rather than as a `go test` timeout minutes later.
	published := make(chan error, 1)
	go func() { published <- cache.Publish(context.Background(), envelope) }()
	select {
	case err := <-published:
		// IgnoreRemoteError is set, so the degraded publish must still succeed.
		if err != nil {
			t.Fatalf("publish returned %v; an unresponsive L2 must degrade, not fail", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("publish did not return within 2s: the L2 call is unbounded, so the publish shard stays pinned for as long as the L2 is unresponsive")
	}
	if l2.sets.Load() != 1 {
		t.Fatalf("L2 sets=%d, want 1", l2.sets.Load())
	}
	// L1 must still hold the snapshot: degrading means skipping L2, not
	// losing the publish. B2: it is held unconfirmed, so a Cached read does
	// not serve it until L2 or the authority confirms it.
	if got, ok, err := cache.l1Snapshot(context.Background(), key); err != nil || !ok || got.StateVersion != 1 {
		t.Fatalf("degraded publish did not populate L1: snapshot=%+v ok=%v err=%v", got, ok, err)
	}
	if got, ok, _ := cache.Get(context.Background(), key, RemoteReadCached, 0); ok {
		t.Fatalf("an unconfirmed L1 entry was served while L2 is unresponsive: version=%d", got.StateVersion)
	}
	// The shard is free: a second publish proceeds without waiting on the
	// first one's dead L2 call.
	second := envelope
	second.StateVersion = 2
	republished := make(chan error, 1)
	go func() { republished <- cache.Publish(context.Background(), second) }()
	select {
	case err := <-republished:
		if err != nil {
			t.Fatalf("second publish on the same shard: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second publish blocked: the first publish's dead L2 call still holds the shard lock")
	}
}

func TestAuthoritativeLoaderRejectsForeignKeyBeforePublish(t *testing.T) {
	key := l2ConflictKey(t, 233, 9535)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		for _, field := range []string{"tenant", "entity", "scope", "policy"} {
			t.Run(fmt.Sprintf("mode%d/%s", mode, field), func(t *testing.T) {
				foreign := key
				switch field {
				case "tenant":
					foreign.Tenant++
				case "entity":
					foreign.EntityID, _ = BuildEntityID(9536, key.Kind)
				case "scope":
					foreign.Scope++
				case "policy":
					foreign.Policy++
				}
				if !foreign.Valid() {
					t.Fatal("fixture foreign key is invalid")
				}
				bad := true
				l2 := newL2Fake()
				c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute}, l2,
					func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
						if bad {
							return snapshotAt(foreign, 4, "foreign"), true, nil
						}
						return snapshotAt(key, 4, "recovered"), true, nil
					})
				if err := c.Publish(context.Background(), snapshotAt(key, 2, "before")); err != nil {
					t.Fatal(err)
				}
				got, found, err := c.Get(context.Background(), key, mode, 4)
				wrong, leaked, l2Err := l2.Get(context.Background(), foreign)
				if err == nil || found || leaked || l2Err != nil {
					t.Fatalf("foreign authority accepted: found=%v version=%d err=%v foreignL2=%v/%d l2err=%v", found, got.StateVersion, err, leaked, wrong.StateVersion, l2Err)
				}
				if _, leaked, _ := c.l1Snapshot(context.Background(), foreign); leaked {
					t.Fatal("foreign result reached L1")
				}
				before, found, err := c.l1Snapshot(context.Background(), key)
				if err != nil || !found || before.StateVersion != 2 || string(before.Payload.BytesCopy()) != "before" {
					t.Fatal("rejection changed requested cache")
				}
				bad = false
				got, found, err = c.Get(context.Background(), key, mode, 4)
				if err != nil || !found || got.Key != key || got.StateVersion != 4 || string(got.Payload.BytesCopy()) != "recovered" {
					t.Fatalf("valid recovery: found=%v snapshot=%+v err=%v", found, got, err)
				}
			})
		}
	}
}

// Publish 可以因较新 epoch 保留旧 L1，但读的最终结果仍须满足 minVersion，不能只检查 loader 的原始结果；错误后不覆盖较新 epoch，
// 合法加载可恢复。
func TestAuthoritativeReadChecksStoredMinimumVersion(t *testing.T) {
	key := l2ConflictKey(t, 233, 9537)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			answer := snapshotAt(key, 4, "old-epoch")
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute}, nil,
				func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
					return answer, true, nil
				})
			current := snapshotAt(key, 2, "new-epoch")
			current.MarkerEpoch, current.RouteEpoch = 2, 2
			if err := c.Publish(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			got, found, err := c.Get(context.Background(), key, mode, 4)
			if !errors.Is(err, ErrRemoteSnapshotStale) || found {
				t.Fatalf("minimum version violated after admission: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
			kept, found, err := c.l1Snapshot(context.Background(), key)
			if err != nil || !found || kept.MarkerEpoch != 2 || kept.StateVersion != 2 || string(kept.Payload.BytesCopy()) != "new-epoch" {
				t.Fatal("older epoch overwrote current cache")
			}
			answer = snapshotAt(key, 4, "recovered")
			answer.MarkerEpoch, answer.RouteEpoch = 2, 2
			got, found, err = c.Get(context.Background(), key, mode, 4)
			if err != nil || !found || got.StateVersion != 4 || string(got.Payload.BytesCopy()) != "recovered" {
				t.Fatalf("valid recovery: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
		})
	}
}

// L2 发布期间跨过信封截止时间，最终读取用当前时间再判过期。等待的是信封自己的语义截止时间，不靠 sleep 猜测并发顺序。
type deadlineSnapshotL2 struct {
	*l2Fake
	writes int
}

func (s *deadlineSnapshotL2) Set(ctx context.Context, value RemoteSnapshotEnvelope) error {
	s.writes++
	if value.ExpiresAt > 0 {
		timer := time.NewTimer(time.Until(time.Unix(0, value.ExpiresAt)))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.l2Fake.Set(ctx, value)
}

func TestAuthoritativeReadRechecksExpiryAfterL2Publish(t *testing.T) {
	key := l2ConflictKey(t, 233, 9538)
	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		t.Run(fmt.Sprintf("mode%d", mode), func(t *testing.T) {
			l2 := &deadlineSnapshotL2{l2Fake: newL2Fake()}
			fresh := false
			c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, LoadTimeout: 3 * time.Second}, l2,
				func(context.Context, RemoteSnapshotKey, RemoteReadConsistency, uint64) (RemoteSnapshotEnvelope, bool, error) {
					if fresh {
						return snapshotAt(key, 5, "fresh"), true, nil
					}
					v := snapshotAt(key, 4, "same")
					v.ExpiresAt = time.Now().Add(250 * time.Millisecond).UnixNano()
					return v, true, nil
				})
			got, found, err := c.Get(context.Background(), key, mode, 4)
			if l2.writes != 1 {
				t.Fatalf("fixture did not cross L2 write: writes=%d", l2.writes)
			}
			if err != nil || found {
				t.Fatalf("expired during publish was served: found=%v expired=%v err=%v", found, got.Expired(time.Now()), err)
			}
			// 合法恢复使用较新版本，避免混入“同版本零有效期刷新”的另一个契约。
			fresh = true
			got, found, err = c.Get(context.Background(), key, mode, 5)
			if err != nil || !found || got.StateVersion != 5 || got.Expired(time.Now()) {
				t.Fatalf("expiry recovery: found=%v version=%d err=%v", found, got.StateVersion, err)
			}
		})
	}
}

func TestDeleteFenceCoversInflightL2Refill(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 240, 9801)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute, TombstoneTTL: time.Minute}, l2, nil)
	a := snapshotAt(key, 1, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	l2.getEntered, l2.getResume = entered, resume
	done := make(chan bool, 1)
	go func() {
		_, ok, _ := c.Get(ctx, key, RemoteReadCached, 0)
		done <- ok
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no L2 read")
	}
	err := c.DeleteAtVersion(ctx, key, 2)
	close(resume)
	if err != nil {
		t.Fatal(err)
	}
	ok := <-done
	_, cached, _ := c.l1Snapshot(ctx, key)
	if ok || cached {
		t.Fatalf("deleted v1 refilled through live tombstone: returned=%v cached=%v", ok, cached)
	}
}

func TestColdL1DeletePreservesNewerL2(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 241, 9802)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, l2, nil)
	a := snapshotAt(key, 3, "new")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := l2.Get(ctx, key)
	if !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed newer L2: found=%v version=%d", ok, got.StateVersion)
	}
	// 顺序对照:不比 L2 旧的删除照常清掉 L2。
	if err := c.DeleteAtVersion(ctx, key, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := l2.Get(ctx, key); ok {
		t.Fatal("delete at the stored version must remove the L2 copy")
	}
}

// B2：L1 冷时的 L2 预查已删除（它只为提前发现同版本冲突），同版本异值由 L2 CAS 的裁决
// （ErrRemoteVersionConflict）直接返回，不再有“预查之后、写入之前”的窗口。
func TestPublishConflictAfterPreflight(t *testing.T) {
	ctx := context.Background()
	key := l2ConflictKey(t, 242, 9803)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, l2, nil)
	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	err := c.Publish(ctx, snapshotAt(key, 5, "B"))
	local, ok, _ := c.l1Snapshot(ctx, key)
	remote, _, _ := l2.Get(ctx, key)
	if !errors.Is(err, ErrRemoteVersionConflict) || ok {
		t.Fatalf("conflict swallowed: err=%v L1=%q L2=%q", err, local.Payload.BytesCopy(), remote.Payload.BytesCopy())
	}
	// 降级契约保留:L2 纯故障时 Publish 仍成功并写 L1。
	outage := newL2Fake()
	outage.setErr = errors.New("dial tcp: connection refused")
	degraded := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{TTL: time.Minute}, outage, nil)
	if err := degraded.Publish(ctx, snapshotAt(key, 6, "C")); err != nil {
		t.Fatalf("an L2 outage must still degrade to L1: %v", err)
	}
	if _, ok, _ := degraded.l1Snapshot(ctx, key); !ok {
		t.Fatal("degraded publish did not reach L1")
	}
}

func deleteVersionKey(t *testing.T, kind EntityKind, seq int64) RemoteSnapshotKey {
	t.Helper()
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(seq, kind)
	if err != nil {
		t.Fatal(err)
	}
	return RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func deleteVersionEnvelope(key RemoteSnapshotKey, version uint64, body string) RemoteSnapshotEnvelope {
	return RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte(body)),
	}
}

func TestRemoteSnapshotDeleteAtVersionPromiseKeepsNewerSnapshot(t *testing.T) {
	key := deleteVersionKey(t, 236, 9101)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, nil)
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 3, "v3")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed v3: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

func TestRemoteSnapshotDeleteAtVersionPromiseFencesOlderSnapshot(t *testing.T) {
	key := deleteVersionKey(t, 237, 9102)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, nil)
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || ok {
		t.Fatalf("deleted snapshot resurrected: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	// 同版本的迟到写也在墓碑之内。
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 2, "v2")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(context.Background(), key, RemoteReadCached, 0); ok {
		t.Fatal("snapshot at the delete's own version resurrected the key")
	}
	// 比墓碑新的快照让键重新活过来，并清掉墓碑。
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 3, "v3")); err != nil {
		t.Fatal(err)
	}
	got, ok, err = c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("newer snapshot did not revive the key: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	// B2：删除标记是 L1 里的一个条目，更新的快照取代它。
	if entry, ok := c.l1Entry(context.Background(), key); !ok || entry.deleted {
		t.Fatal("tombstone survived a newer snapshot")
	}
}

// 边界：墓碑只活 TombstoneTTL。这是有意的——比可重放窗口还晚的旧快照不在协议保护范围里。
func TestRemoteSnapshotDeleteAtVersionPromiseTombstoneExpires(t *testing.T) {
	key := deleteVersionKey(t, 238, 9103)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4, TombstoneTTL: time.Millisecond}, nil, nil)
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(context.Background(), key, RemoteReadCached, 0); !ok {
		t.Fatal("an expired tombstone must stop fencing")
	}
}

func TestAuthoritativeReadsNeverReturnAnExpiredSnapshot(t *testing.T) {
	const kind EntityKind = 233
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9313, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	expiredAt := time.Now().Add(-time.Second).UnixNano()

	for _, mode := range []RemoteReadConsistency{RemoteReadMonotonic, RemoteReadLinearizable} {
		loader := func(_ context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
			return RemoteSnapshotEnvelope{
				Key: k, StateVersion: 4, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
				Payload: CopyFrozenRemoteSnapshotPayload([]byte("dead")), ExpiresAt: expiredAt,
			}, true, nil
		}
		c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, loader)
		got, ok, err := c.Get(context.Background(), key, mode, 0)
		if err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		if ok && got.Expired(time.Now()) {
			t.Fatalf("mode %d returned an expired authoritative snapshot (version %d)", mode, got.StateVersion)
		}
	}

	// Same version, same bytes, fresher expiry from the authority must replace
	// the expired L1 copy rather than be deduplicated against it.
	fresh := time.Now().Add(time.Hour).UnixNano()
	loader := func(_ context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
		return RemoteSnapshotEnvelope{
			Key: k, StateVersion: 7, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
			Payload: CopyFrozenRemoteSnapshotPayload([]byte("same")), ExpiresAt: fresh,
		}, true, nil
	}
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, loader)
	stale := RemoteSnapshotEnvelope{
		Key: key, StateVersion: 7, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte("same")), ExpiresAt: expiredAt,
	}
	if err := c.Publish(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), key, RemoteReadMonotonic, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a fresh same-version authoritative result must be served, not turned into a miss")
	}
	if got.Expired(time.Now()) || got.ExpiresAt != fresh {
		t.Fatalf("fresh authority result was replaced by the expired same-version L1 copy: expires_at=%d want %d", got.ExpiresAt, fresh)
	}
}

func TestExpiredSnapshotIsNotServedFromCache(t *testing.T) {
	const kind EntityKind = 232
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9312, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}

	loaded := 0
	loader := func(_ context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
		loaded++
		return RemoteSnapshotEnvelope{
			Key: k, StateVersion: 9, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
			Payload: CopyFrozenRemoteSnapshotPayload([]byte("fresh")),
		}, true, nil
	}
	c := NewRemoteSnapshotCache(
		RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4},
		nil, loader)

	expired := RemoteSnapshotEnvelope{
		Key: key, StateVersion: 3, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload:   CopyFrozenRemoteSnapshotPayload([]byte("stale")),
		ExpiresAt: time.Now().Add(-time.Second).UnixNano(),
	}
	if err := c.Publish(context.Background(), expired); err != nil {
		t.Fatal(err)
	}

	// Cached: an expired value is a miss, not a hit.
	got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("a cached read served a snapshot that declares itself expired: version=%d expired=%v",
			got.StateVersion, got.Expired(time.Now()))
	}

	// Monotonic: an expired value is a miss too, so the authority is consulted.
	got, ok, err = c.Get(context.Background(), key, RemoteReadMonotonic, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.StateVersion != 9 {
		t.Fatalf("monotonic read after expiry = (%d, %v), want the authoritative version 9", got.StateVersion, ok)
	}
	if loaded != 1 {
		t.Fatalf("authority consulted %d times, want once", loaded)
	}

	// A value with no absolute expiry is unaffected: the container TTL alone
	// governs it, exactly as before.
	forever := RemoteSnapshotEnvelope{
		Key: key, StateVersion: 11, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte("no-expiry")),
	}
	if err := c.Publish(context.Background(), forever); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0); err != nil || !ok || got.StateVersion != 11 {
		t.Fatalf("a snapshot without ExpiresAt = (%d, %v, %v), want it served", got.StateVersion, ok, err)
	}
}

type l2Fake struct {
	mu     sync.Mutex
	values map[RemoteSnapshotKey]RemoteSnapshotEnvelope
	// getEntered / getResume, when set, pin the next Get after it captured
	// its value and before it returns.
	getEntered chan struct{}
	getResume  chan struct{}
	setErr     error
}

func newL2Fake() *l2Fake { return &l2Fake{values: map[RemoteSnapshotKey]RemoteSnapshotEnvelope{}} }

func (f *l2Fake) Get(_ context.Context, key RemoteSnapshotKey) (RemoteSnapshotEnvelope, bool, error) {
	f.mu.Lock()
	value, ok := f.values[key]
	entered, resume := f.getEntered, f.getResume
	f.getEntered, f.getResume = nil, nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
		<-resume
	}
	return value, ok, nil
}

func (f *l2Fake) Set(_ context.Context, value RemoteSnapshotEnvelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if current, ok := f.values[value.Key]; ok &&
		current.MarkerEpoch == value.MarkerEpoch && current.RouteEpoch == value.RouteEpoch && current.StateVersion == value.StateVersion {
		if current.Checksum != value.Checksum || current.Schema != value.Schema || current.Codec != value.Codec {
			return ErrRemoteVersionConflict
		}
	}
	f.values[value.Key] = value
	return nil
}

func (f *l2Fake) Delete(_ context.Context, key RemoteSnapshotKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, key)
	return nil
}

// DeleteAtVersion mirrors the real L2's versioned delete: a stored snapshot newer than version survives.
func (f *l2Fake) DeleteAtVersion(_ context.Context, key RemoteSnapshotKey, version uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if current, ok := f.values[key]; ok && current.StateVersion > version {
		return nil
	}
	delete(f.values, key)
	return nil
}

var _ cache.Store[RemoteSnapshotKey, RemoteSnapshotEnvelope] = (*l2Fake)(nil)

func l2ConflictKey(t *testing.T, kind EntityKind, unique int64) RemoteSnapshotKey {
	t.Helper()
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func snapshotAt(key RemoteSnapshotKey, version uint64, body string) RemoteSnapshotEnvelope {
	return RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte(body)),
	}
}

// L2 的版本冲突是一致性错误，不能被当成可降级的可用性故障吞掉：同版本异值的 Publish 返回 ErrRemoteVersionConflict，不写 L1；
// 网络故障下仍降级写 L1。
// 历史：RR-20260913-05。
func TestPublishSurfacesAnL2VersionConflictAndKeepsItOutOfL1(t *testing.T) {
	key := l2ConflictKey(t, 234, 9314)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, l2, nil)

	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	b := snapshotAt(key, 5, "B")
	err := c.Publish(context.Background(), b)
	if !errors.Is(err, ErrRemoteVersionConflict) {
		t.Fatalf("publishing a conflicting same-version value = %v, want ErrRemoteVersionConflict", err)
	}
	if got, ok, _ := c.l1Snapshot(context.Background(), key); ok {
		t.Fatalf("the conflicting value reached L1: %q", got.Payload.BytesCopy())
	}

	// A plain L2 outage is still degradable: L1 takes the value, Publish succeeds.
	outage := newL2Fake()
	outage.setErr = errors.New("connection refused")
	degraded := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, outage, nil)
	if err := degraded.Publish(context.Background(), snapshotAt(key, 5, "B")); err != nil {
		t.Fatalf("an L2 outage must still degrade to L1: %v", err)
	}
	if _, ok, _ := degraded.l1Snapshot(context.Background(), key); !ok {
		t.Fatal("degraded publish did not populate L1")
	}
}

// 从 L2 回填 L1 走和 Publish 一样的同版本内容规则：读在 L2 取到 A 后暂停、另一路 Publish 同版本的 B 已写进 L1 时，回填不能把 L1
// 改回 A。所有写入 L1 的入口共用一次原子的版本 / 内容校验。
// 历史：RR-20260913-06。
func TestL2BackfillCannotOverwriteAPublishedSameVersionValue(t *testing.T) {
	key := l2ConflictKey(t, 235, 9315)
	l2 := newL2Fake()
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, l2, nil)

	a := snapshotAt(key, 5, "A")
	a.Checksum = RemoteSnapshotChecksum(a.Payload.data)
	if err := l2.Set(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	// Pin a read after it captured A from L2 and before it backfills L1.
	entered, resume := make(chan struct{}), make(chan struct{})
	l2.mu.Lock()
	l2.getEntered, l2.getResume = entered, resume
	l2.mu.Unlock()
	type result struct {
		value RemoteSnapshotEnvelope
		ok    bool
		err   error
	}
	read := make(chan result, 1)
	go func() {
		v, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
		read <- result{v, ok, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never reached L2")
	}

	// Meanwhile B is published for the same version. L2 refuses it (A is
	// there), which the publish surfaces; but that is not this test's point —
	// what matters is that L1 now holds B and the paused backfill must not
	// undo it. Use a fresh L2-less publish path to seed L1 with B.
	l2.mu.Lock()
	l2.values[key] = snapshotAt(key, 5, "B")
	l2.values[key] = func(v RemoteSnapshotEnvelope) RemoteSnapshotEnvelope {
		v.Checksum = RemoteSnapshotChecksum(v.Payload.data)
		return v
	}(l2.values[key])
	l2.mu.Unlock()
	if err := c.Publish(context.Background(), snapshotAt(key, 5, "B")); err != nil {
		t.Fatalf("publishing B: %v", err)
	}
	if got, ok, _ := c.l1Snapshot(context.Background(), key); !ok || string(got.Payload.BytesCopy()) != "B" {
		t.Fatalf("control: L1 does not hold B after publish (ok=%v)", ok)
	}

	close(resume)
	r := <-read
	if r.err != nil {
		t.Fatalf("the paused read failed: %v", r.err)
	}
	if got, ok, _ := c.l1Snapshot(context.Background(), key); !ok || string(got.Payload.BytesCopy()) != "B" {
		t.Fatalf("the L2 backfill overwrote the published same-version value: L1=%q", got.Payload.BytesCopy())
	}
}

func TestSnapshotLoadReturnsACanceledWaitersSlot(t *testing.T) {
	const kind EntityKind = 231
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(9311, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	loader := func(ctx context.Context, k RemoteSnapshotKey, _ RemoteReadConsistency, _ uint64) (RemoteSnapshotEnvelope, bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return RemoteSnapshotEnvelope{
			Key: k, StateVersion: 5, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
			Payload: CopyFrozenRemoteSnapshotPayload([]byte("authority")),
		}, true, nil
	}
	cache := NewRemoteSnapshotCache(
		RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 4, MaxBytes: 1024, TTL: time.Minute, MaxWaiters: 1},
		nil, loader)

	leader := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5)
		leader <- err
	}()
	awaitSignal(t, entered, "the authoritative load to start")
	waitForLoadWaiters(t, cache, 0)

	// One follower joins and takes the only slot.
	followerCtx, cancelFollower := context.WithCancel(context.Background())
	follower := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(followerCtx, key, RemoteReadMonotonic, 5)
		follower <- err
	}()
	waitForLoadWaiters(t, cache, 1)

	// While it is really waiting, the slot is taken: a second follower is refused.
	if _, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5); !errors.Is(err, ErrRemoteOverloaded) {
		t.Fatalf("a live follower must hold the only slot, got %v", err)
	}

	// It leaves. The slot it was holding has to come back.
	cancelFollower()
	if err := awaitChan(t, follower, "the canceled follower to return"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower = %v", err)
	}
	waitForLoadWaiters(t, cache, 0)

	replacement := make(chan error, 1)
	go func() {
		_, _, err := cache.Get(context.Background(), key, RemoteReadMonotonic, 5)
		replacement <- err
	}()
	waitForLoadWaiters(t, cache, 1)

	close(release)
	if err := awaitChan(t, leader, "the leader to finish"); err != nil {
		t.Fatalf("leader = %v", err)
	}
	if err := awaitChan(t, replacement, "the replacement follower to finish"); err != nil {
		t.Fatalf("replacement follower = %v; it was admitted, so it must get the loaded value", err)
	}
}

func waitForLoadWaiters(t *testing.T, c *RemoteSnapshotCache, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.loadMu.Lock()
		got := 0
		for _, call := range c.loads {
			got += call.waiters
		}
		c.loadMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("load waiters = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
