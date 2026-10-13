// 跨节点快照一致性：删除墓碑对共享 L2 生效、L2 拒绝的旧快照不进 L1、L1 条目按陈旧上限与确认状态重新确认并修复 L2。

package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/cache"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
)

// 删除水位对共享 L2 的所有写者生效，不只对本机：版本化删除在 L2 留下与 L2 快照同 TTL 的墓碑，任何节点写入不新于它的快照都被
// L2 判为 stale（包括删除前读到 v1 的权威加载与迟到的 v1 复制消息）；更新的快照（重建）照常写入并清掉墓碑；L1 冷的节点读到 miss。
// 历史：RR-20260913-01。

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

// 对照：删除之后的重建（更新的版本）照常写入、L1 冷的节点读得到；更旧的删除不降低水位；重复删除幂等；删除不会清掉 L2 里更新的快照。
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

// 共享 L2 以版本 CAS 拒绝一份更旧的快照时，本机 L1 不装下这份更旧的快照：被判 stale 的 Set 返回 ErrStaleWrite（cache.Store
// 约定），存下的值至少和这一份一样新——迟到的复制消息或输掉竞争的权威加载不会把 L1 钉在比 L2 旧的版本上。
// kind 251 / 252：本包测试已用的 kind 见同目录各 *_test.go，不能撞号（注册表是进程级的）。
// 历史：RR-20261005-NC-130。

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
	raw, err := json.Marshal(remoteSnapshotWire{Key: key, Update: record, PublishedAt: time.Now().UnixNano()})
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

// 共享 L2 是快照水位的唯一权威：L1 只缓存 L2（或权威加载）确认过的值并带确认时刻；非线性读不交出超过 cached_max_staleness、
// 或从未确认的 L1 条目，而是先重新确认（读 L2，必要时修复 L2 或回源权威）；复制消息带发布时刻，过老的历史快照不被接受。
// kind 253：本包测试已用的 kind 见同目录各 *_test.go（最大 252），不能撞号（注册表是进程级的）。
const b2WatermarkKind entity.EntityKind = 253

type b2Clock struct {
	mu  sync.Mutex
	now time.Time
}

func newB2Clock() *b2Clock { return &b2Clock{now: time.Unix(1_800_000_000, 0)} }

func (c *b2Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *b2Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// b2FaultyRedis 在 Lua 等价替身外加可切换的故障：evalDown 让 CAS / 带版本删除不可达，hgetDown 让读不可达。
type b2FaultyRedis struct {
	*snapshotRedisFake
	evalDown atomic.Bool
	hgetDown atomic.Bool
}

var errB2RedisDown = errors.New("dial tcp 127.0.0.1:6379: connect: connection refused")

func (r *b2FaultyRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if r.evalDown.Load() {
		return nil, errB2RedisDown
	}
	return r.snapshotRedisFake.Eval(ctx, script, keys, args...)
}

func (r *b2FaultyRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	if r.hgetDown.Load() {
		return nil, errB2RedisDown
	}
	return r.snapshotRedisFake.HGet(ctx, key, field)
}

func b2Cache(l2 *remoteSnapshotL2Store, clock *b2Clock, staleness time.Duration, loader entity.RemoteSnapshotLoader) *entity.RemoteSnapshotCache {
	return entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{
		TTL: time.Hour, MaxStaleness: staleness, Now: clock.Now, LoadTimeout: time.Second,
	}, l2, loader)
}

func b2Read(t *testing.T, c *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey) (string, uint64, bool, error) {
	t.Helper()
	got, found, err := c.Get(context.Background(), key, entity.RemoteReadCached, 0)
	return string(got.Payload.BytesCopy()), got.StateVersion, found, err
}

