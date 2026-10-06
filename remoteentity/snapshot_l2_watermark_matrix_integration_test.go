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

// B2 组合矩阵：快照水位修复链里每一类旧缺陷，在真实 Redis（单机）与自建 Redis Cluster（3 主 3 从）上，
// 对每个写入来源、L1 冷 / 热各跑一遍。L2 脚本由真实 Redis 执行。
//
//	场景（缺陷链）                      | 更新的事实（owner 写在 L2）     | 迟到的旧输入
//	old-over-new（RR-05/06、NC-130）     | v2                             | v1
//	delete-resurrect（RR-01 及两轮残余） | 删除 @2（L2 墓碑）              | v1
//	migration-old-epoch（NC-130 子例）   | route epoch 2 / v6             | route epoch 1 / v7
//	deliverall-replay（N05 O5）          | 权威 v2，L2 已过期（键被清掉）   | 10 分钟前发布的 v1 复制消息
//
// 来源：publish（本机 Publish）、replica（SnapshotReplicaStore.ApplyReplica）、load（权威加载返回旧值，
// Linearizable 触发）。L2 回填与删除交错（在途读取）需要暂停 HGET，只在单元测试里覆盖
// （entity/snapshot_delete_l2_promises_test.go）。热 = 读节点 L1 在更新的事实发生前已确认持有旧值。
//
// 断言：(a) L2 从不持有旧输入；(b) 冷节点立刻读到更新的事实；(c) 读节点在陈旧上限（300ms）之后读到更新的
// 事实（上限之内交出已确认的旧值是契约允许的）。
//
// 准入：单机 ROOST_DATAENGINE_IT=1；Cluster 另需 ROOST_REMOTE_CLUSTER_IT=1（端口 17380～17385 自建、
// 用完即杀）。键用部署前缀 b2l2:<pid>:<ns>:<backend>，结束时逐键删除。单独运行：
//
//	go test -tags integration -run '^TestRealB2WatermarkMatrix' ./remoteentity
//
// kind 254：本包测试已用的 kind 见同目录各 *_test.go（b2WatermarkKind 253），不能撞号。
const b2WatermarkMatrixKind entity.EntityKind = 254

const b2MatrixStaleness = 300 * time.Millisecond

func TestRealB2WatermarkMatrixStandalone(t *testing.T) {
	runB2WatermarkMatrix(t, "standalone", realRemoteRedis(t))
}

func TestRealB2WatermarkMatrixCluster(t *testing.T) {
	if os.Getenv("ROOST_REMOTE_CLUSTER_IT") != "1" {
		t.Skip("set ROOST_REMOTE_CLUSTER_IT=1")
	}
	_, addresses := newRemoteTestCluster(t)
	cfg := fredis.DefaultConfig("")
	cfg.ClusterAddrs = addresses
	client := redisdriver.NewRedisClient(cfg)
	t.Cleanup(func() { _ = client.Close() })
	runB2WatermarkMatrix(t, "cluster", client)
}

type b2MatrixScenario struct {
	name string
	// establish 让 owner 在 L2 写下更新的事实；返回旧输入。
	newer func(t *testing.T, ctx context.Context, owner *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey, redisKey string, r fredis.IRedis) (stale entity.RemoteSnapshotEnvelope, publishedAt int64)
	// check 断言读到的是更新的事实。
	want    func(got entity.RemoteSnapshotEnvelope, found bool) bool
	sources []string
}

