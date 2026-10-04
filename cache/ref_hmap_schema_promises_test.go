package cache

// RR-20261004-NC-30：Set/Delete 的 registry 快照变更必须在副作用前拒绝，
// 不能把另一个 schema 已登记的子 hash 变成无 TTL、后续无法清理的孤儿。
// 用真实 Redis 执行 Lua；在 HGet 已读快照后确定性发布新 schema，不靠 sleep。

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
