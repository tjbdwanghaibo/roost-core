package entity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
		call := cache.loads[remoteSnapshotLoadKey{key: key, minVersion: 2}]
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
	// losing the publish.
	if got, ok, err := cache.Get(context.Background(), key, RemoteReadCached, 0); err != nil || !ok || got.StateVersion != 1 {
		t.Fatalf("degraded publish did not populate L1: snapshot=%+v ok=%v err=%v", got, ok, err)
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
