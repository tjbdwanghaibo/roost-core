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

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// B2（维护者决定 2026-10-05 第三轮，docs/feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md）：
// 共享 L2 是快照水位的唯一权威，L1 只缓存 L2（或权威加载）确认过的值并带确认时刻；非线性读不交出
// 超过 cached_max_staleness、或从未确认的 L1 条目，而是先重新确认（读 L2，必要时修复 L2 或回源权威）。
//
// 修前的缺口（每条用例开头写明）：L1 只看自己的 TTL，L2 断网时写入的条目与 L2 写丢之后的条目都被当成
// 可信值一直交出；L2 的删除 / 更新结果未知后，本机不会再补；复制消息没有“多老”的概念，DeliverAll 重放的
// 历史快照在 L2 过期后会被 CAS 接受（N05 O5）。
//
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

// 修前：Cached 读只看 L1 TTL（这里 1h），L1 在 L2 已更新之后继续交出旧版本，直到 L1 过期。
// 承诺：超过陈旧上限的条目先与 L2 重新确认，交出 L2 的较新版本。
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

// 修前：L2 断网时写入的条目与正常确认的条目没有区别，L2 恢复后仍交出比 L2 旧的版本；L2 与 L1 都回答
// 不了时 Cached 读不回源，交出未经确认的旧值。
// 承诺：未确认的条目不直接服务；先读 L2，读不到再回源权威；都失败时返回错误，不交出旧值。
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

// 修前：发布者的 L2 CAS 结果未知 / 失败后，L2 一直停在旧版本（直到 L2 TTL），本机也不会补。
// 承诺：未确认的条目在下一次读取时把自己补进 L2（版本化 CAS 重发是幂等的）。
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

// 修前（A2“未完成”）：带版本删除的 L2 结果被 `_ =` 吞掉；L2 继续持有被删的快照，冷节点读到它直到 L2 TTL，
// 发布者自己也不会补。
// 承诺：删除在 L1 留未确认的删除标记，下一次读取重发带版本删除（幂等），之后 L2 不再持有它。
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

// b2ReplicaWire 用 map 组 JSON，这样在没有 published_at 字段的修前代码上也能编译、拿到红。
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

// 修前（N05 O5）：DeliverAll 重放的历史快照在 L2 已过期（键在 snapshot_l2_ttl 内没有写入）时被 CAS 接受，
// 旧版本同时写进 L2 与 L1，所有冷节点读到它。
// 承诺：比 L2 能担保的窗口（snapshot_l2_ttl / 2）更老的快照更新不被接受；新消息与没有发布时刻的旧格式消息照旧。
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

	// 对照：刚发布的消息、以及旧发布者不带发布时刻的消息照常生效。
	if err := store.ApplyReplica(ctx, b2ReplicaWire(t, key, 2, "v2-live", time.Now().UnixNano(), 1)); err != nil {
		t.Fatal(err)
	}
	if got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || !found || string(got.Payload.BytesCopy()) != "v2-live" {
		t.Fatalf("a live replica: payload=%q found=%v err=%v", got.Payload.BytesCopy(), found, err)
	}
	if err := store.ApplyReplica(ctx, b2ReplicaWire(t, key, 3, "v3-legacy", 0, 1)); err != nil {
		t.Fatal(err)
	}
	if got, found, err := reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || !found || string(got.Payload.BytesCopy()) != "v3-legacy" {
		t.Fatalf("a legacy replica without published_at: payload=%q found=%v err=%v", got.Payload.BytesCopy(), found, err)
	}
}

// 并发组合：两个节点共用一个 L2，四个写者乱序发布 v1～v60（经 Publish 与复制两条入口），另有读者以 1ns 的
// 陈旧上限反复读取（每次都走重新确认），中途一次带版本删除 @30。收敛后两个节点与 L2 都停在 v60：L1 从不
// 超过 L2 已确认的版本，也不会因乱序把 L2 拉回旧版本。
func TestB2ConcurrentWritersAndReconfirmingReadersConvergeOnL2(t *testing.T) {
	ctx := context.Background()
	redis := newSnapshotRedisFake()
	l2 := NewSnapshotL2Store(redis, time.Hour)
	key := staleBackfillKey(t, b2WatermarkKind, 9510)
	newNode := func() *entity.RemoteSnapshotCache {
		return entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Hour, MaxStaleness: time.Nanosecond, LoadTimeout: time.Second}, l2, nil)
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
	for i, node := range nodes {
		got, found, err := node.Get(ctx, key, entity.RemoteReadCached, 0)
		if err != nil || !found || got.StateVersion != top {
			t.Fatalf("node %d after the race: version=%d found=%v err=%v, want v%d", i, got.StateVersion, found, err, top)
		}
	}
}
