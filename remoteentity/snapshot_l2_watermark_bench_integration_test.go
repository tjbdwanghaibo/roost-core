//go:build integration

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// B2 的性能代价（真实 Redis，单机）。每次 L1 写的 L2 往返数在 B2 前后不变（一次 CAS）；B2 去掉了 L1 冷时的
// HGET 预查，只在冷路径（CAS 被拒、重新确认、修复）多一次 HGET。四个基准：
//
//   - PublishWarm：owner 连续发布同一个 key 的新版本（L1 热）。
//   - ReplicaCold：读节点收到一个 L1 里没有的 key 的复制消息。
//   - CachedHit：已确认、在陈旧上限内的 Cached 读（不碰 Redis）。
//   - CachedReconfirm：陈旧上限 1ns，每次 Cached 读都重新确认（一次 HGET）。B2 之前没有对应路径。
//
// 运行（前后各一次，benchstat 对比）：
//
//	go test -tags integration -run '^$' -bench 'BenchmarkRealB2' -benchtime 3000x -count 6 ./remoteentity
func benchB2Setup(b *testing.B) (context.Context, *remoteSnapshotL2Store, func(uint64) entity.RemoteSnapshotKey) {
	b.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		b.Skip("set ROOST_DATAENGINE_IT=1 with isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	r := client
	prefix := fmt.Sprintf("b2l2bench:%d:%d", os.Getpid(), time.Now().UnixNano())
	store, err := NewSnapshotL2StoreWithKeyPrefix(r, time.Minute, prefix)
	if err != nil {
		b.Fatal(err)
	}
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: b2WatermarkMatrixKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	var keys []string
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for start := 0; start < len(keys); start += 500 {
			end := min(start+500, len(keys))
			_, _ = r.Del(ctx, keys[start:end]...)
		}
	})
	keyOf := func(unique uint64) entity.RemoteSnapshotKey {
		id, err := entity.BuildEntityID(int64(unique), b2WatermarkMatrixKind)
		if err != nil {
			b.Fatal(err)
		}
		key := entity.RemoteSnapshotKey{EntityID: id, Kind: b2WatermarkMatrixKind, Scope: 1}
		keys = append(keys, prefix+":"+remoteSnapshotL2Key(key))
		return key
	}
	return context.Background(), store, keyOf
}

func BenchmarkRealB2PublishWarm(b *testing.B) {
	ctx, store, keyOf := benchB2Setup(b)
	c := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, store, nil)
	key := keyOf(1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Publish(ctx, staleBackfillEnvelope(key, uint64(i+1), 1, "payload-payload-payload")); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRealB2ReplicaCold(b *testing.B) {
	ctx, store, keyOf := benchB2Setup(b)
	owner := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, store, nil)
	reader := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, store, nil)
	envelopes := make([]entity.RemoteSnapshotEnvelope, b.N)
	for i := range envelopes {
		envelopes[i] = staleBackfillEnvelope(keyOf(uint64(100000+i)), 1, 1, "payload-payload-payload")
		if err := owner.Publish(ctx, envelopes[i]); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := reader.Publish(ctx, envelopes[i]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRealB2CachedHit(b *testing.B) {
	ctx, store, keyOf := benchB2Setup(b)
	c := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute}, store, nil)
	key := keyOf(2)
	if err := c.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "payload-payload-payload")); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := c.Get(ctx, key, entity.RemoteReadCached, 0); err != nil || !ok {
			b.Fatalf("ok=%v err=%v", ok, err)
		}
	}
}

func BenchmarkRealB2CachedReconfirm(b *testing.B) {
	ctx, store, keyOf := benchB2Setup(b)
	c := entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{TTL: time.Minute, MaxStaleness: time.Nanosecond}, store, nil)
	key := keyOf(3)
	if err := c.Publish(ctx, staleBackfillEnvelope(key, 1, 1, "payload-payload-payload")); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := c.Get(ctx, key, entity.RemoteReadCached, 0); err != nil || !ok {
			b.Fatalf("ok=%v err=%v", ok, err)
		}
	}
}
