package entity

import (
	"context"
	"testing"
	"time"
)

// U-0175 补 · RR-20260913-08 复核残留:所有对外 read 出口都要满足"不返回已过期的快照"。
//
// U-0175 只在 Get 的 layered.Get 命中后判了 Expired,而 Linearizable 直接返回 LoadAuthoritative,
// Monotonic miss 走 loadMonotonic 也返回 LoadAuthoritative 的结果,LoadAuthoritative 自己
// Publish 后 local.Get 直接返回 —— 三条出口都没有过期后置条件。权威返回一份已过期的快照,
// 调用方照样拿到 found=true。
//
// 还有一个交错:L1 里是一份过期值,权威回来同版本同内容但更新的有效期。Publish 的同版本去重
// 分支不看过期元数据直接返回 nil,LoadAuthoritative 再读旧 L1,结果仍是过期值。
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
