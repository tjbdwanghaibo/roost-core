package versionstore

// RR-20261005-NC-52：RedisStore.Update 输掉 compare-and-set 后先退避、再把“输掉那一刻”
// CompareAndSet 带回的旧值交给下一次 mutate。退避的目的是让竞争者先写完（
// DefaultRetryBackoff 的注释：让同键写者错开、不要一起耗尽预算）；竞争者恰好在退避
// 窗口里写了，下一次 CAS 用的就是退避前的值，必输。每个窗口一次竞争写的稳定负载下
// 8 次预算全部浪费，调用方拿到 ErrConflict（kit/service/rank 的循环是“退避后重读”，
// 这里不是）。真实 Redis 6～8 个并发写者已能观察到伪冲突，chat 世界频道最先撞到。
//
// 承诺：退避之后的重试基于退避之后的存储状态；竞争者只在退避窗口里写时，第二次
// 尝试就成功，竞争写一个不丢。

import (
	"context"
	"testing"
	"time"
)

func TestARetryAfterBackoffSeesWritesThatLandedDuringTheBackoff(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedis()
	newStore := func(sleep func(time.Duration)) *RedisStore[string, int] {
		store, err := NewRedisStore(fake, RedisConfig[string, int]{
			Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{},
			RetryBackoff: time.Millisecond, Sleep: sleep,
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	competitor := newStore(func(time.Duration) {})
	competitorWrites := 0
	compete := func() {
		if _, _, err := competitor.Update(ctx, "k", func(v int, _ bool) (int, bool, error) { return v + 1000, true, nil }); err != nil {
			t.Fatalf("competitor write: %v", err)
		}
		competitorWrites++
	}
	if _, _, err := competitor.Create(ctx, "k", 0); err != nil {
		t.Fatal(err)
	}

	// The competitor takes its turn while the loser backs off — which is
	// exactly what the backoff is for — once per backoff window.
	sleeps := 0
	loser := newStore(func(time.Duration) { sleeps++; compete() })
	mutates := 0
	stored, saved, err := loser.Update(ctx, "k", func(v int, _ bool) (int, bool, error) {
		mutates++
		if mutates == 1 {
			compete() // a write between this attempt's read and its compare-and-set
		}
		return v + 1, true, nil
	})
	if err != nil {
		t.Fatalf("Update after %d attempts (%d backoffs, %d competitor writes) failed: %v", mutates, sleeps, competitorWrites, err)
	}
	if !saved || mutates != 2 || sleeps != 1 {
		t.Fatalf("saved=%v after %d attempts and %d backoffs; want saved on the 2nd attempt after 1 backoff", saved, mutates, sleeps)
	}
	final, found, err := loser.Get(ctx, "k")
	if err != nil || !found {
		t.Fatalf("read back: found=%v err=%v", found, err)
	}
	if want := 1 + 1000*competitorWrites; final.Value != want || stored.Value != want || final.Version != stored.Version {
		t.Fatalf("final %+v, returned %+v; want value %d (one own increment, %d competitor writes)", final, stored, want, competitorWrites)
	}
}

// A lost compare-and-set with no write during the backoff still needs exactly
// one more attempt, and a deleted key is seen as absent on the retry.
func TestARetryAfterBackoffSeesADeletionAsAbsence(t *testing.T) {
	ctx := context.Background()
	fake := newFakeRedis()
	store, err := NewRedisStore(fake, RedisConfig[string, int]{
		Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{},
		RetryBackoff: time.Millisecond,
		Sleep: func(time.Duration) {
			if _, err := fake.Del(ctx, "nc52:k"); err != nil {
				t.Fatal(err)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(ctx, "k", 5); err != nil {
		t.Fatal(err)
	}
	other, err := NewRedisStore(fake, RedisConfig[string, int]{Prefix: "nc52:", KeyOf: func(k string) string { return k }, Codec: JSONCodec[int]{}})
	if err != nil {
		t.Fatal(err)
	}
	var seen []bool
	_, _, err = store.Update(ctx, "k", func(v int, found bool) (int, bool, error) {
		seen = append(seen, found)
		if len(seen) == 1 {
			if _, _, err := other.Update(ctx, "k", func(v int, _ bool) (int, bool, error) { return v + 1, true, nil }); err != nil {
				t.Fatal(err)
			}
		}
		return v + 1, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("mutate saw found=%v; want present then absent (deleted during the backoff)", seen)
	}
	got, found, err := store.Get(ctx, "k")
	if err != nil || !found || got.Value != 1 || got.Version != 1 {
		t.Fatalf("recreated value %+v found=%v err=%v; want a fresh key at version 1", got, found, err)
	}
}
