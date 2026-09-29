package versionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type retirementRedis struct{ goredis.UniversalClient }

func (r retirementRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.UniversalClient.Eval(ctx, script, keys, args...).Result()
}
func (r retirementRedis) Get(ctx context.Context, key string) ([]byte, error) {
	return r.UniversalClient.Get(ctx, key).Bytes()
}
func (r retirementRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	return r.UniversalClient.Del(ctx, keys...).Result()
}

// Failure at the transport boundary must not turn a later retry into an
// unconditional removal. The first script either ran or never reached Redis.
type retirementLostReply struct {
	RedisClient
	applied bool
	failed  bool
}

func (r *retirementLostReply) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script == indexRemoveIfAbsentScript && !r.failed {
		r.failed = true
		if r.applied {
			if _, err := r.RedisClient.Eval(ctx, script, keys, args...); err != nil {
				return nil, err
			}
		}
		return nil, context.DeadlineExceeded
	}
	return r.RedisClient.Eval(ctx, script, keys, args...)
}

func TestBugfix6ConditionalIndexRetirementIntegration(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster_%v", cluster), func(t *testing.T) {
			var client goredis.UniversalClient
			if cluster {
				addr := os.Getenv("ROOST_REVIEW_CLUSTER")
				if addr == "" {
					t.Skip("ROOST_REVIEW_CLUSTER is not set")
				}
				client = goredis.NewClusterClient(&goredis.ClusterOptions{Addrs: strings.Split(addr, ",")})
			} else {
				addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
				if addr == "" {
					t.Skip("ROOST_REDIS_TEST_ADDR is not set")
				}
				client = goredis.NewClient(&goredis.Options{Addr: addr})
			}
			t.Cleanup(func() { _ = client.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := client.Ping(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			prefix := fmt.Sprintf("bugfix6:{retire-%d}:", time.Now().UnixNano())
			index := prefix + "index"
			cfg := RedisConfig[string, counter]{Prefix: prefix + "value:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[counter]{},
				Index: &RedisIndex[counter]{Key: index, Entry: func(counter) (float64, bool) { return 1, true }}}
			store, err := NewRedisStore(retirementRedis{client}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ghost := func(t *testing.T, key string) {
				t.Helper()
				t.Cleanup(func() { _ = client.Del(context.Background(), cfg.Prefix+key).Err() })
				if err := client.ZAdd(ctx, index, goredis.Z{Member: key, Score: 1}).Err(); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = client.Del(context.Background(), index).Err() })
			assertIndexed := func(t *testing.T, key string) {
				t.Helper()
				if _, err := client.ZScore(ctx, index, key).Result(); err != nil {
					t.Fatalf("live work disappeared: %v", err)
				}
			}
			t.Run("cleanup_before_create", func(t *testing.T) {
				ghost(t, "before")
				removed, err := store.IndexRemoveIfAbsent(ctx, "before")
				if err != nil || !removed {
					t.Fatalf("ghost: removed=%v err=%v", removed, err)
				}
				if _, err := client.ZScore(ctx, index, "before").Result(); !errors.Is(err, goredis.Nil) {
					t.Fatalf("ghost retained: %v", err)
				}
				if removed, err := store.IndexRemoveIfAbsent(ctx, "before"); err != nil || removed {
					t.Fatalf("repeat: removed=%v err=%v", removed, err)
				}
				if _, created, err := store.Create(ctx, "before", counter{Total: 1}); err != nil || !created {
					t.Fatalf("create: %v %v", created, err)
				}
				assertIndexed(t, "before")
			})
			t.Run("create_before_cleanup", func(t *testing.T) {
				ghost(t, "after")
				if _, created, err := store.Create(ctx, "after", counter{Total: 2}); err != nil || !created {
					t.Fatalf("create: %v %v", created, err)
				}
				if removed, err := store.IndexRemoveIfAbsent(ctx, "after"); err != nil || removed {
					t.Fatalf("live value: removed=%v err=%v", removed, err)
				}
				assertIndexed(t, "after")
			})
			for i, raw := range []string{"", "not-an-envelope", "0\n{}"} {
				t.Run(fmt.Sprintf("existing_raw_%d", i), func(t *testing.T) {
					key := fmt.Sprintf("raw-%d", i)
					ghost(t, key)
					if err := client.Set(ctx, cfg.Prefix+key, raw, 0).Err(); err != nil {
						t.Fatal(err)
					}
					if removed, err := store.IndexRemoveIfAbsent(ctx, key); err != nil || removed {
						t.Fatalf("malformed record hidden: %v %v", removed, err)
					}
					assertIndexed(t, key)
					if got, err := client.Get(ctx, cfg.Prefix+key).Result(); err != nil || got != raw {
						t.Fatalf("record changed: %q %v", got, err)
					}
				})
			}
			for _, applied := range []bool{false, true} {
				t.Run(fmt.Sprintf("lost_reply_applied_%v", applied), func(t *testing.T) {
					key := fmt.Sprintf("unknown-%v", applied)
					ghost(t, key)
					lost := &retirementLostReply{RedisClient: retirementRedis{client}, applied: applied}
					uncertain, err := NewRedisStore(lost, cfg)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := uncertain.IndexRemoveIfAbsent(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("lost reply not surfaced: %v", err)
					}
					if _, created, err := store.Create(ctx, key, counter{Total: 3}); err != nil || !created {
						t.Fatalf("late create: %v %v", created, err)
					}
					if removed, err := uncertain.IndexRemoveIfAbsent(ctx, key); err != nil || removed {
						t.Fatalf("retry hid new work: %v %v", removed, err)
					}
					assertIndexed(t, key)
				})
			}
		})
	}
}