func runB2WatermarkMatrix(t *testing.T, backend string, r fredis.IRedis) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prefix := fmt.Sprintf("b2l2:%d:%d:%s", os.Getpid(), time.Now().UnixNano(), backend)
	cfg := DefaultConfig()
	cfg.CachedMaxStaleness = b2MatrixStaleness
	cfg.SnapshotL2TTL = time.Minute

	scenarios := []b2MatrixScenario{
		{
			name: "old-over-new",
			newer: func(t *testing.T, ctx context.Context, owner *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey, _ string, _ fredis.IRedis) (entity.RemoteSnapshotEnvelope, int64) {
				if err := owner.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
					t.Fatal(err)
				}
				return staleBackfillEnvelope(key, 1, 1, "v1"), time.Now().UnixNano()
			},
			want:    func(got entity.RemoteSnapshotEnvelope, found bool) bool { return found && got.StateVersion == 2 },
			sources: []string{"publish", "replica", "load"},
		},
		{
			name: "delete-resurrect",
			newer: func(t *testing.T, ctx context.Context, owner *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey, _ string, _ fredis.IRedis) (entity.RemoteSnapshotEnvelope, int64) {
				if err := owner.DeleteAtVersion(ctx, key, 2); err != nil {
					t.Fatal(err)
				}
				return staleBackfillEnvelope(key, 1, 1, "v1"), time.Now().UnixNano()
			},
			want:    func(_ entity.RemoteSnapshotEnvelope, found bool) bool { return !found },
			sources: []string{"publish", "replica", "load"},
		},
		{
			name: "migration-old-epoch",
			newer: func(t *testing.T, ctx context.Context, owner *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey, _ string, _ fredis.IRedis) (entity.RemoteSnapshotEnvelope, int64) {
				if err := owner.Publish(ctx, staleBackfillEnvelope(key, 6, 2, "route2-v6")); err != nil {
					t.Fatal(err)
				}
				return staleBackfillEnvelope(key, 7, 1, "route1-v7"), time.Now().UnixNano()
			},
			want: func(got entity.RemoteSnapshotEnvelope, found bool) bool {
				return found && got.RouteEpoch == 2 && got.StateVersion == 6
			},
			sources: []string{"publish", "replica", "load"},
		},
		{
			name: "deliverall-replay",
			newer: func(t *testing.T, ctx context.Context, owner *entity.RemoteSnapshotCache, key entity.RemoteSnapshotKey, redisKey string, r fredis.IRedis) (entity.RemoteSnapshotEnvelope, int64) {
				if err := owner.Publish(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
					t.Fatal(err)
				}
				// 键在 snapshot_l2_ttl 内没有写入：L2 已过期。直接删键模拟过期，不等真实 TTL。
				if _, err := r.Del(ctx, redisKey); err != nil {
					t.Fatal(err)
				}
				return staleBackfillEnvelope(key, 1, 1, "v1"), time.Now().Add(-10 * time.Minute).UnixNano()
			},
			// Cached 不因 miss 回源：L2 已空、权威（读节点没有 backend）不可答时是“未找到”，绝不能是重放的 v1。
			want:    func(got entity.RemoteSnapshotEnvelope, found bool) bool { return !found || got.StateVersion == 2 },
			sources: []string{"replica"},
		},
	}

	unique := int64(9700)
	for _, sc := range scenarios {
		for _, source := range sc.sources {
			for _, hot := range []bool{false, true} {
				unique++
				name := fmt.Sprintf("%s/%s/%s", sc.name, source, map[bool]string{false: "cold", true: "hot"}[hot])
				t.Run(name, func(t *testing.T) {
					key := staleBackfillKey(t, b2WatermarkMatrixKind, unique)
					redisKey := prefix + ":" + remoteSnapshotL2Key(key)
					b27DeleteKeys(t, r, redisKey)
					newL2 := func() *remoteSnapshotL2Store { return mustPrefixedL2Store(t, r, prefix, cfg.SnapshotL2TTL) }
					owner := NewManager(newMockVersionedLockFactory(), cfg, 2201, newL2())

					// 读节点：load 来源用带 loader 的缓存（loader 返回旧输入），其余用 Manager。
					var staleForLoader entity.RemoteSnapshotEnvelope
					var reader *Manager
					var readerCache *entity.RemoteSnapshotCache
					if source == "load" {
						readerCache = entity.NewRemoteSnapshotCache(entity.RemoteSnapshotCacheConfig{
							TTL: cfg.SnapshotCacheTTL, MaxStaleness: b2MatrixStaleness, LoadTimeout: time.Second,
						}, newL2(), func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
							if staleForLoader.StateVersion == 0 {
								return entity.RemoteSnapshotEnvelope{}, false, nil
							}
							return staleForLoader, true, nil
						})
					} else {
						reader = NewManager(newMockVersionedLockFactory(), cfg, 2202, newL2())
						readerCache = reader.snapshots.cache
					}
					read := func() (entity.RemoteSnapshotEnvelope, bool, error) {
						if reader != nil {
							return reader.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0)
						}
						return readerCache.Get(ctx, key, entity.RemoteReadCached, 0)
					}

					if hot {
						// 读节点先确认持有 v1（或旧 epoch 的旧值），之后才发生更新的事实。
						seed := staleBackfillEnvelope(key, 1, 1, "v1")
						if sc.name == "migration-old-epoch" {
							seed = staleBackfillEnvelope(key, 5, 1, "route1-v5")
						}
						if err := owner.snapshots.cache.Publish(ctx, seed); err != nil {
							t.Fatal(err)
						}
						if got, found, err := read(); err != nil || !found || got.StateVersion != seed.StateVersion {
							t.Fatalf("hot seed: version=%d found=%v err=%v", got.StateVersion, found, err)
						}
					}
					stale, publishedAt := sc.newer(t, ctx, owner.snapshots.cache, key, redisKey, r)

					switch source {
					case "publish":
						if err := readerCache.Publish(ctx, stale); err != nil {
							t.Fatalf("a stale publish is the past, not a failure: %v", err)
						}
					case "replica":
						if err := (SnapshotReplicaStore{client: reader.snapshots}).ApplyReplica(ctx, b2ReplicaWire(t, key, stale.StateVersion, string(stale.Payload.BytesCopy()), publishedAt, stale.RouteEpoch)); err != nil {
							t.Fatalf("a stale replica is the past, not a failure: %v", err)
						}
					case "load":
						staleForLoader = stale
						_, _, _ = readerCache.Get(ctx, key, entity.RemoteReadLinearizable, 0)
					}

					// (a) L2 从不持有旧输入。
					stored, held, err := newL2().Get(ctx, key)
					if err != nil {
						t.Fatal(err)
					}
					if held && stored.StateVersion == stale.StateVersion && stored.RouteEpoch == stale.RouteEpoch {
						t.Fatalf("[%s] L2 holds the stale input version=%d route=%d", backend, stored.StateVersion, stored.RouteEpoch)
					}
					// (b) 冷节点立刻读到更新的事实。
					cold := NewManager(newMockVersionedLockFactory(), cfg, 2203, newL2())
					if got, found, err := cold.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || !sc.want(got, found) {
						t.Fatalf("[%s] a cold node read version=%d route=%d payload=%q found=%v err=%v", backend, got.StateVersion, got.RouteEpoch, got.Payload.BytesCopy(), found, err)
					}
					// (c) 读节点：冷时立刻、热时在陈旧上限之后读到更新的事实。
					if hot {
						time.Sleep(b2MatrixStaleness + 100*time.Millisecond)
					}
					got, found, err := read()
					if err != nil || !sc.want(got, found) {
						t.Fatalf("[%s] the reader (hot=%v) read version=%d route=%d payload=%q found=%v err=%v", backend, hot, got.StateVersion, got.RouteEpoch, got.Payload.BytesCopy(), found, err)
					}
				})
			}
		}
	}
}

func mustPrefixedL2Store(t *testing.T, r remoteSnapshotRedis, prefix string, ttl time.Duration) *remoteSnapshotL2Store {
	t.Helper()
	store, err := NewSnapshotL2StoreWithKeyPrefix(r, ttl, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
