// ref-hmap 契约：Patch 路径与布局拒绝、Stale 判定、整条记录随 Patch 续命、缺失引用按未命中、注册表守卫并发、真实 Redis 边界。

package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

func newSessionRefHMapStore(t *testing.T, maxDepth int) *RedisRefHMapStore[int64, refHMapSession] {
	t.Helper()
	return NewRedisRefHMapStore[int64, refHMapSession](newRefHMapFakeRedis(), RefHMapConfig[int64, refHMapSession]{
		Prefix: "roost:test", Name: "session", TTL: time.Hour, MaxDepth: maxDepth,
		StoreConfig: refHMapSessionConfig(),
	})
}

// A patch path is resolved against the reflective layout before anything is written. Each way a path can miss the layout is
// refused as ErrRefHMapUnsupported with the path named; a nested type deeper than the configured limit is refused when the layout is built.
func TestRefHMapPatchPathRefusesEachShapeItCannotAddress(t *testing.T) {
	store := newSessionRefHMapStore(t, 8)
	plan, err := store.plan(1001)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"snapshot..score":     `empty patch path "snapshot..score"`,
		"":                    `empty patch path ""`,
		"snapshot.nope":       `unknown patch path "snapshot.nope"`,
		"snapshot":            `patch path "snapshot" is not scalar`,
		"snapshot.inner":      `patch path "snapshot.inner" is not scalar`,
		"version.major":       `patch path "version.major" crosses non-struct field`,
		"snapshot.state.bits": `patch path "snapshot.state.bits" crosses non-struct field`,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			_, err := plan.patchTarget(path)
			if !errors.Is(err, ErrRefHMapUnsupported) || !strings.Contains(err.Error(), want) {
				t.Fatalf("patchTarget(%q) = %v; want ErrRefHMapUnsupported containing %q", path, err, want)
			}
		})
	}
	var nilPlan *refHMapPlan
	if _, err := nilPlan.patchTarget("snapshot.inner.score"); !errors.Is(err, ErrRefHMapUnsupported) || !strings.Contains(err.Error(), "empty plan") {
		t.Fatalf("nil plan patchTarget = %v", err)
	}
	target, err := plan.patchTarget("snapshot.inner.score")
	if err != nil || target.node == nil || target.key != "roost:test:{session:1001}:snapshot:inner" {
		t.Fatalf("valid patch target = %+v, %v", target, err)
	}

	// Patch goes through the same resolver: a bad path writes nothing.
	redis := newRefHMapFakeRedis()
	writing := NewRedisRefHMapStore[int64, refHMapSession](redis, RefHMapConfig[int64, refHMapSession]{
		Prefix: "roost:test", Name: "session", TTL: time.Hour, StoreConfig: refHMapSessionConfig(),
	})
	ctx := context.Background()
	if err := writing.Set(ctx, refHMapSession{ID: 7, Version: 1, Snapshot: refHMapSnapshot{Inner: refHMapInner{Score: 3}}}); err != nil {
		t.Fatal(err)
	}
	if err := writing.Patch(ctx, 7, "snapshot.nope", int64(9)); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Patch with an unknown path = %v", err)
	}
	got, ok, err := writing.Get(ctx, 7)
	if err != nil || !ok || got.Snapshot.Inner.Score != 3 {
		t.Fatalf("value after refused patch = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestRefHMapLayoutRefusesTypesDeeperThanTheLimit(t *testing.T) {
	shallow := newSessionRefHMapStore(t, 1)
	if _, err := shallow.plan(1); !errors.Is(err, ErrRefHMapMaxDepth) {
		t.Fatalf("plan with MaxDepth 1 for session.snapshot.inner = %v, want ErrRefHMapMaxDepth", err)
	}
	if _, err := newSessionRefHMapStore(t, 2).plan(1); err != nil {
		t.Fatalf("plan with MaxDepth 2 = %v, want the layout to fit", err)
	}
}

// The JSON store's stale-write rule and key-func requirement: a write the
// store refuses must not read back as success.
func TestRedisJSONStoreRefusesStaleWritesAndMissingKeyFunc(t *testing.T) {
	ctx := context.Background()
	keyOf := func(k int) string { return fmt.Sprintf("stale:%d", k) }
	store := NewRedisJSONStore[int, staleValue](newRefHMapFakeRedis(), time.Hour, keyOf, staleConfig())
	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, staleValue{Key: 1, Version: 4, Payload: "old"}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale Set = %v, want ErrStaleWrite", err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Payload != "new" {
		t.Fatalf("value after stale Set = %+v ok=%v err=%v", got, ok, err)
	}
	if err := NewRedisJSONStore[int, staleValue](newRefHMapFakeRedis(), time.Hour, nil, staleConfig()).Set(ctx, staleValue{Key: 1}); err == nil || !strings.Contains(err.Error(), "key func is nil") {
		t.Fatalf("Set without a key func = %v", err)
	}
}

