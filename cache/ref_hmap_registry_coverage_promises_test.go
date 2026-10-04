package cache

// RR-20261004-09：RefHMap Set / Delete 的注册表 guard 只防“当前注册表里有本次
// 清理清单没覆盖的键”（真正的 schema 竞争）。同布局的正常并发——两写者同时
// 首次创建、两个 Delete 并发、Set 与 Delete 并发、Set 读完注册表后整条记录到期、
// 经 LayeredStore 首次创建——注册表字节会变，但清理清单仍覆盖它的每个键，
// 必须成功。v1.19.1（NC-30）逐字节比较，把它们都报成 ErrRefHMapRegistryChanged。
// 经 LayeredStore 时还多一层：remote.Delete 失败直接返回、不删 L1，Redis 里
// 已被删掉的记录在本进程 L1 的 TTL 窗口内仍被返回。
// 真实 Redis 执行 Lua；并发写者在 HGet 读完注册表之后、Eval 之前确定性插入。

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

func TestRefHMapRegistryGuardAcceptsSameLayoutConcurrencyRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires isolated Redis for registry/Lua semantics")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{
		"first_create_same_layout", "delete_vs_delete", "set_vs_delete", "set_vs_expiry", "layered_first_create",
		"layered_delete_vs_delete_drops_l1", "layered_schema_race_delete_drops_l1", "schema_race_first_create_refused",
	} {
		t.Run(scenario, func(t *testing.T) {
			prefix := "roost:rr0409:" + rand.Text()
			cfg := RefHMapConfig[int64, refSchemaNew]{Prefix: prefix, Name: "record", TTL: time.Hour, StoreConfig: StoreConfig[int64, refSchemaNew]{KeyOf: func(v refSchemaNew) int64 { return v.ID }}}
			oldCfg := RefHMapConfig[int64, refSchemaOld]{Prefix: prefix, Name: "record", TTL: time.Hour, StoreConfig: StoreConfig[int64, refSchemaOld]{KeyOf: func(v refSchemaOld) int64 { return v.ID }}}
			other := NewRedisRefHMapStore(client, cfg) // 另一个进程，同布局
			root := prefix + ":{record:1}:root"
			child := prefix + ":{record:1}:child"
			t.Cleanup(func() { _, _ = client.Del(context.Background(), root, child) })
			value := func(version int64) refSchemaNew {
				return refSchemaNew{ID: 1, Version: version, Child: &refSchemaChild{Score: version}}
			}
			// wantFinal：0 表示记录必须已删除（Redis 里没有任何键），否则是最终版本。
			var opErr error
			var wantFinal int64
			switch scenario {
			case "first_create_same_layout":
				hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return other.Set(ctx, value(1)) }}
				opErr = NewRedisRefHMapStore(hook, cfg).Set(ctx, value(2))
				wantFinal = 2
			case "delete_vs_delete":
				if err := other.Set(ctx, value(1)); err != nil {
					t.Fatal(err)
				}
				hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return other.Delete(ctx, 1) }}
				opErr = NewRedisRefHMapStore(hook, cfg).Delete(ctx, 1)
			case "set_vs_delete", "set_vs_expiry":
				if err := other.Set(ctx, value(1)); err != nil {
					t.Fatal(err)
				}
				hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error {
					if scenario == "set_vs_delete" {
						return other.Delete(ctx, 1)
					}
					// 整条记录在 HGet 与 Eval 之间一起到达公共 TTL。
					_, err := client.Del(ctx, root, child)
					return err
				}}
				opErr = NewRedisRefHMapStore(hook, cfg).Set(ctx, value(2))
				wantFinal = 2
			case "layered_first_create":
				hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return other.Set(ctx, value(1)) }}
				layered := NewLayeredStore[int64, refSchemaNew](NewLocalStore(cfg.StoreConfig), NewRedisRefHMapStore(hook, cfg), time.Minute, cfg.StoreConfig)
				opErr = layered.Set(ctx, value(2))
				if got, held, err := layered.Get(ctx, 1); opErr == nil && (err != nil || !held || got.Version != 2) {
					t.Fatalf("layered read after first create: %+v held=%v err=%v", got, held, err)
				}
				wantFinal = 2
			case "layered_delete_vs_delete_drops_l1":
				// 进程 A 经 LayeredStore 删除，进程 B 在 A 读完注册表后先删掉同一条记录。
				hook := &refSchemaHookRedis{IRedis: client}
				layered := NewLayeredStore[int64, refSchemaNew](NewLocalStore(cfg.StoreConfig), NewRedisRefHMapStore(hook, cfg), time.Minute, cfg.StoreConfig)
				if err := layered.Set(ctx, value(1)); err != nil {
					t.Fatal(err)
				}
				hook.afterRegistry = func() error { return other.Delete(ctx, 1) }
				opErr = layered.Delete(ctx, 1)
				got, held, err := layered.Get(ctx, 1)
				n, _ := client.Exists(ctx, root, child)
				if opErr != nil || err != nil || held {
					t.Fatalf("record deleted in Redis (keys=%d) but A.Delete=%v and A.Get held=%v version=%d err=%v", n, opErr, held, got.Version, err)
				}
			case "layered_schema_race_delete_drops_l1":
				// 真正的 schema 竞争：旧布局进程 A 删除时，新布局进程在它读完注册表后
				// 发布了新键。远端必须拒绝；A 的 L1 不能继续返回被它要求删除的旧值。
				newStore := NewRedisRefHMapStore(client, cfg)
				hook := &refSchemaHookRedis{IRedis: client}
				local := NewLocalStore(oldCfg.StoreConfig)
				layered := NewLayeredStore[int64, refSchemaOld](local, NewRedisRefHMapStore(hook, oldCfg), time.Minute, oldCfg.StoreConfig)
				if err := layered.Set(ctx, refSchemaOld{ID: 1, Version: 1}); err != nil {
					t.Fatal(err)
				}
				hook.afterRegistry = func() error { return newStore.Set(ctx, value(2)) }
				delErr := layered.Delete(ctx, 1)
				_, localHeld, localErr := local.Get(ctx, 1)
				t.Logf("schema race delete: err=%v local_held=%v", delErr, localHeld)
				if !errors.Is(delErr, ErrRefHMapRegistryChanged) {
					t.Fatalf("schema race delete must be refused: %v", delErr)
				}
				if localErr != nil || localHeld {
					t.Fatalf("remote refused Delete=%v but L1 still held=%v err=%v", delErr, localHeld, localErr)
				}
				if got, held, err := newStore.Get(ctx, 1); err != nil || !held || got.Version != 2 || got.Child == nil || got.Child.Score != 2 {
					t.Fatalf("new layout record changed by refused delete: %+v held=%v err=%v", got, held, err)
				}
				return
			case "schema_race_first_create_refused":
				// 对照：旧布局写者按 fallback 清单清理，新布局创建者在它读完注册表后
				// 登记了 child。旧写者的清单不含 child，NC-30 的拒绝必须保留。
				hook := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return other.Set(ctx, value(1)) }}
				opErr = NewRedisRefHMapStore(hook, oldCfg).Set(ctx, refSchemaOld{ID: 1, Version: 2})
				if !errors.Is(opErr, ErrRefHMapRegistryChanged) {
					t.Fatalf("schema race must be refused before writes: %v", opErr)
				}
				if got, held, err := other.Get(ctx, 1); err != nil || !held || got.Version != 1 || got.Child == nil || got.Child.Score != 1 {
					t.Fatalf("refused write changed the record: %+v held=%v err=%v", got, held, err)
				}
				return
			}
			got, held, readErr := other.Get(ctx, 1)
			n, _ := client.Exists(ctx, root, child)
			t.Logf("%s: op=%v final=%+v held=%v read=%v keys=%d", scenario, opErr, got, held, readErr, n)
			if opErr != nil {
				t.Fatalf("same-layout %s refused although the cleanup list covers every registered key: err=%v (final held=%v keys=%d)", scenario, opErr, held, n)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if wantFinal == 0 {
				if held || n != 0 {
					t.Fatalf("record not deleted: held=%v keys=%d", held, n)
				}
				return
			}
			if !held || got.Version != wantFinal || got.Child == nil || got.Child.Score != wantFinal {
				t.Fatalf("final record = %+v held=%v, want version %d", got, held, wantFinal)
			}
			// 整条记录都带 Set 的 TTL，不留无 TTL 的键。
			for _, key := range []string{root, child} {
				if ttl, err := client.TTL(ctx, key); err != nil || ttl <= 0 {
					t.Fatalf("%s ttl=%v err=%v", key, ttl, err)
				}
			}
		})
	}
}

