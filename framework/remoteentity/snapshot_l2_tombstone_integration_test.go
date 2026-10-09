//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20261005-NC-130 与 RR-20260913-01 复核（2026-10-05，N05）在真实 Redis 上的验证：替身只证明我们对脚本的理解，
// 这里 CAS / DeleteAtVersion 的 Lua 由真实 Redis 执行。键用独立部署前缀 revn05:<pid>:<ns>:<用例>，
// 结束时逐键删除；以 ROOST_DATAENGINE_IT 准入（realRemoteRedis）。单独运行：
//
//	go test -tags integration -run 'TestRealSnapshotL2(StaleWrite|Tombstone)' ./framework/remoteentity
func revn05L2Prefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("revn05:%d:%d", os.Getpid(), time.Now().UnixNano())
}

// 真实 CAS 落败返回 ErrStaleWrite，L2 内容不变；L1 冷的节点不被钉在旧版本上。
func TestRealSnapshotL2StaleWriteIsReportedAndNotPinnedInL1(t *testing.T) {
	r := realRemoteRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := revn05L2Prefix(t)
	key := staleBackfillKey(t, staleBackfillReplicaKind, 9441)
	b27DeleteKeys(t, r, prefix+":"+remoteSnapshotL2Key(key))
	store, err := NewSnapshotL2StoreWithKeyPrefix(r, time.Minute, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("older write on real Redis = %v, want cache.ErrStaleWrite", err)
	}
	if err := store.Set(ctx, staleBackfillEnvelope(key, 5, 1, "old-epoch")); err != nil {
		t.Fatalf("newer version, same epoch: %v", err)
	}
	newEpoch := staleBackfillEnvelope(key, 6, 2, "new-epoch")
	if err := store.Set(ctx, newEpoch); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, staleBackfillEnvelope(key, 7, 1, "late-old-epoch")); !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("older route epoch on real Redis = %v, want cache.ErrStaleWrite", err)
	}

	reader := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1301, mustPrefixedL2(t, r, prefix))
	if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, staleBackfillReplica(t, key, 7, 1, "late-old-epoch")); err != nil {
		t.Fatal(err)
	}
	got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !found || got.RouteEpoch != 2 || got.StateVersion != 6 {
		t.Fatalf("cold node after a late old-epoch replica: version=%d route=%d found=%v err=%v", got.StateVersion, got.RouteEpoch, found, err)
	}
}

// 真实删除墓碑：TTL、拒绝不新于墓碑的写、重建清墓碑、更旧的删除不降低水位、旧格式混合键收敛。
func TestRealSnapshotL2TombstoneFencesEveryWriter(t *testing.T) {
	r := realRemoteRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := revn05L2Prefix(t)
	key := staleBackfillKey(t, staleBackfillReplicaKind, 9442)
	redisKey := prefix + ":" + remoteSnapshotL2Key(key)
	b27DeleteKeys(t, r, redisKey)
	store := mustPrefixedL2(t, r, prefix)

	if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	if v := b27HashField(t, r, redisKey, "deleted_version"); v != "2" {
		t.Fatalf("tombstone deleted_version=%q, want 2", v)
	}
	if v := b27HashField(t, r, redisKey, "data"); v != "" {
		t.Fatalf("tombstone still carries data %q", v)
	}
	if ttl := b27RedisPTTL(t, r, redisKey); ttl <= 0 || ttl > time.Minute.Milliseconds() {
		t.Fatalf("tombstone PTTL=%dms, want within the store TTL", ttl)
	}
	if _, held, err := store.Get(ctx, key); err != nil || held {
		t.Fatalf("Get on a tombstone: held=%v err=%v", held, err)
	}
	for _, version := range []uint64{1, 2} {
		if err := store.Set(ctx, staleBackfillEnvelope(key, version, 9, "resurrect")); !errors.Is(err, cache.ErrStaleWrite) {
			t.Fatalf("write v%d (any epoch) over tombstone 2 = %v, want cache.ErrStaleWrite", version, err)
		}
	}
	if err := store.DeleteAtVersion(ctx, key, 1); err != nil {
		t.Fatal(err)
	}
	if v := b27HashField(t, r, redisKey, "deleted_version"); v != "2" {
		t.Fatalf("an older delete lowered the tombstone to %q", v)
	}

	// 跨节点：owner 删除后，另一节点在途加载回来的 v1 写不回 L2，冷节点读到 miss。
	owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1311, mustPrefixedL2(t, r, prefix))
	nodeB := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, mustPrefixedL2(t, r, prefix),
		func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 3); err != nil {
				t.Errorf("owner delete: %v", err)
			}
			return staleBackfillEnvelope(key, 1, 1, "v1"), true, nil
		})
	if _, _, err := nodeB.LoadAuthoritative(ctx, key, entity.RemoteReadMonotonic, 0); err != nil {
		t.Fatal(err)
	}
	cold := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1312, mustPrefixedL2(t, r, prefix))
	if got, found, err := cold.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || found {
		t.Fatalf("cold node read a deleted snapshot: found=%v version=%d err=%v", found, got.StateVersion, err)
	}

	// 重建（版本接着删除递增）清掉墓碑。
	if err := store.Set(ctx, staleBackfillEnvelope(key, 4, 1, "v4")); err != nil {
		t.Fatal(err)
	}
	if v := b27HashField(t, r, redisKey, "deleted_version"); v != "" {
		t.Fatalf("recreate left deleted_version=%q", v)
	}
	if got, held, err := store.Get(ctx, key); err != nil || !held || got.StateVersion != 4 {
		t.Fatalf("recreated v4: version=%d held=%v err=%v", got.StateVersion, held, err)
	}

	// 旧版本节点（不认 deleted_version）可能在墓碑上写回数据：两种字段并存时，新的删除照样删数据、保留水位。
	if err := store.DeleteAtVersion(ctx, key, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Eval(ctx, `redis.call("HSET", KEYS[1], "marker", "1", "route", "1", "version", "1", "data", "x") return 1`, []string{redisKey}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAtVersion(ctx, key, 5); err != nil {
		t.Fatal(err)
	}
	if v := b27HashField(t, r, redisKey, "data"); v != "" {
		t.Fatalf("mixed-format key kept data %q after the delete", v)
	}
	if v := b27HashField(t, r, redisKey, "deleted_version"); v != "5" {
		t.Fatalf("mixed-format key deleted_version=%q, want 5", v)
	}
}

func mustPrefixedL2(t *testing.T, r remoteSnapshotRedis, prefix string) *remoteSnapshotL2Store {
	t.Helper()
	store, err := NewSnapshotL2StoreWithKeyPrefix(r, time.Minute, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