type refTTLRecord struct {
	ID    int64           `json:"id"`
	Name  string          `json:"name"`
	Meta  *refTTLMeta     `json:"meta"`
	Other *refTTLOther    `json:"other"`
	Opt   *refTTLOptional `json:"opt"`
}
type refTTLMeta struct {
	Label string `json:"label"`
}
type refTTLOther struct {
	X int64 `json:"x"`
}

// refTTLOptional 只有指针字段：全为 nil 时 Set 引用它却不写它的 hash，
// 所以“被引用但不存在”对它是合法状态，不能当作过期。
type refTTLOptional struct {
	Note *string `json:"note"`
}

func refTTLStore(redis fredis.IRedis, prefix string, ttl time.Duration) *RedisRefHMapStore[int64, refTTLRecord] {
	return NewRedisRefHMapStore(redis, RefHMapConfig[int64, refTTLRecord]{
		Prefix: prefix, Name: "rec", TTL: ttl,
		StoreConfig: StoreConfig[int64, refTTLRecord]{KeyOf: func(v refTTLRecord) int64 { return v.ID }},
	})
}

func refTTLValue() refTTLRecord {
	return refTTLRecord{ID: 1, Name: "n", Meta: &refTTLMeta{Label: "a"}, Other: &refTTLOther{X: 42}, Opt: &refTTLOptional{}}
}

func TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires an isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	newStore := func(t *testing.T, ttl time.Duration) (*RedisRefHMapStore[int64, refTTLRecord], *refHMapPlan) {
		t.Helper()
		store := refTTLStore(client, "roost:rr0403:"+rand.Text(), ttl)
		plan, err := store.plan(1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = client.Del(context.Background(), plan.keys()...) })
		return store, plan
	}
	otherKey := func(plan *refHMapPlan) string { return plan.base + ":other" }

	// 复审 D3 原样：TTL 600ms，400ms 后 Patch，再 400ms 后 Get。
	for _, patch := range []struct {
		path  string
		value any
	}{{"meta.label", "b"}, {"name", "m"}} {
		t.Run("sibling_outlives_set_ttl/"+patch.path, func(t *testing.T) {
			ctx := context.Background()
			store, _ := newStore(t, 600*time.Millisecond)
			if err := store.Set(ctx, refTTLValue()); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond)
			if err := store.Patch(ctx, 1, patch.path, patch.value); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond) // 过了 Set 的 TTL，仍在 Patch 的 TTL 内
			got, ok, err := store.Get(ctx, 1)
			if err != nil || !ok {
				t.Fatalf("Get after patch renewal = ok=%v err=%v, want the record kept alive by Patch", ok, err)
			}
			if got.Other == nil || got.Other.X != 42 {
				t.Fatalf("partial record reported as found: other=%+v (written 42)", got.Other)
			}
			if got.Meta == nil || got.Opt == nil {
				t.Fatalf("Get after patch renewal lost a subtree: meta=%+v opt=%+v", got.Meta, got.Opt)
			}
		})
	}

	// 不靠睡眠的同一承诺：Patch 之后兄弟 hash 的 TTL 回到 store TTL。
	t.Run("patch_renews_sibling_hash", func(t *testing.T) {
		ctx := context.Background()
		store, plan := newStore(t, time.Hour)
		if err := store.Set(ctx, refTTLValue()); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Expire(ctx, otherKey(plan), time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := store.Patch(ctx, 1, "meta.label", "b"); err != nil {
			t.Fatal(err)
		}
		ttl, err := client.TTL(ctx, otherKey(plan))
		if err != nil {
			t.Fatal(err)
		}
		if ttl < 30*time.Minute {
			t.Fatalf("sibling TTL after Patch = %v, want renewed to the store TTL (1h)", ttl)
		}
	})

	// 修复前写入的数据：兄弟已过期而根还在时，Get 报 miss 而不是部分记录。
	t.Run("expired_sibling_reads_as_miss", func(t *testing.T) {
		ctx := context.Background()
		store, plan := newStore(t, time.Hour)
		if err := store.Set(ctx, refTTLValue()); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Del(ctx, otherKey(plan)); err != nil {
			t.Fatal(err)
		}
		got, ok, err := store.Get(ctx, 1)
		if err != nil || ok {
			t.Fatalf("Get with the referenced sibling gone = %+v other=%+v ok=%v err=%v, want miss", got, got.Other, ok, err)
		}
	})

	// 没有 __keys 的旧数据被 Patch 后，注册表不能只剩路径键，否则 Delete 漏删兄弟。
	t.Run("legacy_registry_delete_after_patch", func(t *testing.T) {
		ctx := context.Background()
		store, plan := newStore(t, time.Hour)
		if err := store.Set(ctx, refTTLValue()); err != nil {
			t.Fatal(err)
		}
		if _, err := client.HDel(ctx, plan.key(plan.root), refHMapRegistryField); err != nil {
			t.Fatal(err)
		}
		if err := store.Patch(ctx, 1, "name", "m"); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, 1); err != nil {
			t.Fatal(err)
		}
		left, err := client.Exists(ctx, plan.keys()...)
		if err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Fatalf("Delete after patching a record without a registry left %d of %v", left, plan.keys())
		}
	})
}

