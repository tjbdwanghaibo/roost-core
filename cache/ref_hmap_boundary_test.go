package cache

// RR-20261004-NC-16～19：补nil根、codec失败、深层Patch、类型冲突和名称边界。
// Redis脚本语义用专属真实实例验证，不以Go替身模拟原子性。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

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
