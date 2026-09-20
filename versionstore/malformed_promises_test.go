package versionstore

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// RR-20260920-05 的使能件：一条读不回来的记录，必须和"基础设施出问题了"区分开。
//
// 两者要的反应正相反：超时值得立刻重试，而字节解不开的记录会永远以同样的方式失败。
// 分不开的重试循环只有两个结局——要么对瞬时错误放弃，要么让一条坏记录永久占住队头。
func TestAnUnreadableRecordIsReportedAsMalformed(t *testing.T) {
	for name, stored := range map[string]string{
		"no version separator": "not-an-envelope",
		"version not a number": "abc\n{}",
		"version is zero":      "0\n{}",
		"payload does not decode": "1\n{not json",
	} {
		t.Run(name, func(t *testing.T) {
			store := newMalformedTestStore(t)
			store.client.values["bad"] = []byte(stored)
			_, _, err := store.store.Get(context.Background(), "bad")
			if !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("an unreadable record reported %v, want ErrMalformedRecord", err)
			}
		})
	}
}

// 反面：基础设施错误**不能**被归成 malformed，否则调用方会把一次超时当成永久损坏。
func TestATransportFailureIsNotMalformed(t *testing.T) {
	store := newMalformedTestStore(t)
	store.client.fail = errors.New("connection reset")
	_, _, err := store.store.Get(context.Background(), "anything")
	if err == nil {
		t.Fatal("a failing transport returned no error")
	}
	if errors.Is(err, ErrMalformedRecord) {
		t.Fatalf("a transport failure was classified as an unreadable record: %v", err)
	}
}

type malformedTestStore struct {
	client *malformedFakeRedis
	store  *RedisStore[string, map[string]any]
}

func newMalformedTestStore(t *testing.T) malformedTestStore {
	t.Helper()
	client := &malformedFakeRedis{values: map[string][]byte{}}
	store, err := NewRedisStore(client, RedisConfig[string, map[string]any]{
		Prefix: "orders",
		KeyOf:  func(k string) string { return k },
		Codec:  JSONCodec[map[string]any]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return malformedTestStore{client: client, store: store}
}

type malformedFakeRedis struct {
	values map[string][]byte
	fail   error
}

func (f *malformedFakeRedis) Get(_ context.Context, key string) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	// Keyed by nothing on purpose: this fake answers for whatever key the
	// store renders, so the test is about decoding rather than key format.
	if len(f.values) == 0 {
		return nil, fredis.ErrNil
	}
	for _, value := range f.values {
		return value, nil
	}
	return nil, fredis.ErrNil
}

func (f *malformedFakeRedis) Del(_ context.Context, keys ...string) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	var removed int64
	for _, key := range keys {
		if _, ok := f.values[key]; ok {
			delete(f.values, key)
			removed++
		}
	}
	return removed, nil
}

func (f *malformedFakeRedis) Eval(_ context.Context, _ string, _ []string, _ ...any) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return []any{int64(1), ""}, nil
}


// IndexDefer 的两条承诺：把已有条目挪到后面，不存在的条目不要凭空造出来。
// 后一条是为了不让"索引"和"记录"悄悄分叉——一个 defer 不存在条目的调用方，
// 说明它已经和索引不同步了。
func TestIndexDeferMovesAnExistingEntryAndCreatesNothingIntegration(t *testing.T) {
	client := integrationVersionRedis(t)
	ctx := context.Background()
	unique := strconv.FormatInt(time.Now().UnixNano(), 36)
	prefix := "roost:test:vs:" + unique
	indexKey := prefix + ":index"
	t.Cleanup(func() { client.Del(ctx, indexKey, prefix+":a") })

	store, err := NewRedisStore(evalRedis{client: client}, RedisConfig[string, map[string]any]{
		Prefix: prefix,
		KeyOf:  func(k string) string { return k },
		Codec:  JSONCodec[map[string]any]{},
		Index: &RedisIndex[map[string]any]{
			Key:   indexKey,
			Entry: func(map[string]any) (float64, bool) { return 10, true },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(ctx, "a", map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	due, err := store.IndexDue(ctx, 20, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due=%v err=%v", due, err)
	}

	moved, err := store.IndexDefer(ctx, "a", 1000)
	if err != nil || !moved {
		t.Fatalf("deferring an existing entry: moved=%v err=%v", moved, err)
	}
	if due, err = store.IndexDue(ctx, 20, 10); err != nil || len(due) != 0 {
		t.Fatalf("a deferred entry is still due: due=%v err=%v", due, err)
	}
	if due, err = store.IndexDue(ctx, 2000, 10); err != nil || len(due) != 1 {
		t.Fatalf("a deferred entry disappeared instead of moving: due=%v err=%v", due, err)
	}

	moved, err = store.IndexDefer(ctx, "never-indexed", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("deferring an absent entry reported a move")
	}
	if due, err = store.IndexDue(ctx, 2000, 10); err != nil || len(due) != 1 {
		t.Fatalf("deferring an absent entry added it to the index: due=%v err=%v", due, err)
	}
}

// The integration harness: the index operations are Lua, so a Go double would
// only prove what I think the script does.
func integrationVersionRedis(t *testing.T) *goredis.Client {
	t.Helper()
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	client := goredis.NewClient(&goredis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis at %s is not reachable: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

type evalRedis struct{ client *goredis.Client }

func (r evalRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return r.client.Eval(ctx, script, keys, args...).Result()
}

func (r evalRedis) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return nil, fredis.ErrNil
	}
	return value, err
}

func (r evalRedis) Del(ctx context.Context, keys ...string) (int64, error) {
	return r.client.Del(ctx, keys...).Result()
}
