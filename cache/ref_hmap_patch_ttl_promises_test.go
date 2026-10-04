package cache

// RR-20261004-03：一条 RefHMap 记录的所有 hash 同生共死。
// 旧行为：Patch 只对根到叶路径上的 hash 续期，兄弟 hash 保留上一次 Set 的 TTL；
// 兄弟过期后 Get 仍报 ok=true，返回从未写过的部分记录（写入的 Other.X=42 读回 0）。
// 承诺：Patch 续期整条记录的全部布局 hash；Get 遇到被引用、按布局写入时必然非空、
// 却已不存在的子 hash，把整条记录当 miss，而不是拼出部分值。
// 真实 Lua 语义用专属 Redis（ROOST_REDIS_TEST_ADDR）验证；替身只覆盖 Get 的解码兜底。

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

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