func TestRefHMapGetTreatsAMissingReferencedHashAsMiss(t *testing.T) {
	ctx := context.Background()
	redis := newRefHMapFakeRedis()
	store := refTTLStore(redis, "roost:test", time.Hour)
	plan, err := store.plan(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, refTTLValue()); err != nil {
		t.Fatal(err)
	}
	// 控制：Opt 只有 nil 指针字段，被引用而没有 hash，记录仍完整。
	if _, exists := redis.hashes[plan.base+":opt"]; exists {
		t.Fatal("an all-nil optional child wrote a hash; the control no longer covers the empty-child case")
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Opt == nil || got.Other == nil || got.Other.X != 42 {
		t.Fatalf("complete record = %+v ok=%v err=%v", got, ok, err)
	}

	delete(redis.hashes, plan.base+":other")
	got, ok, err = store.Get(ctx, 1)
	if err != nil || ok {
		t.Fatalf("Get with the referenced other hash gone = %+v other=%+v ok=%v err=%v, want miss", got, got.Other, ok, err)
	}
}

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

// LayeredStore.Delete 的远端删除报错（拒绝、网络、结果未知）时 L1 副本也丢掉——丢缓存总是安全的，下一次 Get 回到权威；错误照常返回。
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

type refSchemaOld struct{ ID, Version int64 }
type refSchemaChild struct{ Score int64 }
type refSchemaNew struct {
	ID, Version int64
	Child       *refSchemaChild
}

type refSchemaHookRedis struct {
	fredis.IRedis
	afterRegistry func() error
	beforeEval    error
	afterEval     error
}

func (r *refSchemaHookRedis) HGet(ctx context.Context, key, field string) ([]byte, error) {
	raw, err := r.IRedis.HGet(ctx, key, field)
	if r.afterRegistry != nil {
		hook := r.afterRegistry
		r.afterRegistry = nil
		if hookErr := hook(); hookErr != nil {
			return nil, hookErr
		}
	}
	return raw, err
}

func (r *refSchemaHookRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if r.beforeEval != nil {
		return nil, r.beforeEval
	}
	result, err := r.IRedis.Eval(ctx, script, keys, args...)
	if err == nil && r.afterEval != nil {
		return nil, r.afterEval
	}
	return result, err
}

func TestRefHMapSchemaRegistryPromises(t *testing.T) {
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
		"set_registry_changed", "delete_registry_changed", "set_sequential_cleanup", "delete_sequential_cleanup",
		"same_layout_last_writer", "delete_unknown_applied", "delete_unknown_unapplied", "registry_read_error",
		"set_registry_changed_by_patch", "delete_registry_changed_by_patch", "legacy_registry_order_cleanup", "delete_missing_root",
	} {
		t.Run(scenario, func(t *testing.T) {
			prefix := "roost:refschema:" + rand.Text()
			oldCfg := RefHMapConfig[int64, refSchemaOld]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refSchemaOld]{KeyOf: func(v refSchemaOld) int64 { return v.ID }}}
			newCfg := RefHMapConfig[int64, refSchemaNew]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refSchemaNew]{KeyOf: func(v refSchemaNew) int64 { return v.ID }}}
			oldStore := NewRedisRefHMapStore(client, oldCfg)
			newStore := NewRedisRefHMapStore(client, newCfg)
			root := prefix + ":{record:1}:root"
			child := prefix + ":{record:1}:child"
			t.Cleanup(func() { _, _ = client.Del(context.Background(), root, child) })
			if err := oldStore.Set(ctx, refSchemaOld{ID: 1, Version: 1}); err != nil {
				t.Fatal(err)
			}
			publish := func() error {
				return newStore.Set(ctx, refSchemaNew{ID: 1, Version: 2, Child: &refSchemaChild{Score: 9}})
			}
			switch scenario {
			case "set_registry_changed", "delete_registry_changed", "set_registry_changed_by_patch", "delete_registry_changed_by_patch":
				wantVersion := int64(2)
				if scenario == "set_registry_changed_by_patch" || scenario == "delete_registry_changed_by_patch" {
					publish = func() error { return newStore.Patch(ctx, 1, "Child.Score", int64(9)) }
					wantVersion = 1
				}
				wrapped := &refSchemaHookRedis{IRedis: client, afterRegistry: publish}
				writer := NewRedisRefHMapStore(wrapped, oldCfg)
				var opErr error
				if scenario == "set_registry_changed" || scenario == "set_registry_changed_by_patch" {
					opErr = writer.Set(ctx, refSchemaOld{ID: 1, Version: 3})
				} else {
					opErr = writer.Delete(ctx, 1)
				}
				got, held, readErr := newStore.Get(ctx, 1)
				t.Logf("registry changed after snapshot: op=%v value=%+v held=%v read=%v", opErr, got, held, readErr)
				if !errors.Is(opErr, ErrRefHMapRegistryChanged) || readErr != nil || !held || got.Version != wantVersion || got.Child == nil || got.Child.Score != 9 {
					// Old implementation reports nil and abandons the child outside root registry.
					if err := oldStore.Delete(ctx, 1); err != nil {
						t.Fatal(err)
					}
					n, err := client.Exists(ctx, child)
					t.Fatalf("registry race was not refused before writes: err=%v child_remaining_after_delete=%d probe=%v", opErr, n, err)
				}
				// Explicit reconciliation/retry reads the fresh registry and owns all keys.
				if err := oldStore.Delete(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if n, err := client.Exists(ctx, root, child); err != nil || n != 0 {
					t.Fatalf("retry left keys: %d %v", n, err)
				}
			case "set_sequential_cleanup", "delete_sequential_cleanup":
				if err := publish(); err != nil {
					t.Fatal(err)
				}
				if scenario == "set_sequential_cleanup" {
					if err := oldStore.Set(ctx, refSchemaOld{ID: 1, Version: 3}); err != nil {
						t.Fatal(err)
					}
					got, held, err := oldStore.Get(ctx, 1)
					if err != nil || !held || got.Version != 3 {
						t.Fatalf("Set: %+v %v %v", got, held, err)
					}
				} else if err := oldStore.Delete(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if n, err := client.Exists(ctx, child); err != nil || n != 0 {
					t.Fatalf("sequential cleanup: %d %v", n, err)
				}
			case "same_layout_last_writer":
				wrapped := &refSchemaHookRedis{IRedis: client, afterRegistry: func() error { return oldStore.Set(ctx, refSchemaOld{ID: 1, Version: 2}) }}
				if err := NewRedisRefHMapStore(wrapped, oldCfg).Set(ctx, refSchemaOld{ID: 1, Version: 3}); err != nil {
					t.Fatal(err)
				}
				got, held, err := oldStore.Get(ctx, 1)
				if err != nil || !held || got.Version != 3 {
					t.Fatalf("not a value CAS: %+v %v %v", got, held, err)
				}
			case "delete_unknown_applied", "delete_unknown_unapplied":
				if err := publish(); err != nil {
					t.Fatal(err)
				}
				wrapped := &refSchemaHookRedis{IRedis: client}
				if scenario == "delete_unknown_applied" {
					wrapped.afterEval = context.DeadlineExceeded
				} else {
					wrapped.beforeEval = context.DeadlineExceeded
				}
				opErr := NewRedisRefHMapStore(wrapped, oldCfg).Delete(ctx, 1)
				if !errors.Is(opErr, context.DeadlineExceeded) {
					t.Fatalf("original unknown cause lost: %v", opErr)
				}
				want := int64(2)
				if scenario == "delete_unknown_applied" {
					want = 0
				}
				if n, err := client.Exists(ctx, root, child); err != nil || n != want {
					t.Fatalf("unknown state: %d want=%d %v", n, want, err)
				}
			case "registry_read_error":
				if err := client.Set(ctx, root, "wrongtype", 0); err != nil {
					t.Fatal(err)
				}
				if err := oldStore.Set(ctx, refSchemaOld{ID: 1, Version: 3}); err == nil {
					t.Fatal("Set ignored registry read error")
				}
				if err := oldStore.Delete(ctx, 1); err == nil {
					t.Fatal("Delete ignored registry read error")
				}
				if raw, err := client.Get(ctx, root); err != nil || string(raw) != "wrongtype" {
					t.Fatalf("read error had side effects: %q %v", raw, err)
				}
			case "legacy_registry_order_cleanup":
				if err := publish(); err != nil {
					t.Fatal(err)
				}
				if err := client.HSet(ctx, root, refHMapRegistryField, child+"\n"+root); err != nil {
					t.Fatal(err)
				}
				if err := oldStore.Delete(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if n, err := client.Exists(ctx, root, child); err != nil || n != 0 {
					t.Fatalf("root was not KEYS[1]: %d %v", n, err)
				}
			case "delete_missing_root":
				for range 2 {
					if err := oldStore.Delete(ctx, 1); err != nil {
						t.Fatal(err)
					}
				}
				if n, err := client.Exists(ctx, root, child); err != nil || n != 0 {
					t.Fatalf("missing root Delete: %d %v", n, err)
				}
			}
		})
	}
}

