package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
)

// RR-20261005-NC-130：共享 L2 以版本 CAS 拒绝一份更旧的快照时，本机 L1 不能装下这份更旧的快照。
//
// RemoteSnapshotCache.Publish 的约定是“输给更新的快照是预期结果：存下的值至少和这一份一样新”。
// 但 remoteSnapshotL2Store.Set 在 CAS 落败（L2 已有更新版本或更新的 epoch）时返回 nil，
// cache.ReadThroughStore.Set 于是照常写 L1：L1 冷的节点收到一条迟到的复制消息、或权威加载
// 返回时 owner 已经提交了更新版本，这个节点的 L1 就停在比共享 L2 更旧的版本上，之后的 Cached /
// Monotonic(minVersion 不高于旧版本) 读取在 L1 TTL 内一直返回它，而 L2 与权威早已更新。
// 这同时违反 cache.Store 的约定：被判 stale 拒绝的 Set 必须返回 ErrStaleWrite（RR-20261004-NC-15）。
//
// kind 251 / 252：本包测试已用的 kind 见同目录各 *_test.go，不能撞号（注册表是进程级的）。

const (
	staleBackfillReplicaKind entity.EntityKind = 251
	staleBackfillLoadKind    entity.EntityKind = 252
)

func staleBackfillKey(t *testing.T, kind entity.EntityKind, unique int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(unique, kind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func staleBackfillEnvelope(key entity.RemoteSnapshotKey, version, routeEpoch uint64, payload string) entity.RemoteSnapshotEnvelope {
	data := []byte(payload)
	return entity.RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: routeEpoch,
		Schema: 1, Codec: 1, Full: true, Checksum: entity.RemoteSnapshotChecksum(data),
		Payload: entity.CopyFrozenRemoteSnapshotPayload(data),
	}
}

func staleBackfillReplica(t *testing.T, key entity.RemoteSnapshotKey, version, routeEpoch uint64, payload string) mirror.Envelope {
	t.Helper()
	data := []byte(payload)
	record := entity.RemoteSnapshotRecord{
		Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: routeEpoch,
		Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data),
	}
	raw, err := json.Marshal(remoteSnapshotWire{Key: key, Update: record})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

// failingEvalRedis 让 L2 写入不可达（断网），用于对照：L2 故障仍按既有策略降级为只写 L1。
type failingEvalRedis struct{ *snapshotRedisFake }

func (failingEvalRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return nil, errors.New("redis: connection refused")
}

// 两个节点共用一个 L2：owner 已把 v2 写进 L2，L1 冷的另一个节点随后收到 v1 的迟到复制消息。
func TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name               string
		newerRoute         uint64 // L2 里更新的那份的 route epoch
		newerVersion       uint64
		lateRoute          uint64
		lateVersion        uint64
		wantVersion        uint64
		wantRoute          uint64
		wantPayloadAfterRd string
	}{
		{name: "same epoch, newer version in L2", newerRoute: 1, newerVersion: 2, lateRoute: 1, lateVersion: 1, wantVersion: 2, wantRoute: 1, wantPayloadAfterRd: "v2"},
		{name: "takeover: newer route epoch in L2", newerRoute: 2, newerVersion: 6, lateRoute: 1, lateVersion: 5, wantVersion: 6, wantRoute: 2, wantPayloadAfterRd: "v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redis := newSnapshotRedisFake()
			key := staleBackfillKey(t, staleBackfillReplicaKind, 9401+int64(tc.newerRoute))
			owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1001, NewSnapshotL2Store(redis, time.Minute))
			reader := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1002, NewSnapshotL2Store(redis, time.Minute))
			if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, tc.newerVersion, tc.newerRoute, "v2")); err != nil {
				t.Fatal(err)
			}

			late := staleBackfillReplica(t, key, tc.lateVersion, tc.lateRoute, "v1")
			if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, late); err != nil {
				t.Fatalf("a late replica is the past, not a failure: %v", err)
			}
			for _, consistency := range []entity.RemoteReadConsistency{entity.RemoteReadCached, entity.RemoteReadMonotonic} {
				got, found, err := reader.ReadRemoteSnapshot(ctx, key, consistency, 0)
				if err != nil || !found {
					t.Fatalf("consistency %d: found=%v err=%v", consistency, found, err)
				}
				if got.StateVersion != tc.wantVersion || got.RouteEpoch != tc.wantRoute || string(got.Payload.BytesCopy()) != tc.wantPayloadAfterRd {
					t.Fatalf("consistency %d read version=%d route=%d payload=%q after a late replica; L2 holds version=%d route=%d — L1 was pinned below the shared layer",
						consistency, got.StateVersion, got.RouteEpoch, got.Payload.BytesCopy(), tc.wantVersion, tc.wantRoute)
				}
			}
			stored, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key)
			if err != nil || !held || stored.StateVersion != tc.newerVersion {
				t.Fatalf("L2 must keep the newer snapshot: version=%d held=%v err=%v", stored.StateVersion, held, err)
			}

			// 恢复：更新的复制消息照常生效。
			next := staleBackfillReplica(t, key, tc.newerVersion+1, tc.newerRoute, "v3")
			if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, next); err != nil {
				t.Fatal(err)
			}
			got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0)
			if err != nil || !found || got.StateVersion != tc.newerVersion+1 || string(got.Payload.BytesCopy()) != "v3" {
				t.Fatalf("a newer replica after the refusal: version=%d found=%v err=%v", got.StateVersion, found, err)
			}
		})
	}
}

