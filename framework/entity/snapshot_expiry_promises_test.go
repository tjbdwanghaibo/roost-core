package entity

import (
	"context"
	"testing"
	"time"
)

// U-0175 · C8 · RR-20260913-08：信封自己的绝对有效期必须参与读取准入。
//
// RemoteSnapshotEnvelope 有 ExpiresAt 和 Expired(),但构造缓存时只用了固定 TTL,Get 从不看
// 这个字段。于是把 ExpiresAt 设成一秒前、容器 TTL 设成一分钟,Cached 读仍然 found=true,
// 而返回的这份 Expired(now) 为真 —— 调用方拿到的是一份已经声明过期的业务快照。
//
// 容器 TTL 是"本机缓存保留多久",信封 ExpiresAt 是"这份数据到什么时候为止还算数",两者判的不是
// 同一件事。可用性应由两者中更早的那个决定。
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