// 支持的类型往返，Patch 成功可见；坏布局在写前拒绝。历史：RR-20261004-NC-16～19。

type refContractPointerText int

func (v *refContractPointerText) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("code=%d", *v)), nil
}
func (v *refContractPointerText) UnmarshalText(raw []byte) error {
	var n int
	_, err := fmt.Sscanf(string(raw), "code=%d", &n)
	*v = refContractPointerText(n)
	return err
}

type refContractValueText int

func (v refContractValueText) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("code=%d", v)), nil
}
func (v *refContractValueText) UnmarshalText(raw []byte) error {
	var n int
	_, err := fmt.Sscanf(string(raw), "code=%d", &n)
	*v = refContractValueText(n)
	return err
}

type refContractPointerTextRecord struct {
	ID     int64
	Amount refContractPointerText
}
type refContractValueTextRecord struct {
	ID     int64
	Amount refContractValueText
}
type refContractRootAlias struct {
	ID   int64 `json:"id"`
	Root struct {
		ID int64 `json:"id"`
	} `json:"root"`
}
type refContractReservedField struct {
	ID   int64
	Data string `redisdao:"__keys"`
}

func TestRefHMapContractsRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires an isolated Redis")
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"value_control", "pointer_root", "pointer_text", "value_text_control", "patch_nil_parent", "patch_existing_control", "root_alias", "reserved_field"} {
		t.Run(scenario, func(t *testing.T) {
			prefix := "roost:refContract:" + rand.Text()
			defer func() {
				if caught := recover(); caught != nil {
					t.Errorf("supported generic store panicked: %v", caught)
				}
			}()
			switch scenario {
			case "pointer_root":
				store := NewRedisRefHMapStore[int64, *refHMapSession](client, RefHMapConfig[int64, *refHMapSession]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, *refHMapSession]{KeyOf: func(v *refHMapSession) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := &refHMapSession{ID: 1, Version: 2}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				if err != nil || !held || !reflect.DeepEqual(got, want) {
					t.Fatalf("pointer roundtrip: %+v %v %v", got, held, err)
				}
			case "pointer_text":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractPointerTextRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractPointerTextRecord]{KeyOf: func(v refContractPointerTextRecord) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractPointerTextRecord{ID: 1, Amount: 7}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				t.Logf("pointer TextMarshaler roundtrip: %+v held=%v err=%v", got, held, err)
				if err != nil || !held || got != want {
					t.Fatal("recognized text scalar did not roundtrip")
				}
			case "value_text_control":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractValueTextRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractValueTextRecord]{KeyOf: func(v refContractValueTextRecord) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractValueTextRecord{ID: 1, Amount: 7}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				got, held, err := store.Get(ctx, 1)
				if err != nil || !held || got != want {
					t.Fatalf("value TextMarshaler: %+v %v %v", got, held, err)
				}
			case "root_alias":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractRootAlias]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractRootAlias]{KeyOf: func(v refContractRootAlias) int64 { return v.ID }}})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refContractRootAlias{ID: 1}
				want.Root.ID = 99
				if err := store.Set(ctx, want); !errors.Is(err, ErrRefHMapUnsupported) {
					t.Fatalf("RR-NC-19: root collision err=%v, want unsupported before write", err)
				}
				if exists, err := client.Exists(ctx, prefix+":{record:1}:root"); err != nil || exists != 0 {
					t.Fatalf("rejected layout wrote data: exists=%d err=%v", exists, err)
				}

			case "reserved_field":
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refContractReservedField]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refContractReservedField]{KeyOf: func(v refContractReservedField) int64 { return v.ID }}})
				// Registered keys are precisely this test's root; do not delete arbitrary keys from the payload.
				rootKey := prefix + ":{record:1}:root"
				t.Cleanup(func() { _, _ = client.Del(context.Background(), rootKey) })
				want := refContractReservedField{ID: 1, Data: "payload"}
				if err := store.Set(ctx, want); !errors.Is(err, ErrRefHMapUnsupported) {
					t.Fatalf("RR-NC-19: reserved field err=%v, want unsupported before write", err)
				}
				if exists, err := client.Exists(ctx, rootKey); err != nil || exists != 0 {
					t.Fatalf("rejected layout wrote data: exists=%d err=%v", exists, err)
				}

			default:
				store := NewRedisRefHMapStore(client, RefHMapConfig[int64, refHMapSession]{Prefix: prefix, Name: "record", StoreConfig: refHMapSessionConfig()})
				t.Cleanup(func() { _ = store.Delete(context.Background(), 1) })
				want := refHMapSession{ID: 1, Version: 2}
				if scenario != "patch_nil_parent" {
					want.Meta = &refHMapMeta{Label: "before"}
				}
				if err := store.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				if scenario != "value_control" {
					if err := store.Patch(ctx, 1, "Meta.Label", "after"); err != nil {
						t.Fatal(err)
					}
					want.Meta = &refHMapMeta{Label: "after"}
				}
				got, held, err := store.Get(ctx, 1)
				t.Logf("nested patch: got=%+v Meta=%+v held=%v err=%v", got, got.Meta, held, err)
				if err != nil || !held || !reflect.DeepEqual(got, want) {
					t.Fatal("successful nested patch was not observable")
				}
			}
		})
	}
}