// 超过陈旧上限的 L1 条目先与 L2 重新确认，交出 L2 的较新版本（不只看 L1 TTL）。
func TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2(t *testing.T) {
	ctx := context.Background()
	clock := newB2Clock()
	redis := newSnapshotRedisFake()
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9501)
	reader := b2Cache(l2, clock, 10*time.Second, nil)
	if err := reader.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	// owner 提交了 v2 并写进 L2，这个节点没有收到复制消息（兴趣过期、消息丢失）。
	if err := l2.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(5 * time.Second)
	if payload, _, found, err := b2Read(t, reader, key); err != nil || !found || payload != "v1" {
		t.Fatalf("within the staleness bound the confirmed v1 may be served: payload=%q found=%v err=%v", payload, found, err)
	}
	clock.Advance(6 * time.Second)
	payload, version, found, err := b2Read(t, reader, key)
	if err != nil || !found || version != 2 || payload != "v2" {
		t.Fatalf("Cached read 11s after confirmation (max staleness 10s) returned payload=%q version=%d found=%v err=%v; L2 holds v2", payload, version, found, err)
	}
}

// L2 断网时写入的条目是未确认的：不直接服务；先读 L2，读不到再回源权威；都失败时返回错误，不交出旧值。
func TestB2EntryWrittenDuringL2OutageIsNotServedUnconfirmed(t *testing.T) {
	ctx := context.Background()
	clock := newB2Clock()
	redis := &b2FaultyRedis{snapshotRedisFake: newSnapshotRedisFake()}
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9502)
	var authorityMu sync.Mutex
	var authority any = errB2RedisDown // entity.RemoteSnapshotEnvelope, or error
	setAuthority := func(v any) { authorityMu.Lock(); authority = v; authorityMu.Unlock() }
	loads := atomic.Int32{}
	reader := b2Cache(l2, clock, 10*time.Second, func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		loads.Add(1)
		authorityMu.Lock()
		current := authority
		authorityMu.Unlock()
		switch v := current.(type) {
		case entity.RemoteSnapshotEnvelope:
			return v, true, nil
		case error:
			return entity.RemoteSnapshotEnvelope{}, false, v
		}
		return entity.RemoteSnapshotEnvelope{}, false, nil
	})

	redis.evalDown.Store(true)
	if err := reader.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatalf("an L2 outage must not fail the write: %v", err)
	}
	// L2 恢复，期间另一个节点已经把 v2 写进 L2。
	redis.evalDown.Store(false)
	if err := NewSnapshotL2Store(redis.snapshotRedisFake, time.Hour).Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatal(err)
	}
	payload, version, found, err := b2Read(t, reader, key)
	if err != nil || !found || version != 2 || payload != "v2" {
		t.Fatalf("an entry written while L2 was unreachable was served unconfirmed: payload=%q version=%d found=%v err=%v; L2 holds v2", payload, version, found, err)
	}

	// L2 完全不可达：未确认 / 过期条目改为回源权威，权威的结果可以直接服务。
	key3 := staleBackfillKey(t, b2WatermarkKind, 9503)
	redis.evalDown.Store(true)
	redis.hgetDown.Store(true)
	if err := reader.Publish(ctx, staleBackfillEnvelope(key3, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if payload, _, found, err := b2Read(t, reader, key3); err == nil && found && payload == "v1" {
		t.Fatalf("with L2 and the authority both unreachable, the unconfirmed v1 was served")
	}
	setAuthority(staleBackfillEnvelope(key3, 3, 1, "v3"))
	before := loads.Load()
	payload, version, found, err = b2Read(t, reader, key3)
	if err != nil || !found || version != 3 || payload != "v3" {
		t.Fatalf("L2 unreachable: Cached read must fall back to the authority, got payload=%q version=%d found=%v err=%v", payload, version, found, err)
	}
	// 权威的结果在陈旧上限内直接服务，不再回源。
	clock.Advance(5 * time.Second)
	if payload, _, found, err := b2Read(t, reader, key3); err != nil || !found || payload != "v3" {
		t.Fatalf("the authority's answer within the bound: payload=%q found=%v err=%v", payload, found, err)
	}
	if got := loads.Load() - before; got != 1 {
		t.Fatalf("authority loads during the outage = %d, want 1 per key per staleness window", got)
	}
}

// 发布者的 L2 CAS 结果未知 / 失败后，未确认的条目在下一次读取时把自己补进 L2（版本化 CAS 重发是幂等的）。
func TestB2PublisherRepairsALostL2Write(t *testing.T) {
	ctx := context.Background()
	clock := newB2Clock()
	redis := &b2FaultyRedis{snapshotRedisFake: newSnapshotRedisFake()}
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9504)
	owner := b2Cache(l2, clock, 10*time.Second, nil)
	if err := owner.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	redis.evalDown.Store(true)
	if err := owner.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatalf("an L2 outage must not fail the publish: %v", err)
	}
	redis.evalDown.Store(false)
	if payload, _, found, err := b2Read(t, owner, key); err != nil || !found || payload != "v2" {
		t.Fatalf("owner read after recovery: payload=%q found=%v err=%v", payload, found, err)
	}
	stored, held, err := l2.Get(ctx, key)
	if err != nil || !held || stored.StateVersion != 2 {
		t.Fatalf("L2 after the owner's next read holds version=%d held=%v err=%v; the lost v2 write was never repaired", stored.StateVersion, held, err)
	}
}