// RR-20261004-09：LayeredStore.Delete 的远端删除报错（拒绝、网络、结果未知）时，
// L1 副本也必须丢掉——丢缓存总是安全的，下一次 Get 回到权威；错误照常返回。
func TestLayeredDeleteDropsL1WhenRemoteDeleteFails(t *testing.T) {
	ctx := context.Background()
	cfg := staleConfig()
	local := NewLocalStore(cfg)
	cause := errors.New("remote delete failed")
	remote := &admissionPolicyRemote{Store: NewLocalStore(cfg)}
	store := NewLayeredStore[int, staleValue](local, remote, time.Minute, cfg)
	if err := store.Set(ctx, staleValue{Key: 1, Version: 1, Payload: "held"}); err != nil {
		t.Fatal(err)
	}
	remote.deleteErr = cause
	if err := store.Delete(ctx, 1); !errors.Is(err, cause) {
		t.Fatalf("Delete err=%v, want remote cause", err)
	}
	if _, held, err := local.Get(ctx, 1); err != nil || held {
		t.Fatalf("L1 still held=%v err=%v after the remote delete failed", held, err)
	}
	if store.localValid(1, time.Now()) {
		t.Fatal("L1 expiry kept after the remote delete failed")
	}
	// 远端没删成：下一次 Get 读权威，而不是 L1 的旧副本或假 miss。
	if got, held, err := store.Get(ctx, 1); err != nil || !held || got.Payload != "held" {
		t.Fatalf("Get after failed delete = %+v held=%v err=%v", got, held, err)
	}
}