// 补 nil 根、codec 失败、深层 Patch、类型冲突和名称边界。Redis 脚本语义用专属真实实例验证，不以 Go 替身模拟原子性。

type refBoundaryText int

var refBoundaryCodecError = errors.New("negative codec value")

func (v *refBoundaryText) MarshalText() ([]byte, error) {
	if *v < 0 {
		return nil, refBoundaryCodecError
	}
	n := *v
	*v = n + 1 // codec拥有指针receiver；框架不能因此改业务scalar。
	return []byte(fmt.Sprintf("code=%d", n)), nil
}
func (v *refBoundaryText) UnmarshalText(raw []byte) error {
	var n int
	_, err := fmt.Sscanf(string(raw), "code=%d", &n)
	*v = refBoundaryText(n)
	return err
}

type refBoundaryRecord struct {
	ID     int64
	Amount refBoundaryText
	Meta   *refBoundaryMeta
}
type refBoundaryMeta struct {
	Child *refBoundaryChild
	Note  string
}
type refBoundaryChild struct{ Score int64 }
type refBoundaryPointer *refBoundaryRecord

func TestRefHMapBoundaryContractsRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires an isolated Redis")
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
	for _, scenario := range []string{"nil_root", "named_pointer", "patch_codec", "marshal_failure", "deep_nil_patch", "missing_root", "wrongtype_no_prefix_write", "registry_delete", "ancestor_ttl"} {
		t.Run(scenario, func(t *testing.T) {
			prefix := "roost:boundary:" + scenario
			cfg := RefHMapConfig[int64, refBoundaryRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refBoundaryRecord]{KeyOf: func(v refBoundaryRecord) int64 { return v.ID }}}
			store := NewRedisRefHMapStore(client, cfg)
			plan, err := store.plan(1)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = client.Del(ctx, plan.keys()...)
			t.Cleanup(func() { _, _ = client.Del(context.Background(), plan.keys()...) })
			if scenario == "nil_root" {
				keyCalls := 0
				ptrStore := NewRedisRefHMapStore(client, RefHMapConfig[int64, *refBoundaryRecord]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, *refBoundaryRecord]{KeyOf: func(v *refBoundaryRecord) int64 { keyCalls++; return v.ID }}})
				if err := ptrStore.Set(ctx, nil); !errors.Is(err, ErrRefHMapUnsupported) || keyCalls != 0 {
					t.Fatalf("nil root: err=%v key calls=%d", err, keyCalls)
				}
				if got, held, err := ptrStore.Get(ctx, 1); err != nil || held || got != nil {
					t.Fatalf("pointer miss: %v %v %v", got, held, err)
				}
				return
			}
			if scenario == "named_pointer" {
				ptrStore := NewRedisRefHMapStore(client, RefHMapConfig[int64, refBoundaryPointer]{Prefix: prefix, Name: "record", StoreConfig: StoreConfig[int64, refBoundaryPointer]{KeyOf: func(v refBoundaryPointer) int64 { return v.ID }}})
				want := refBoundaryPointer(&refBoundaryRecord{ID: 1, Amount: 7})
				if err := ptrStore.Set(ctx, want); err != nil {
					t.Fatal(err)
				}
				if want.Amount != 7 {
					t.Fatalf("codec mutated business scalar: %d", want.Amount)
				}
				got, held, err := ptrStore.Get(ctx, 1)
				if err != nil || !held || !reflect.DeepEqual(got, want) || got == want {
					t.Fatalf("named pointer roundtrip: %v %v %v", got, held, err)
				}
				return
			}
			if scenario == "missing_root" {
				if err := store.Patch(ctx, 1, "Meta.Child.Score", int64(9)); !errors.Is(err, ErrRefHMapUnsupported) {
					t.Fatalf("missing root patch: %v", err)
				}
				if n, err := client.Exists(ctx, plan.keys()...); err != nil || n != 0 {
					t.Fatalf("missing root created keys: %d %v", n, err)
				}
				return
			}
			want := refBoundaryRecord{ID: 1, Amount: 7}
			if scenario == "ancestor_ttl" {
				store.cfg.TTL = 10 * time.Second
			}
			if err := store.Set(ctx, want); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "patch_codec":
				if err := store.Patch(ctx, 1, "Amount", refBoundaryText(10)); err != nil {
					t.Fatal(err)
				}
				want.Amount = 10
			case "marshal_failure":
				bad := want
				bad.Amount = -1
				if err := store.Set(ctx, bad); !errors.Is(err, refBoundaryCodecError) {
					t.Fatalf("marshal failure: %v", err)
				}
				if err := store.Patch(ctx, 1, "Amount", refBoundaryText(-1)); !errors.Is(err, refBoundaryCodecError) {
					t.Fatalf("patch marshal failure: %v", err)
				}
			case "wrongtype_no_prefix_write":
				childKey := prefix + ":{record:1}:meta:child"
				if err := client.Set(ctx, childKey, "sentinel", 0); err != nil {
					t.Fatal(err)
				}
				if err := store.Patch(ctx, 1, "Meta.Child.Score", int64(9)); err == nil {
					t.Fatal("wrongtype patch succeeded")
				}
				if _, err := client.HGet(ctx, plan.key(plan.root), "meta"); !errors.Is(err, fredis.ErrNil) {
					t.Fatalf("type error changed ancestor: %v", err)
				}
				_, _ = client.Del(ctx, childKey)
			default:
				if err := store.Patch(ctx, 1, "Meta.Child.Score", int64(9)); err != nil {
					t.Fatal(err)
				}
				want.Meta = &refBoundaryMeta{Child: &refBoundaryChild{Score: 9}}
				if scenario == "registry_delete" {
					if err := store.Delete(ctx, 1); err != nil {
						t.Fatal(err)
					}
					if n, err := client.Exists(ctx, plan.keys()...); err != nil || n != 0 {
						t.Fatalf("Delete orphaned keys: %d %v", n, err)
					}
					return
				}
				if scenario == "ancestor_ttl" {
					for _, key := range []string{plan.key(plan.root), prefix + ":{record:1}:meta", prefix + ":{record:1}:meta:child"} {
						if ttl, err := client.TTL(ctx, key); err != nil || ttl <= 0 || ttl > 10*time.Second {
							t.Fatalf("ancestor TTL %s: %v %v", key, ttl, err)
						}
					}
				}
			}
			got, held, err := store.Get(ctx, 1)
			if err != nil || !held || !reflect.DeepEqual(got, want) {
				t.Fatalf("boundary roundtrip: %+v want %+v held=%v err=%v", got, want, held, err)
			}
		})
	}
}