// 带版本删除的 L2 结果未知时，删除在 L1 留未确认的删除标记，下一次读取重发带版本删除（幂等），之后 L2 不再持有它。
func TestB2LostL2DeleteIsRepairedByTheNextRead(t *testing.T) {
	ctx := context.Background()
	clock := newB2Clock()
	redis := &b2FaultyRedis{snapshotRedisFake: newSnapshotRedisFake()}
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9505)
	owner := b2Cache(l2, clock, 10*time.Second, nil)
	if err := owner.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	redis.evalDown.Store(true)
	if err := owner.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatalf("an L2 outage must not fail the delete: %v", err)
	}
	redis.evalDown.Store(false)
	if _, _, found, err := b2Read(t, owner, key); found || err != nil {
		t.Fatalf("owner read after its own delete: found=%v err=%v", found, err)
	}
	if stored, held, err := l2.Get(ctx, key); err != nil || held {
		t.Fatalf("L2 still serves the deleted snapshot version=%d held=%v err=%v; the lost delete was never repaired", stored.StateVersion, held, err)
	}
	// 冷节点同样读不到。
	cold := b2Cache(l2, clock, 10*time.Second, nil)
	if _, _, found, err := b2Read(t, cold, key); found || err != nil {
		t.Fatalf("cold node after the repaired delete: found=%v err=%v", found, err)
	}
}

