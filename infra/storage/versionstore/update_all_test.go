// 多键原子更新（UpdateAll）：全部键一起生效或都不生效、读写之间任何一个键被改就整体重试、拒绝保存与错误不写、
// 不存在的键被创建、入参守卫、回复丢失按写令牌判定、预算用尽报 ErrConflict；真实 Redis 上端到端重试。

package versionstore

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// forEachMultiStore 用同一套用例驱动两个实现。
func forEachMultiStore(t *testing.T, run func(t *testing.T, store interface {
	Store[string, counter]
	MultiUpdater[string, counter]
}, fake *fakeRedis)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryStore[string, counter](), nil) })
	t.Run("redis", func(t *testing.T) {
		fake := newFakeRedis()
		run(t, newTokenTestStore(t, fake), fake)
	})
}

func incrementAll(current []counter, _ []bool) ([]counter, bool, error) {
	next := make([]counter, len(current))
	for i, value := range current {
		value.Total++
		next[i] = value
	}
	return next, true, nil
}

func TestUpdateAllWritesEveryKeyTogether(t *testing.T) {
	forEachMultiStore(t, func(t *testing.T, store interface {
		Store[string, counter]
		MultiUpdater[string, counter]
	}, _ *fakeRedis) {
		ctx := context.Background()
		if _, _, err := store.Create(ctx, "a", counter{Name: "a"}); err != nil {
			t.Fatal(err)
		}
		results, applied, err := store.UpdateAll(ctx, []string{"a", "fresh"}, func(current []counter, found []bool) ([]counter, bool, error) {
			if !found[0] || found[1] {
				t.Errorf("found = %v, want [true false]", found)
			}
			return incrementAll(current, found)
		})
		if err != nil || !applied || len(results) != 2 || results[0].Version != 2 || results[1].Version != 1 {
			t.Fatalf("UpdateAll = %+v applied=%v err=%v, want a at v2 and fresh created at v1", results, applied, err)
		}
		for key, want := range map[string]Versioned[counter]{"a": {Value: counter{Name: "a", Total: 1}, Version: 2}, "fresh": {Value: counter{Total: 1}, Version: 1}} {
			got, found, err := store.Get(ctx, key)
			if err != nil || !found || got != want {
				t.Fatalf("%s = %+v found=%v err=%v, want %+v", key, got, found, err, want)
			}
		}

		// 拒绝保存、mutate 出错：一个键也不写。
		declined, applied, err := store.UpdateAll(ctx, []string{"a", "fresh"}, func(current []counter, _ []bool) ([]counter, bool, error) {
			return current, false, nil
		})
		if err != nil || applied || declined[0].Version != 2 || declined[1].Version != 1 {
			t.Fatalf("a declined UpdateAll = %+v applied=%v err=%v, want the current values", declined, applied, err)
		}
		refusal := errors.New("refused")
		if _, _, err := store.UpdateAll(ctx, []string{"a", "fresh"}, func([]counter, []bool) ([]counter, bool, error) {
			return nil, false, refusal
		}); !errors.Is(err, refusal) {
			t.Fatalf("mutate error = %v, want it returned", err)
		}
		if got, _, _ := store.Get(ctx, "a"); got.Version != 2 {
			t.Fatalf("a declined / failed UpdateAll moved a to v%d", got.Version)
		}
	})
}

