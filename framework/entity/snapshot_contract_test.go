package entity

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"sync"
	"testing"
	"time"
)

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

// RR-20261005-NC-36：Publish 可以因较新 epoch 保留旧 L1，但 read 的最终结果仍须
// 满足 minVersion；不能只检查 loader 的原始结果。错误后不得覆盖较新 epoch，合法加载可恢复。
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

// RR-20260913-08 残余：L2 发布期间跨过信封截止时间，最终读取必须用当前时间再判过期。
// 等待的是信封自己的语义截止时间，不是用任意 sleep 猜测并发顺序。
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

// DeleteAtVersion mirrors the real L2's versioned delete (U-0187 复核补修):
// a stored snapshot newer than version survives.
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

// U-0180 · C8 · RR-20260913-05:L2 的版本冲突是一致性错误,不能被当成可降级的可用性故障吞掉。
//
// 缓存对 L2 固定 IgnoreRemoteError:true,ReadThrough 对 remote.Set 的一切错误统一降级再写 L1。
// L2 已存同 key/epoch/version 的 A,一个 L1 为空的进程 Publish 同版本的 B:L2 正确返回
// ErrRemoteVersionConflict,却被吞掉,Publish 返回 nil,L1=B、L2=A —— 同一个版本在不同进程返回
// 不同内容,而发布方收到成功。网络故障下保留 L1 可用是对的;语义冲突不是网络故障。
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

// U-0181 · C8 · RR-20260913-06:从 L2 回填 L1 必须走和 Publish 一样的同版本内容规则。
//
// ReadThrough 的 L2 命中直接 setLocal(value)。确定性交错:L1 为空,一次读在 L2 取到 A 后暂停;
// 另一路 Publish 同版本的 B 成功写进 L1;放行之前那次读 —— 回填把 L1 改回 A,没有任何错误。
// 所有写入 L1 的入口都要共用一次原子的版本/内容校验,不能只在 Publish 外层加锁。
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