// 权威加载与 owner 的提交交错：加载读到 v1 的同时 owner 已把 v2 写进 L2。加载结果不能把本机 L1 钉在 v1，
// 读取交出的至少是 L2 已有的版本。
func TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot(t *testing.T) {
	ctx := context.Background()
	redis := newSnapshotRedisFake()
	key := staleBackfillKey(t, staleBackfillLoadKind, 9411)
	ownerL2 := NewSnapshotL2Store(redis, time.Minute)
	loads := 0
	cacheUnderTest := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, NewSnapshotL2Store(redis, time.Minute),
		func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
			loads++
			// owner 在这次加载进行中提交了 v2，并写进共享 L2。
			if err := ownerL2.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
				t.Errorf("owner publish: %v", err)
			}
			return staleBackfillEnvelope(key, 1, 1, "v1"), true, nil
		})

	got, found, err := cacheUnderTest.Get(ctx, key, entity.RemoteReadMonotonic, 1)
	if err != nil || !found {
		t.Fatalf("monotonic read: found=%v err=%v", found, err)
	}
	if loads != 1 {
		t.Fatalf("loads=%d, want the one authoritative load", loads)
	}
	if got.StateVersion != 2 || string(got.Payload.BytesCopy()) != "v2" {
		t.Fatalf("monotonic read returned version=%d payload=%q; L2 already held version 2", got.StateVersion, got.Payload.BytesCopy())
	}
	cached, found, err := cacheUnderTest.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !found || cached.StateVersion != 2 {
		t.Fatalf("the next cached read: version=%d found=%v err=%v — L1 kept the load's older answer", cached.StateVersion, found, err)
	}
}

// 对照：L2 不可达仍是“降级到只写 L1”，迟到的复制消息照常装进 L1、复制不失败；B2 之后这份条目是未确认的，
// Cached 读要先确认（L2 没有值、读节点没有权威时就是未找到）。同一份快照的重复发布仍被接受。
func TestStaleBackfillControls(t *testing.T) {
	ctx := context.Background()
	t.Run("L2 outage still degrades to L1", func(t *testing.T) {
		key := staleBackfillKey(t, staleBackfillReplicaKind, 9421)
		reader := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1003, NewSnapshotL2Store(failingEvalRedis{newSnapshotRedisFake()}, time.Minute))
		if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, staleBackfillReplica(t, key, 1, 1, "v1")); err != nil {
			t.Fatalf("an L2 outage must not fail replication: %v", err)
		}
		// WaitForVersion 只看 L1 是否持有该版本（不论是否确认）：已持有时立即返回。
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		if err := reader.snapshots.cache.WaitForVersion(waitCtx, key, 1); err != nil {
			t.Fatalf("L1 after an L2 outage does not hold v1: %v", err)
		}
		if got, found, _ := reader.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0); found {
			t.Fatalf("an unconfirmed entry was served: version=%d", got.StateVersion)
		}
	})
	t.Run("identical republish is accepted", func(t *testing.T) {
		redis := newSnapshotRedisFake()
		key := staleBackfillKey(t, staleBackfillReplicaKind, 9422)
		owner := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1004, NewSnapshotL2Store(redis, time.Minute))
		reader := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1005, NewSnapshotL2Store(redis, time.Minute))
		if err := owner.snapshots.cache.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
			t.Fatal(err)
		}
		if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, staleBackfillReplica(t, key, 2, 1, "v2")); err != nil {
			t.Fatalf("the same snapshot again: %v", err)
		}
		got, found, err := reader.snapshots.cache.Get(ctx, key, entity.RemoteReadCached, 0)
		if err != nil || !found || got.StateVersion != 2 {
			t.Fatalf("after an identical republish: version=%d found=%v err=%v", got.StateVersion, found, err)
		}
	})
	t.Run("L2 store reports a refused older write as ErrStaleWrite", func(t *testing.T) {
		key := staleBackfillKey(t, staleBackfillReplicaKind, 9423)
		store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
		if err := store.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
			t.Fatal(err)
		}
		if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); !errors.Is(err, cache.ErrStaleWrite) {
			t.Fatalf("older write = %v, want cache.ErrStaleWrite (cache.Store contract)", err)
		}
		stored, held, err := store.Get(ctx, key)
		if err != nil || !held || stored.StateVersion != 2 {
			t.Fatalf("refused write changed L2: version=%d held=%v err=%v", stored.StateVersion, held, err)
		}
	})
}