// 读与写之间另一个写者改了其中一个键：这一次多键写整体落空（另一个键也没写），重读后重试，
// 每个键最终只被加一次，另一个写者的修改保留。
func TestUpdateAllRetriesAsAWholeWhenAnyKeyMoved(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	other := newTokenTestStore(t, fake)
	ctx := context.Background()
	for _, key := range []string{"a", "b"} {
		if _, _, err := store.Create(ctx, key, counter{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	results, applied, err := store.UpdateAll(ctx, []string{"a", "b"}, func(current []counter, found []bool) ([]counter, bool, error) {
		calls++
		if calls == 1 {
			if _, _, err := other.Update(ctx, "b", func(c counter, _ bool) (counter, bool, error) {
				c.Total += 100
				return c, true, nil
			}); err != nil {
				t.Errorf("other writer: %v", err)
			}
		}
		return incrementAll(current, found)
	})
	if err != nil || !applied || calls != 2 {
		t.Fatalf("UpdateAll = applied=%v err=%v after %d mutate calls, want one lost attempt and one applied", applied, err, calls)
	}
	a, _, _ := store.Get(ctx, "a")
	b, _, _ := store.Get(ctx, "b")
	if a.Version != 2 || a.Value.Total != 1 || b.Version != 3 || b.Value.Total != 101 {
		t.Fatalf("a = %+v, b = %+v; want a at v2 total 1 (the lost attempt wrote nothing), b at v3 total 101", a, b)
	}
	if results[0].Version != 2 || results[1].Version != 3 {
		t.Fatalf("results = %+v", results)
	}
}

func TestUpdateAllRefusesBadArguments(t *testing.T) {
	forEachMultiStore(t, func(t *testing.T, store interface {
		Store[string, counter]
		MultiUpdater[string, counter]
	}, _ *fakeRedis) {
		ctx := context.Background()
		if _, _, err := store.UpdateAll(ctx, nil, incrementAll); err == nil {
			t.Fatal("no keys was accepted")
		}
		if _, _, err := store.UpdateAll(ctx, []string{"a", "a"}, incrementAll); err == nil {
			t.Fatal("a duplicate key was accepted")
		}
		if _, _, err := store.UpdateAll(ctx, []string{"a"}, nil); err == nil {
			t.Fatal("a nil mutate was accepted")
		}
		if _, _, err := store.UpdateAll(ctx, []string{"a", "b"}, func(current []counter, _ []bool) ([]counter, bool, error) {
			return current[:1], true, nil
		}); err == nil {
			t.Fatal("a mutate returning the wrong number of values was accepted")
		}
		if _, found, _ := store.Get(ctx, "a"); found {
			t.Fatal("a refused UpdateAll wrote a key")
		}
	})
	indexed, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		Prefix: "idx:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		Index: &RedisIndex[counter]{Key: "idx:due", Entry: func(counter) (float64, bool) { return 0, true }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := indexed.UpdateAll(context.Background(), []string{"a", "b"}, incrementAll); err == nil {
		t.Fatal("UpdateAll on an indexed store was accepted; it would leave the index behind")
	}
}

// 回复丢失：脚本已执行时按令牌认作已生效、不叠第二次；没执行时原样重发一次；执行后别人又改了其中一个键，
// 仍认作已生效；核对时读不到返回 ErrOutcomeUnknown，且 Resume 不接管（重试重新调用 mutate）。
func TestUpdateAllSettlesALostReplyByItsTokens(t *testing.T) {
	setup := func(t *testing.T) (*fakeRedis, *RedisStore[string, counter]) {
		fake := newFakeRedis()
		store := newTokenTestStore(t, fake)
		for _, key := range []string{"a", "b"} {
			if _, _, err := store.Create(context.Background(), key, counter{Name: key}); err != nil {
				t.Fatal(err)
			}
		}
		return fake, store
	}
	expect := func(t *testing.T, store *RedisStore[string, counter], why string, aTotal, bTotal int64, aVersion, bVersion uint64) {
		t.Helper()
		a, _, _ := store.Get(context.Background(), "a")
		b, _, _ := store.Get(context.Background(), "b")
		if a.Value.Total != aTotal || b.Value.Total != bTotal || a.Version != aVersion || b.Version != bVersion {
			t.Fatalf("%s: a = %+v, b = %+v; want totals %d/%d at v%d/v%d", why, a, b, aTotal, bTotal, aVersion, bVersion)
		}
	}
	ctx := context.Background()

	t.Run("ran", func(t *testing.T) {
		fake, store := setup(t)
		fake.loseReplies = 1
		if _, applied, err := store.UpdateAll(ctx, []string{"a", "b"}, incrementAll); err != nil || !applied {
			t.Fatalf("UpdateAll = applied=%v err=%v", applied, err)
		}
		expect(t, store, "script ran, reply lost", 1, 1, 2, 2)
	})
	t.Run("never ran", func(t *testing.T) {
		fake, store := setup(t)
		fake.dropWrites = 1
		if _, applied, err := store.UpdateAll(ctx, []string{"a", "b"}, incrementAll); err != nil || !applied {
			t.Fatalf("UpdateAll = applied=%v err=%v", applied, err)
		}
		expect(t, store, "script never ran, resent", 1, 1, 2, 2)
	})
	t.Run("ran, then someone wrote on top", func(t *testing.T) {
		fake, store := setup(t)
		other := newTokenTestStore(t, fake)
		fake.loseReplies = 1
		fake.afterLostReply = func() {
			fake.afterLostReply = nil
			if _, _, err := other.Update(ctx, "b", func(c counter, _ bool) (counter, bool, error) {
				c.Total += 100
				return c, true, nil
			}); err != nil {
				t.Errorf("other writer: %v", err)
			}
		}
		if _, applied, err := store.UpdateAll(ctx, []string{"a", "b"}, incrementAll); err != nil || !applied {
			t.Fatalf("UpdateAll = applied=%v err=%v", applied, err)
		}
		expect(t, store, "script ran, then b was written by someone else", 1, 101, 2, 3)
	})
	t.Run("unknown", func(t *testing.T) {
		fake, store := setup(t)
		loseReplyUnchecked(fake, false)
		_, _, err := store.UpdateAll(ctx, []string{"a", "b"}, incrementAll)
		var unknown *UnknownOutcomeError
		if !errors.Is(err, ErrOutcomeUnknown) || !errors.As(err, &unknown) {
			t.Fatalf("UpdateAll with an unreadable outcome = %v, want ErrOutcomeUnknown", err)
		}
		if resumed := Resume(ctx, err); resumed != ctx {
			t.Fatal("Resume accepted a multi-key outcome; it cannot settle one")
		}
		expect(t, store, "script ran, outcome unknown", 1, 1, 2, 2)
	})
}

func TestUpdateAllReportsConflictWhenTheBudgetIsExhausted(t *testing.T) {
	fake := newFakeRedis()
	store := newTokenTestStore(t, fake)
	ctx := context.Background()
	for _, key := range []string{"a", "b"} {
		if _, _, err := store.Create(ctx, key, counter{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	fake.failEveryCAS = true
	calls := fake.casCalls
	if _, _, err := store.UpdateAll(ctx, []string{"a", "b"}, incrementAll); !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateAll under permanent contention = %v, want ErrConflict", err)
	}
	if sent := fake.casCalls - calls; sent != DefaultMaxAttempts {
		t.Fatalf("UpdateAll sent %d compare-and-sets, want the budget of %d", sent, DefaultMaxAttempts)
	}
}

// 真实 Redis 上端到端：两个同槽的键一起更新；其中一个键在读与写之间被另一个写者改过时整体重试，
// 另一个键不会被落空的那次写改动（ROOST_REDIS_TEST_ADDR）。脚本本身的逐键语义在 fredis 的
// TestCompareAndSetAllIsAllOrNothingIntegration 里核对。
func TestUpdateAllRetriesAsAWholeOnRedisIntegration(t *testing.T) {
	client := integrationVersionRedis(t)
	prefix := "vs-update-all:" + strconv.FormatInt(time.Now().UnixNano(), 10) + ":{t}:"
	newStore := func() *RedisStore[string, counter] {
		store, err := NewRedisStore[string, counter](evalRedis{client: client}, RedisConfig[string, counter]{
			Prefix: prefix, KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	store, other := newStore(), newStore()
	ctx := context.Background()
	t.Cleanup(func() { client.Del(context.Background(), prefix+"a", prefix+"b") })
	for _, key := range []string{"a", "b"} {
		if _, _, err := store.Create(ctx, key, counter{Name: key}); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	if _, applied, err := store.UpdateAll(ctx, []string{"a", "b"}, func(current []counter, found []bool) ([]counter, bool, error) {
		calls++
		if calls == 1 {
			if _, _, err := other.Update(ctx, "b", func(c counter, _ bool) (counter, bool, error) {
				c.Total += 100
				return c, true, nil
			}); err != nil {
				t.Errorf("other writer: %v", err)
			}
		}
		return incrementAll(current, found)
	}); err != nil || !applied || calls != 2 {
		t.Fatalf("UpdateAll on Redis = applied=%v err=%v after %d calls", applied, err, calls)
	}
	a, _, _ := store.Get(ctx, "a")
	b, _, _ := store.Get(ctx, "b")
	if a.Version != 2 || a.Value.Total != 1 || b.Version != 3 || b.Value.Total != 101 {
		t.Fatalf("on Redis a = %+v, b = %+v; want a at v2 total 1, b at v3 total 101", a, b)
	}
}
