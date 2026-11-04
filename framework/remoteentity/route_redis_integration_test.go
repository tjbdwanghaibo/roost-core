//go:build integration

// 路由登记的三个 Lua 脚本在真实 Redis（单机与 3 主 3 从 Cluster）上的按值比较（联盟服务方案 §2.7）：迟到的旧 owner
// 登记 / 续期 / 删除不影响新 owner 的登记；续期不重建已删除的键；kill -9 后（没人续期）登记按 TTL 过期。
// 按 owner 枚举所有权记录（预加载）在真实 Mongo 上验。

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
)

// routeRedisTargets 返回要跑的 Redis：单机（ROOST_DATAENGINE_IT_REDIS_ADDR）与私有环境的 Cluster
// （ROOST_MIRROR_LOCAL_REDIS_CLUSTER，没有时跳过）。
func routeRedisTargets(t *testing.T) map[string]string {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with isolated Redis")
	}
	targets := map[string]string{"single": os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")}
	if targets["single"] == "" {
		t.Fatal("isolated Redis address required")
	}
	if cluster := os.Getenv("ROOST_MIRROR_LOCAL_REDIS_CLUSTER"); cluster != "" {
		targets["cluster"] = cluster
	}
	return targets
}

func routeRedisClient(t *testing.T, addrs string) fredis.IRedis {
	t.Helper()
	cfg := fredis.DefaultConfig(addrs)
	if strings.Contains(addrs, ",") {
		cfg = fredis.DefaultConfig("")
		cfg.ClusterAddrs = strings.Split(addrs, ",")
	}
	client, err := redisdriver.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestRealRouteScriptsCompareByValue(t *testing.T) {
	for name, addrs := range routeRedisTargets(t) {
		t.Run(name, func(t *testing.T) {
			prefix := redistest.Prefix(t, addrs, "route-it")
			client := routeRedisClient(t, addrs)
			store, err := NewRouteStore(client, prefix, 30*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// 多个实体：Cluster 下分散在不同槽位，pipeline 按节点路由。
			ids := []int64{1<<12 | 7, 2<<12 | 7, 3<<12 | 7, 4<<12 | 7, 5<<12 | 7, 6<<12 | 7}
			for _, id := range ids {
				if written, err := store.Register(ctx, id, 1000, 2); err != nil || !written {
					t.Fatalf("old owner register %d = %v, %v", id, written, err)
				}
			}
			// 旧 owner 的续期批次在转移之前拼好。
			stale := make([]RouteEntry, len(ids))
			for i, id := range ids {
				stale[i] = RouteEntry{ID: id, Value: RouteValue(1000, 2)}
			}
			// 转移：旧 owner 删除自己的值，新 owner 登记更新的代际。
			if err := store.Delete(ctx, stale[:1]); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if written, err := store.Register(ctx, id, 2000, 3); err != nil || !written {
					t.Fatalf("new owner register %d = %v, %v", id, written, err)
				}
			}
			// 迟到的旧 owner：登记、续期、删除都不能改动新 owner 的登记，续期也不能延长它的 TTL。
			for _, id := range ids {
				if written, err := store.Register(ctx, id, 1000, 2); err != nil || written {
					t.Fatalf("late old register %d = %v, %v; want refused", id, written, err)
				}
			}
			before, err := client.TTL(ctx, RouteKey(prefix, ids[0]))
			if err != nil {
				t.Fatal(err)
			}
			results, err := store.Renew(ctx, stale)
			if err != nil {
				t.Fatal(err)
			}
			for i, result := range results {
				if result != RouteReplaced {
					t.Fatalf("late old renew of %d = %d; want replaced", ids[i], result)
				}
			}
			if err := store.Delete(ctx, stale); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				owner, epoch, found, err := store.Lookup(ctx, id)
				if err != nil || !found || owner != 2000 || epoch != 3 {
					t.Fatalf("after the late old owner, %d routes to %d:%d (found %v, %v); want 2000:3", id, owner, epoch, found, err)
				}
			}
			if after, _ := client.TTL(ctx, RouteKey(prefix, ids[0])); after > before {
				t.Fatalf("the late renewal extended the new owner's TTL from %s to %s", before, after)
			}
			// 新 owner 续期：成功；同代际重新登记（同 sid 重启）覆盖。
			fresh := []RouteEntry{{ID: ids[0], Value: RouteValue(2000, 3)}}
			if results, err := store.Renew(ctx, fresh); err != nil || results[0] != RouteRenewed {
				t.Fatalf("owner renew = %v, %v", results, err)
			}
			if written, err := store.Register(ctx, ids[0], 2000, 3); err != nil || !written {
				t.Fatalf("same-epoch register = %v, %v", written, err)
			}
			// 删掉之后续期得到“不存在”，不重建键。
			if err := store.Delete(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			if results, err := store.Renew(ctx, fresh); err != nil || results[0] != RouteMissing {
				t.Fatalf("renew after delete = %v, %v; want missing", results, err)
			}
			if _, _, found, _ := store.Lookup(ctx, ids[0]); found {
				t.Fatal("a renewal recreated a deleted registration")
			}
			// 大代际（超过 2^53）按字符串比较，不丢精度。
			big := uint64(1)<<60 + 1
			if written, _ := store.Register(ctx, ids[1], 2000, big); !written {
				t.Fatal("register with a large epoch refused")
			}
			if written, _ := store.Register(ctx, ids[1], 1000, big-1); written {
				t.Fatal("an older large epoch overwrote a newer one")
			}
		})
	}
}

// kill -9：进程没了，没人续期，也没人删除；登记在 TTL 后自己消失，调用方回退读所有权记录。
func TestRealRouteRegistrationExpiresWithoutRenewal(t *testing.T) {
	for name, addrs := range routeRedisTargets(t) {
		t.Run(name, func(t *testing.T) {
			prefix := redistest.Prefix(t, addrs, "route-ttl-it")
			client := routeRedisClient(t, addrs)
			store, err := NewRouteStore(client, prefix, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			id := int64(9<<12 | 7)
			if written, err := store.Register(ctx, id, 3000, 1); err != nil || !written {
				t.Fatalf("register = %v, %v", written, err)
			}
			if ttl, err := client.TTL(ctx, RouteKey(prefix, id)); err != nil || ttl <= 0 || ttl > time.Second {
				t.Fatalf("registration TTL = %s, %v; want (0, 1s]", ttl, err)
			}
			deadline := time.Now().Add(4 * time.Second)
			for {
				_, _, found, err := store.Lookup(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the registration of a killed owner did not expire")
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// 预加载的枚举：只返回 owner 是本 sid、独占、kind 在列表里的实体，按 ID 分页；{_owner_sid: 1} 索引由 EnsureRemoteStorage 建。
func TestRealListOwnedRemoteEntitiesFiltersByOwnerAndKind(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || uri == "" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with isolated MongoDB")
	}
	const kindA, kindB entity.EntityKind = 174, 175
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: kindA, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
		entity.EntityKindDef{Kind: kindB, Category: 1, RemotePolicy: entity.RemotePolicyManaged},
	)
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database := "route_it_" + strings.ReplaceAll(generateToken(), "-", "")[:12]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Database(database).Drop(ctx)
		_ = client.Close(ctx)
	})
	store := NewMongoCommitter(client, database, 3000, 0)
	if err := store.EnsureRemoteStorage(ctx); err != nil {
		t.Fatal(err)
	}
	build := func(raw int64, kind entity.EntityKind) int64 {
		id, err := entity.BuildEntityID(raw, kind)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var want []int64
	for raw := int64(1); raw <= 5; raw++ {
		id := build(raw, kindA)
		if _, err := store.ClaimOwnership(ctx, id, 3000); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	otherKind, otherOwner, shared := build(10, kindB), build(11, kindA), build(12, kindA)
	if _, err := store.ClaimOwnership(ctx, otherKind, 3000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOwnership(ctx, otherOwner, 3001); err != nil {
		t.Fatal(err)
	}
	lease, err := store.ClaimOwnership(ctx, shared, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnterSharedExpected(ctx, shared, lease); err != nil {
		t.Fatal(err)
	}
	var got []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		ids, next, err := store.ListOwnedRemoteEntities(ctx, 3000, []entity.EntityKind{kindA}, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ids...)
		if next == 0 || pages > 10 {
			break
		}
		after = next
	}
	if len(got) != len(want) {
		t.Fatalf("listed %v; want exactly the five kind-%d entities owned by 3000 %v", got, kindA, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("listed %v; want %v in id order", got, want)
		}
	}
	var indexes []struct {
		Name string `bson:"name"`
	}
	if err := client.Database(database).Collection(remoteMetaCollection).Aggregate(ctx, []map[string]any{{"$indexStats": map[string]any{}}}, &indexes); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, index := range indexes {
		found = found || index.Name == "owner_sid"
	}
	if !found {
		t.Fatalf("indexes %+v; want owner_sid", indexes)
	}
}

// 端到端（真实 Redis 上的登记与所有权记录）：A 首次认领即登记；A 把实体转给 B 时删掉自己的登记；B 经前置检查冷加载
// 并登记更新的代际；A 迟到的续期不动 B 的登记；B 被 kill -9（不停机、不续期）后登记按 TTL 过期，查找回退到所有权
// 记录，仍找到 B。
func TestRealRouteFollowsOwnershipAcrossTransferAndKill(t *testing.T) {
	addrs := routeRedisTargets(t)["single"]
	redis := routeRedisClient(t, addrs)
	prefix := redistest.Prefix(t, addrs, "route-e2e-it")
	registerRouteKinds()
	key := remoteRedisKey(t, redis)
	cfg := DefaultConfig()
	cfg.LockKey, cfg.LockTTL, cfg.OpTimeout = key, time.Second, 2*time.Second
	cfg.RouteKinds = []entity.EntityKind{routeKindAlliance}
	cfg.SnapshotL2KeyPrefix = prefix
	cfg.RouteTTL = time.Second
	store, err := NewRouteStore(redis, prefix, cfg.RouteTTL)
	if err != nil {
		t.Fatal(err)
	}
	newManager := func(sid int32) *Manager {
		mgr := NewManager(NewVersionedLockFactory(redis), cfg, sid)
		mgr.SetOwnershipStore(NewRedisMarker(redis, key))
		backend := newRemoteTestLoader()
		backend.add(newTestRemoteEntity(7601, 1, routeKindAlliance))
		mgr.SetBackend(backend)
		mgr.routes = newRouteRegistry(store, cfg)
		return mgr
	}
	a, b := newManager(1000), newManager(2000)
	id := testRemoteFullIDWithKind(7601, 1, routeKindAlliance)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = redis.Del(ctx, fmt.Sprintf("lock:%s:%d", key, id), fmt.Sprintf("lock:%s:%d:fence", key, id), key)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lease, err := a.ClaimRemoteOwnership(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if owner, epoch, found, _ := store.Lookup(ctx, id); !found || owner != 1000 || epoch != lease.MarkerEpoch {
		t.Fatalf("after A claimed, route = %d:%d found %v", owner, epoch, found)
	}
	staleRenewal := []RouteEntry{{ID: id, Value: RouteValue(1000, lease.MarkerEpoch)}}
	next, err := a.TransferRemoteOwnership(ctx, id, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := store.Lookup(ctx, id); found {
		t.Fatal("A's registration survived the transfer")
	}
	if owner, found, err := b.ServeRoutedRequest(ctx, id); err != nil || !found || owner != 2000 {
		t.Fatalf("B serve = %d,%v,%v", owner, found, err)
	}
	if results, err := store.Renew(ctx, staleRenewal); err != nil || results[0] != RouteReplaced {
		t.Fatalf("A's late renewal = %v, %v; want replaced", results, err)
	}
	if owner, epoch, found, _ := store.Lookup(ctx, id); !found || owner != 2000 || epoch != next.MarkerEpoch {
		t.Fatalf("B's route = %d:%d found %v; want 2000:%d", owner, epoch, found, next.MarkerEpoch)
	}
	// kill -9：B 不停机、不续期。登记过期后查找回退到所有权记录。
	lookup, err := NewRouteLookup(store, a)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, _, found, _ := store.Lookup(ctx, id); !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the killed owner's registration did not expire")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if owner, found, err := lookup.LookupOwner(ctx, id); err != nil || !found || owner != 2000 {
		t.Fatalf("lookup after expiry = %d,%v,%v; want 2000 from the ownership record", owner, found, err)
	}
}
