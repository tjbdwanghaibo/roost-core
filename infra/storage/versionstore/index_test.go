// 存储内索引：与值同一次写、按分数有界读取、按所有者分开、条件退役与条件删除、NaN 分数拒绝。

package versionstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// 一条记录和“它在不在待办集合里”是同一次写：索引是存储自己的一部分，值怎么写索引就怎么动，值没写成功索引就不动。
// 历史：RR-20260919-04。

type indexedRecord struct {
	Name    string `json:"name"`
	Pending bool   `json:"pending"`
	Due     int64  `json:"due"`
}

func newIndexedStore(t *testing.T, client RedisClient) *RedisStore[string, indexedRecord] {
	t.Helper()
	store, err := NewRedisStore(client, RedisConfig[string, indexedRecord]{
		Prefix: "test:rec:",
		KeyOf:  func(key string) string { return key },
		Codec:  JSONCodec[indexedRecord]{},
		Index: &RedisIndex[indexedRecord]{
			Key: "test:pending",
			Entry: func(value indexedRecord) (float64, bool) {
				return float64(value.Due), value.Pending
			},
		},
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func TestAnIndexedCreateEntersTheIndex(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()

	if _, created, err := store.Create(ctx, "a", indexedRecord{Name: "a", Pending: true, Due: 100}); err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	due, err := store.IndexDue(ctx, 1000, 10)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if len(due) != 1 || due[0] != "a" {
		t.Fatalf("index = %v, want the created record", due)
	}

	// A record that is not pending is not indexed at all: the index is the
	// set of things the reader has work for, not a copy of the keyspace.
	if _, created, err := store.Create(ctx, "b", indexedRecord{Name: "b", Due: 50}); err != nil || !created {
		t.Fatalf("create b: %v", err)
	}
	if due, _ := store.IndexDue(ctx, 1000, 10); len(due) != 1 {
		t.Fatalf("index = %v, want only the pending record", due)
	}
}

// The update that ends the work retires the entry, in the same write.
func TestAnIndexedUpdateRetiresTheEntry(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", indexedRecord{Name: "a", Pending: true, Due: 100}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, "a", func(current indexedRecord, _ bool) (indexedRecord, bool, error) {
		current.Pending = false
		return current, true, nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if due, _ := store.IndexDue(ctx, 1000, 10); len(due) != 0 {
		t.Fatalf("index = %v, want the finished record retired", due)
	}
}

// Due is a cursor, not a filter applied afterwards: an entry whose score is in
// the future is in the index and is not returned.
func TestTheIndexIsReadByScoreAndBounded(t *testing.T) {
	client := newFakeRedis()
	store := newIndexedStore(t, client)
	ctx := context.Background()
	for _, record := range []indexedRecord{
		{Name: "soon", Pending: true, Due: 100},
		{Name: "later", Pending: true, Due: 500},
		{Name: "never", Pending: true, Due: 900},
	} {
		if _, _, err := store.Create(ctx, record.Name, record); err != nil {
			t.Fatal(err)
		}
	}
	due, err := store.IndexDue(ctx, 600, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0] != "soon" || due[1] != "later" {
		t.Fatalf("due = %v, want the two due entries oldest first", due)
	}
	if due, _ := store.IndexDue(ctx, 50, 10); len(due) != 0 {
		t.Fatalf("due = %v, want nothing before the first score", due)
	}
}

// A store without an index keeps working, and asking one for its index says
// so rather than answering an empty list.
func TestAStoreWithoutAnIndexSaysSo(t *testing.T) {
	client := newFakeRedis()
	store, err := NewRedisStore(client, RedisConfig[string, indexedRecord]{
		Prefix: "test:rec:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[indexedRecord]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(context.Background(), "a", indexedRecord{Name: "a"}); err != nil {
		t.Fatalf("an unindexed store stopped working: %v", err)
	}
	if _, err := store.IndexDue(context.Background(), 100, 10); err == nil {
		t.Fatal("a store with no index answered an index read with an empty list")
	}
}

// 一个存储可以每个所有者一份待办清单：索引键由值算出，且只能取值里不会变的部分（一次写只会碰它当时算出来的那个键，键变了旧条目就成孤儿）。
// 历史：RR-20260919-10。

type ownedRecord struct {
	Owner   string `json:"owner"`
	Pending bool   `json:"pending"`
	Due     int64  `json:"due"`
}

func TestAPerOwnerIndexKeepsOwnersApart(t *testing.T) {
	client := newFakeRedis()
	store, err := NewRedisStore(client, RedisConfig[string, ownedRecord]{
		Prefix: "test:owned:",
		KeyOf:  func(key string) string { return key },
		Codec:  JSONCodec[ownedRecord]{},
		Index: &RedisIndex[ownedRecord]{
			KeyOf: func(value ownedRecord) string { return "test:owed:" + value.Owner },
			Entry: func(value ownedRecord) (float64, bool) { return float64(value.Due), value.Pending },
		},
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	for key, record := range map[string]ownedRecord{
		"a-1": {Owner: "a", Pending: true, Due: 100},
		"a-2": {Owner: "a", Pending: true, Due: 200},
		"b-1": {Owner: "b", Pending: true, Due: 150},
	} {
		if _, _, err := store.Create(ctx, key, record); err != nil {
			t.Fatal(err)
		}
	}

	owedA, err := store.IndexDueIn(ctx, "test:owed:a", 1000, 10)
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	if len(owedA) != 2 || owedA[0] != "a-1" || owedA[1] != "a-2" {
		t.Fatalf("owner a is owed %v, want its own two, oldest first", owedA)
	}
	owedB, _ := store.IndexDueIn(ctx, "test:owed:b", 1000, 10)
	if len(owedB) != 1 || owedB[0] != "b-1" {
		t.Fatalf("owner b is owed %v, want only its own", owedB)
	}

	// Finishing one owner's record leaves the other's alone.
	if _, _, err := store.Update(ctx, "a-1", func(current ownedRecord, _ bool) (ownedRecord, bool, error) {
		current.Pending = false
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if owed, _ := store.IndexDueIn(ctx, "test:owed:a", 1000, 10); len(owed) != 1 || owed[0] != "a-2" {
		t.Fatalf("owner a is owed %v after one was finished, want the other one", owed)
	}
	if owed, _ := store.IndexDueIn(ctx, "test:owed:b", 1000, 10); len(owed) != 1 {
		t.Fatalf("owner b's index was disturbed: %v", owed)
	}
}

// Exactly one of Key / KeyOf: a store that says both, or neither, is a
// configuration nobody can read.
func TestAnIndexNeedsExactlyOneKeyForm(t *testing.T) {
	client := newFakeRedis()
	for name, index := range map[string]*RedisIndex[ownedRecord]{
		"neither": {Entry: func(ownedRecord) (float64, bool) { return 0, true }},
		"both": {
			Key:   "test:owed",
			KeyOf: func(ownedRecord) string { return "test:owed:x" },
			Entry: func(ownedRecord) (float64, bool) { return 0, true },
		},
	} {
		_, err := NewRedisStore(client, RedisConfig[string, ownedRecord]{
			Prefix: "test:owned:", KeyOf: func(key string) string { return key },
			Codec: JSONCodec[ownedRecord]{}, Index: index,
		})
		if err == nil {
			t.Errorf("%s: an index with an unclear key form was accepted", name)
		}
	}
}

// 带索引的写，索引分数在发出前校验：NaN 的写不发出、什么都不改，返回 ErrCASInvalidCommand（Redis 的 ZADD 拒绝 NaN 时同一脚本里的
// SET 已经执行，值与索引就不再同进退）。替身忠实模拟“ZADD 拒绝时 SET 已生效”；真实 Redis 见 write_token_integration_test.go。
// 历史：RR-20261006-35。

func TestAnIndexedWriteWithANaNScoreChangesNothing(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "nan:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		Index: &RedisIndex[counter]{Key: "nan:due", Entry: func(c counter) (float64, bool) {
			if c.Total == 1 {
				return math.NaN(), true
			}
			return float64(c.Total), true
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	_, applied, err := store.Update(ctx, "a", increment)
	stored, _, _ := store.Get(ctx, "a")
	if applied || !errors.Is(err, fredis.ErrCASInvalidCommand) || stored.Version != 1 || stored.Value.Total != 0 {
		t.Fatalf("an update whose index score is NaN returned applied=%v err=%v and the store holds %+v (want nothing written, ErrCASInvalidCommand)",
			applied, err, stored)
	}
	if score := fake.index["nan:due"]["a"]; score != 0 {
		t.Fatalf("index entry moved to %v", score)
	}
}

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

// 条件索引退役在真实 Redis（单机与 Cluster）上只移除仍指向本记录的条目；首个脚本回复丢失后的重试不变成无条件移除。
func TestConditionalIndexRetirementIntegration(t *testing.T) {
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

func TestConditionalDeleteProtectsRecreatedIdentity(t *testing.T) {
	run := func(t *testing.T, store interface {
		Store[string, string]
		ConditionalDeleter[string, string]
	}) {
		ctx := context.Background()
		a, _, err := store.Create(ctx, "owner", "run-a")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "owner", a); err != nil {
			t.Fatal(err)
		}
		b, _, err := store.Create(ctx, "owner", "run-b")
		if err != nil {
			t.Fatal(err)
		}
		if b.Version != a.Version {
			t.Fatalf("fixture did not recreate the ABA: %d/%d", a.Version, b.Version)
		}
		err = store.DeleteIf(ctx, "owner", a, func(current string) bool { return current == a.Value })
		if !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("old identity delete: %v", err)
		}
		got, found, err := store.Get(ctx, "owner")
		if err != nil || !found || got.Value != "run-b" {
			t.Fatalf("new holder lost: %+v %v %v", got, found, err)
		}
		if err := store.DeleteIf(ctx, "owner", b, func(current string) bool { return current == b.Value }); err != nil {
			t.Fatal(err)
		}
		if _, found, err := store.Get(ctx, "owner"); found || err != nil {
			t.Fatal(found, err)
		}
	}
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryStore[string, string]()) })
	t.Run("redis", func(t *testing.T) {
		addr := os.Getenv("ROOST_REVIEW_REDIS")
		if addr == "" {
			t.Skip("set ROOST_REVIEW_REDIS for isolated Redis")
		}
		client, err := driver.NewClient(fredis.DefaultConfig(addr))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		prefix := fmt.Sprintf("delete-regression:%d", time.Now().UnixNano())
		store, err := NewRedisStore(client, RedisConfig[string, string]{Prefix: prefix, KeyOf: func(k string) string { return k }, Codec: JSONCodec[string]{}, Index: &RedisIndex[string]{Key: prefix + ":pending", Entry: func(string) (float64, bool) { return 0, true }}})
		if err != nil {
			t.Fatal(err)
		}
		run(t, store)
		if size, err := client.ZCard(context.Background(), prefix+":pending"); err != nil || size != 0 {
			t.Fatalf("deleted value left an index member: size=%d err=%v", size, err)
		}
	})
}
