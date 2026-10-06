package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260913-01 复核后的补修（2026-10-05，N05）：删除水位必须对共享 L2 的所有写者生效，不只对本机。
//
// U-0187 的墓碑是每个进程 L1 旁的本地表，L2 的 DeleteAtVersion 只是条件 DEL。于是 owner 在 v2 删除之后，
// 另一个节点只要自己还没处理那条删除消息——它的权威加载在删除前读到了 v1、或它先收到了迟到的 v1 复制
// 消息——就会把 v1 写回已经被删空的 L2（CAS 面对的是一个不存在的键）。之后任何 L1 冷的节点都从 L2 读到
// 这个已删除的实体，直到 L2 TTL（默认 5 分钟）或那个节点处理到删除消息；普通 NATS 同步总线最多投递
// 一次，删除消息丢了就一直复活到 TTL。
//
// 承诺：版本化删除在 L2 留下与 L2 快照同 TTL 的墓碑，任何节点写入不新于它的快照都被 L2 判为 stale；
// 更新的快照（重建）照常写入并清掉墓碑；L1 冷的节点读到的是 miss。

// node B 的权威加载在删除之前读到了 v1，返回时 owner 已经在 v2 删除了这个快照。
func TestDeleteWatermarkHoldsInL2AgainstAnInflightLoadOnAnotherNode(t *testing.T) {
	ctx := context.Background()
	redis := newSnapshotRedisFake()
	key := staleBackfillKey(t, staleBackfillLoadKind, 9431)
	owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1201, NewSnapshotL2Store(redis, time.Minute))
	if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	nodeB := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, NewSnapshotL2Store(redis, time.Minute),
		func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 2); err != nil {
				t.Errorf("owner delete: %v", err)
			}
			return staleBackfillEnvelope(key, 1, 1, "v1"), true, nil
		})
	if _, _, err := nodeB.LoadAuthoritative(ctx, key, entity.RemoteReadMonotonic, 0); err != nil {
		t.Fatalf("node B load: %v", err)
	}
	assertDeletedEverywhere(t, ctx, redis, key, 1202)
}

// node B 没有收到删除消息（或还没处理到），先处理了一条迟到的 v1 复制消息。
func TestDeleteWatermarkHoldsInL2AgainstALateReplicaOnAnotherNode(t *testing.T) {
	ctx := context.Background()
	redis := newSnapshotRedisFake()
	key := staleBackfillKey(t, staleBackfillReplicaKind, 9432)
	owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1211, NewSnapshotL2Store(redis, time.Minute))
	nodeB := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1212, NewSnapshotL2Store(redis, time.Minute))
	if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	if err := (SnapshotReplicaStore{client: nodeB.snapshots}).ApplyReplica(ctx, staleBackfillReplica(t, key, 1, 1, "v1")); err != nil {
		t.Fatalf("a late replica is the past, not a failure: %v", err)
	}
	assertDeletedEverywhere(t, ctx, redis, key, 1213)
}

// 对照：删除之后的重建（更新的版本）照常写入、L1 冷的节点读得到；更旧的删除不降低水位；
// 重复删除幂等；删除不会清掉 L2 里更新的快照（原 U-0187 复核行为不变）。
func TestDeleteWatermarkControls(t *testing.T) {
	ctx := context.Background()
	t.Run("recreate above the watermark is visible", func(t *testing.T) {
		redis := newSnapshotRedisFake()
		key := staleBackfillKey(t, staleBackfillReplicaKind, 9433)
		owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1221, NewSnapshotL2Store(redis, time.Minute))
		if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
			t.Fatal(err)
		}
		if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 2); err != nil {
			t.Fatal(err)
		}
		if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 2); err != nil {
			t.Fatalf("repeated delete: %v", err)
		}
		if err := owner.snapshots.cache.DeleteAtVersion(ctx, key, 1); err != nil {
			t.Fatalf("older delete: %v", err)
		}
		// 更旧的删除没有把水位降到 1：v2 的快照仍是“删除之前”。
		other := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1222, NewSnapshotL2Store(redis, time.Minute))
		if err := other.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
			t.Fatal(err)
		}
		if _, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); err != nil || held {
			t.Fatalf("a snapshot at the delete's own version reached L2: held=%v err=%v", held, err)
		}
		recreated := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1223, NewSnapshotL2Store(redis, time.Minute))
		if err := recreated.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 3, 1, "v3")); err != nil {
			t.Fatal(err)
		}
		cold := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1224, NewSnapshotL2Store(redis, time.Minute))
		got, found, err := cold.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
		if err != nil || !found || got.StateVersion != 3 || string(got.Payload.BytesCopy()) != "v3" {
			t.Fatalf("recreated v3 on a cold node: version=%d found=%v err=%v", got.StateVersion, found, err)
		}
		// 重建之后 v3 不受旧墓碑影响：更旧的 v2 仍不能覆盖它，更新的 v4 照常。
		if err := recreated.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 4, 1, "v4")); err != nil {
			t.Fatal(err)
		}
		if stored, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); err != nil || !held || stored.StateVersion != 4 {
			t.Fatalf("after recreate the key moves on: version=%d held=%v err=%v", stored.StateVersion, held, err)
		}
	})
	t.Run("delete never removes a newer L2 snapshot", func(t *testing.T) {
		redis := newSnapshotRedisFake()
		key := staleBackfillKey(t, staleBackfillReplicaKind, 9434)
		owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1231, NewSnapshotL2Store(redis, time.Minute))
		if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 3, 1, "v3")); err != nil {
			t.Fatal(err)
		}
		late := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1232, NewSnapshotL2Store(redis, time.Minute))
		if err := late.snapshots.cache.DeleteAtVersion(ctx, key, 2); err != nil {
			t.Fatal(err)
		}
		if stored, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); err != nil || !held || stored.StateVersion != 3 {
			t.Fatalf("an older delete removed the newer L2 snapshot: version=%d held=%v err=%v", stored.StateVersion, held, err)
		}
	})
}

func assertDeletedEverywhere(t *testing.T, ctx context.Context, redis *snapshotRedisFake, key entity.RemoteSnapshotKey, coldSID int32) {
	t.Helper()
	if stored, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); err != nil || held {
		t.Fatalf("L2 after the delete at v2: held=%v version=%d err=%v — the deleted snapshot was written back", held, stored.StateVersion, err)
	}
	cold := NewManager(newMockVersionedLockFactory(), DefaultConfig(), coldSID, NewSnapshotL2Store(redis, time.Minute))
	for _, consistency := range []entity.RemoteReadConsistency{entity.RemoteReadCached, entity.RemoteReadMonotonic} {
		got, found, err := cold.ReadRemoteSnapshot(ctx, key, consistency, 0)
		if err != nil || found {
			t.Fatalf("cold node, consistency %d: found=%v version=%d err=%v — a snapshot deleted at v2 came back", consistency, found, got.StateVersion, err)
		}
	}
}