func TestRefHMapLayoutRejectsAliasesBeforeIO(t *testing.T) {
	t.Run("duplicate_tag", func(t *testing.T) {
		assertRefHMapAliasRejected(t, struct {
			A int `redisdao:"same"`
			B int `redisdao:"same"`
		}{})
	})
	t.Run("path_separator", func(t *testing.T) {
		assertRefHMapAliasRejected(t, struct {
			Child struct{ Value int } `json:"a:b"`
		}{})
	})
	t.Run("registry", func(t *testing.T) {
		assertRefHMapAliasRejected(t, struct {
			Data string `redisdao:"__keys"`
		}{})
	})
	t.Run("root", func(t *testing.T) {
		assertRefHMapAliasRejected(t, struct {
			Root struct{ Value int } `json:"root"`
		}{})
	})
}

type refHMapNoIO struct{ fredis.IRedis }

func assertRefHMapAliasRejected[V any](t *testing.T, value V) {
	t.Helper()
	// nil内嵌IRedis让任何意外I/O直接失败；通过公开API检验写前拒绝。
	store := NewRedisRefHMapStore[int64, V](&refHMapNoIO{}, RefHMapConfig[int64, V]{StoreConfig: StoreConfig[int64, V]{KeyOf: func(V) int64 { return 1 }}})
	ctx := context.Background()
	if err := store.Set(ctx, value); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Set: %v", err)
	}
	if _, _, err := store.Get(ctx, 1); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Get: %v", err)
	}
	if err := store.Patch(ctx, 1, "A", 1); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Patch: %v", err)
	}
	if err := store.Delete(ctx, 1); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Delete: %v", err)
	}
}