// b2ReplicaWire 用 map 组 JSON，直接控制 published_at 字段是否出现。
func b2ReplicaWire(t *testing.T, key entity.RemoteSnapshotKey, version uint64, payload string, publishedAt int64, routeEpoch uint64) mirror.Envelope {
	t.Helper()
	data := []byte(payload)
	record := entity.RemoteSnapshotRecord{
		Key: key, StateVersion: version, BaseVersion: version - 1, MarkerEpoch: 1, RouteEpoch: routeEpoch,
		Schema: 1, Codec: 1, Full: true, Data: data, Checksum: entity.RemoteSnapshotChecksum(data),
	}
	wire := map[string]any{"key": key, "update": record}
	if publishedAt != 0 {
		wire["published_at"] = publishedAt
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

// DeliverAll 重放的历史快照比 L2 能担保的窗口（snapshot_l2_ttl / 2）更老时不被接受；新消息照常生效；没有发布时刻的
// 更新无法判断新旧，按坏消息拒绝（不兼容旧发布者）。
func TestB2HistoricReplicaPastL2MemoryIsNotAdmitted(t *testing.T) {
	ctx := context.Background()
	redis := newSnapshotRedisFake()
	cfg := DefaultConfig()
	cfg.SnapshotL2TTL = time.Minute
	reader := NewManager(newMockVersionedLockFactory(), cfg, 1501, NewSnapshotL2Store(redis, cfg.SnapshotL2TTL))
	store := SnapshotReplicaStore{client: reader.snapshots}
	key := staleBackfillKey(t, b2WatermarkKind, 9506)

	historic := b2ReplicaWire(t, key, 1, "v1-from-history", time.Now().Add(-10*time.Minute).UnixNano(), 1)
	if err := store.ApplyReplica(ctx, historic); err != nil {
		t.Fatalf("a historic replica is dropped, not a failure: %v", err)
	}
	if got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || found {
		t.Fatalf("a replica published 10m ago (L2 TTL 1m) was admitted: payload=%q found=%v err=%v", got.Payload.BytesCopy(), found, err)
	}
	if stored, held, err := NewSnapshotL2Store(redis, time.Minute).Get(ctx, key); err != nil || held {
		t.Fatalf("the historic replica reached the shared L2: version=%d held=%v err=%v", stored.StateVersion, held, err)
	}

	// 对照：刚发布的消息照常生效。
	if err := store.ApplyReplica(ctx, b2ReplicaWire(t, key, 2, "v2-live", time.Now().UnixNano(), 1)); err != nil {
		t.Fatal(err)
	}
	if got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || !found || string(got.Payload.BytesCopy()) != "v2-live" {
		t.Fatalf("a live replica: payload=%q found=%v err=%v", got.Payload.BytesCopy(), found, err)
	}
	if err := store.ApplyReplica(ctx, b2ReplicaWire(t, key, 3, "v3-no-published-at", 0, 1)); err == nil {
		t.Fatal("an update without published_at was accepted; it cannot be told apart from a replay older than the L2 window")
	}
	if got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || !found || string(got.Payload.BytesCopy()) != "v2-live" {
		t.Fatalf("a refused update without published_at replaced v2-live: payload=%q found=%v err=%v", got.Payload.BytesCopy(), found, err)
	}
}

// 并发组合：两个节点共用一个 L2，四个写者乱序发布 v1～v60（经 Publish 与复制两条入口），另有读者以 1ns 的
// 陈旧上限反复读取，中途一次带版本删除 @30。全部写入后显式跨过陈旧上限，再验两个节点与 L2 都停在 v60：L1 从不
// 超过 L2 已确认的版本，也不会因乱序把 L2 拉回旧版本。
func TestB2ConcurrentWritersAndReconfirmingReadersConvergeOnL2(t *testing.T) {
	ctx := context.Background()
	clock := newB2Clock()
	redis := newSnapshotRedisFake()
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9510)
	newNode := func() *entity.RemoteSnapshotCache {
		return entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Hour, MaxStaleness: time.Nanosecond, LoadTimeout: time.Second, Now: clock.Now}, l2, nil)
	}
	nodes := []*entity.RemoteSnapshotCache{newNode(), newNode()}
	const top = 60
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for v := top - w; v >= 1; v -= 4 {
				node := nodes[v%2]
				var err error
				if v%3 == 0 {
					err = node.ApplyUpdate(ctx, entity.RemoteSnapshotRecord{
						Key: key, StateVersion: uint64(v), BaseVersion: uint64(v - 1), MarkerEpoch: 1, RouteEpoch: 1,
						Schema: 1, Codec: 1, Full: true, Data: []byte(fmt.Sprintf("v%d", v)),
					})
				} else {
					err = node.Publish(ctx, staleBackfillEnvelope(key, uint64(v), 1, fmt.Sprintf("v%d", v)))
				}
				if err != nil {
					t.Errorf("write v%d: %v", v, err)
				}
				if v == 30 {
					if err := nodes[1].DeleteAtVersion(ctx, key, 30); err != nil {
						t.Errorf("delete: %v", err)
					}
				}
			}
		}(w)
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func(node *entity.RemoteSnapshotCache) {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				clock.Advance(2 * time.Nanosecond)
				if _, _, err := node.Get(ctx, key, entity.RemoteReadCached, 0); err != nil && !errors.Is(err, entity.ErrRemoteSnapshotStale) {
					t.Errorf("read: %v", err)
					return
				}
			}
		}(nodes[r])
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	stored, held, err := l2.Get(ctx, key)
	if err != nil || !held || stored.StateVersion != top {
		t.Fatalf("L2 after the race: version=%d held=%v err=%v, want v%d", stored.StateVersion, held, err, top)
	}
	// RR-20261008-41：等待写者/读者结束后再推进，最终读取确定超过最近确认时间。
	clock.Advance(2 * time.Nanosecond)
	for i, node := range nodes {
		got, found, err := node.Get(ctx, key, entity.RemoteReadCached, 0)
		if err != nil || !found || got.StateVersion != top {
			t.Fatalf("node %d after the race: version=%d found=%v err=%v, want v%d", i, got.StateVersion, found, err, top)
		}
	}
}
