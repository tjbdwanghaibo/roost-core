package security

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 2, Refill: 1, Interval: time.Second})
	limiter.now = func() time.Time { return now }
	key := RateLimitKey{OwnerID: 1, Action: 7}
	if !limiter.Allow(key) || !limiter.Allow(key) {
		t.Fatal("first two requests should pass")
	}
	if limiter.Allow(key) {
		t.Fatal("third request should be limited")
	}
	now = now.Add(time.Second)
	if !limiter.Allow(key) {
		t.Fatal("request after refill should pass")
	}
}

func TestRateLimiterBoundsKeyCardinality(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		Capacity:      2,
		Refill:        1,
		Interval:      time.Second,
		MaxKeys:       2,
		IdleTTL:       time.Hour,
		SweepInterval: time.Minute,
	})
	limiter.now = func() time.Time { return now }
	if !limiter.Allow(RateLimitKey{OwnerID: 1}) || !limiter.Allow(RateLimitKey{OwnerID: 2}) {
		t.Fatal("keys within configured cardinality should pass")
	}
	if limiter.Allow(RateLimitKey{OwnerID: 3}) {
		t.Fatal("new key over configured cardinality must fail closed")
	}
	stats := limiter.Stats()
	if stats.Keys != 2 || stats.MaxKeys != 2 || stats.CapacityRejected != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRateLimiterAutomaticallyReclaimsIdleKeys(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		Capacity:      1,
		MaxKeys:       1,
		IdleTTL:       time.Second,
		SweepInterval: time.Second,
	})
	limiter.now = func() time.Time { return now }
	if !limiter.Allow(RateLimitKey{OwnerID: 1}) {
		t.Fatal("first key should pass")
	}
	now = now.Add(time.Second)
	if !limiter.Allow(RateLimitKey{OwnerID: 2}) {
		t.Fatal("new key should pass after the idle key is reclaimed")
	}
	stats := limiter.Stats()
	if stats.Keys != 1 || stats.Evicted != 1 || stats.CapacityRejected != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRateLimiterActivityExtendsIdleLifetime(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{
		Capacity:      10,
		MaxKeys:       1,
		IdleTTL:       time.Second,
		SweepInterval: time.Second,
	})
	limiter.now = func() time.Time { return now }
	key := RateLimitKey{OwnerID: 1}
	if !limiter.Allow(key) {
		t.Fatal("first request should pass")
	}
	now = now.Add(750 * time.Millisecond)
	if !limiter.Allow(key) {
		t.Fatal("active key should remain available")
	}
	now = now.Add(750 * time.Millisecond)
	if limiter.Allow(RateLimitKey{OwnerID: 2}) {
		t.Fatal("recently active key must not be evicted")
	}
}

// The bucket arithmetic is golang.org/x/time/rate: refill is continuous, so a
// key that spent its burst gets Refill/Interval back per unit time rather than
// waiting for a whole Interval to elapse.
func TestRateLimiterRefillsContinuously(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 2, Refill: 2, Interval: time.Second})
	limiter.now = func() time.Time { return now }
	key := RateLimitKey{OwnerID: 1}
	if !limiter.AllowN(key, 2) {
		t.Fatal("burst should pass")
	}
	if limiter.Allow(key) {
		t.Fatal("bucket should be empty right after the burst")
	}
	now = now.Add(500 * time.Millisecond)
	if !limiter.Allow(key) {
		t.Fatal("half an interval at 2 tokens/interval should have refilled one token")
	}
	if limiter.Allow(key) {
		t.Fatal("only one token should have refilled")
	}
}

// A request larger than the burst can never be satisfied; it is refused
// without consuming what is there, and non-positive requests are free.
func TestRateLimiterRefusesOversizedRequestsWithoutConsuming(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 3, Refill: 1, Interval: time.Hour})
	limiter.now = func() time.Time { return now }
	key := RateLimitKey{OwnerID: 1}
	if limiter.AllowN(key, 4) {
		t.Fatal("a request above the burst must be refused")
	}
	if !limiter.AllowN(key, 0) || !limiter.AllowN(key, -1) {
		t.Fatal("non-positive requests are always allowed")
	}
	if !limiter.AllowN(key, 3) {
		t.Fatal("the refused oversized request must not have consumed tokens")
	}
	var nilLimiter *RateLimiter
	if !nilLimiter.Allow(key) || nilLimiter.Size() != 0 {
		t.Fatal("a nil limiter allows everything")
	}
}

// 不可满足的需求不能占 key、续 idle 活性或计作 key 容量拒绝。
func TestOversizedDemandHasNoKeySideEffects(t *testing.T) {
	t.Run("new_key", func(t *testing.T) {
		l := NewRateLimiter(RateLimitConfig{Capacity: 1, MaxKeys: 1})
		if l.AllowN(RateLimitKey{OwnerID: 1}, 2) {
			t.Fatal("oversized demand admitted")
		}
		if s := l.Stats(); s.Keys != 0 || s.CapacityRejected != 0 {
			t.Fatalf("rejected demand changed key stats: %+v", s)
		}
		if !l.Allow(RateLimitKey{OwnerID: 2}) {
			t.Fatal("valid key blocked by rejected demand")
		}
	})
	t.Run("full_table", func(t *testing.T) {
		l := NewRateLimiter(RateLimitConfig{Capacity: 1, MaxKeys: 1})
		l.Allow(RateLimitKey{OwnerID: 1})
		l.AllowN(RateLimitKey{OwnerID: 2}, 2)
		if s := l.Stats(); s.CapacityRejected != 0 {
			t.Fatalf("oversized demand counted as key rejection: %+v", s)
		}
		if l.Allow(RateLimitKey{OwnerID: 2}) || l.Stats().CapacityRejected != 1 {
			t.Fatal("valid demand did not record real key capacity rejection")
		}
	})
	t.Run("idle_key", func(t *testing.T) {
		l := NewRateLimiter(RateLimitConfig{Capacity: 1, IdleTTL: time.Hour, SweepInterval: time.Hour})
		now := time.Unix(1000, 0)
		l.now = func() time.Time { return now }
		key := RateLimitKey{OwnerID: 1}
		l.Allow(key)
		now = now.Add(30 * time.Minute)
		l.AllowN(key, 2)
		now = now.Add(30 * time.Minute)
		if removed := l.GC(time.Hour); removed != 1 {
			t.Fatalf("oversized demand refreshed idle activity: removed=%d", removed)
		}
	})
	t.Run("clamped_burst", func(t *testing.T) {
		l := NewRateLimiter(RateLimitConfig{Capacity: math.MaxInt64})
		if l.AllowN(RateLimitKey{}, int64(math.MaxInt32)+1) || l.Size() != 0 {
			t.Fatal("clamped burst demand occupied key")
		}
	})
	t.Run("nonpositive_and_nil", func(t *testing.T) {
		var disabled *RateLimiter
		if !disabled.AllowN(RateLimitKey{}, math.MaxInt64) {
			t.Fatal("nil limiter changed")
		}
		l := NewRateLimiter(RateLimitConfig{})
		if !l.AllowN(RateLimitKey{}, 0) || !l.AllowN(RateLimitKey{}, -1) || l.Size() != 0 {
			t.Fatal("nonpositive demand changed")
		}
	})
	t.Run("existing_tokens", func(t *testing.T) {
		l := NewRateLimiter(RateLimitConfig{Capacity: 2, Refill: 1, Interval: time.Hour})
		key := RateLimitKey{OwnerID: 1}
		if !l.Allow(key) || l.AllowN(key, 3) || !l.Allow(key) || l.Allow(key) {
			t.Fatal("oversized demand changed existing tokens")
		}
	})
}

// key 表的容量不能被一个主体用完，满表拒绝不能变成全表扫描放大器。

// 一个主体改变 Action（如客户端可控的 MessageID）只能占用自己的名额，
// 其他主体的首个请求不受影响。默认上限下即可成立，无需调用方另行配置。
func TestOneOwnerCannotFillTheKeyTableForOthers(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 5, MaxKeys: 1000, IdleTTL: time.Hour})
	limiter.now = func() time.Time { return now }
	admitted := 0
	for action := uint32(1); action <= 1000; action++ {
		if limiter.Allow(RateLimitKey{OwnerID: 1, Action: action}) {
			admitted++
		}
	}
	if !limiter.Allow(RateLimitKey{OwnerID: 2, Action: 1}) {
		t.Fatalf("another owner's first request was refused after owner 1 admitted %d actions: %+v", admitted, limiter.Stats())
	}
	if admitted >= 1000 {
		t.Fatalf("owner 1 admitted all %d distinct actions: its key count is unbounded", admitted)
	}
	// 已有 key 不受上限影响：超出上限的主体仍能继续使用已建立的动作。
	if !limiter.Allow(RateLimitKey{OwnerID: 1, Action: 1}) {
		t.Fatal("an owner at its key limit lost an action it already had")
	}
}

// 显式上限、分开计数，以及闲置回收后名额归还给同一主体。
func TestOwnerKeyLimitIsCountedAndReleasedByIdleSweep(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 5, MaxKeys: 100, MaxKeysPerOwner: 2, IdleTTL: time.Second, SweepInterval: time.Second})
	limiter.now = func() time.Time { return now }
	if !limiter.Allow(RateLimitKey{OwnerID: 7, Action: 1}) || !limiter.Allow(RateLimitKey{OwnerID: 7, Action: 2}) {
		t.Fatal("actions within the owner limit were refused")
	}
	if limiter.Allow(RateLimitKey{OwnerID: 7, Action: 3}) {
		t.Fatal("a third action passed an owner limit of 2")
	}
	st := limiter.Stats()
	if st.Keys != 2 || st.MaxKeysPerOwner != 2 || st.OwnerCapacityRejected != 1 || st.CapacityRejected != 0 {
		t.Fatalf("stats at the owner limit: %+v", st)
	}
	now = now.Add(time.Second) // 两个 key 闲置满 IdleTTL，周期清扫回收并归还名额
	if !limiter.Allow(RateLimitKey{OwnerID: 7, Action: 3}) || !limiter.Allow(RateLimitKey{OwnerID: 7, Action: 4}) {
		t.Fatalf("the owner did not get its slots back after the idle sweep: %+v", limiter.Stats())
	}
	if limiter.Allow(RateLimitKey{OwnerID: 7, Action: 5}) {
		t.Fatal("the owner limit was not enforced again after the sweep")
	}
	if got := NewRateLimiter(RateLimitConfig{MaxKeys: 10}).Stats().MaxKeysPerOwner; got != 10 {
		t.Fatalf("an owner limit above MaxKeys was not clamped: %d", got)
	}
	if got := NewRateLimiter(RateLimitConfig{}).Stats().MaxKeysPerOwner; got != DefaultMaxKeysPerOwner {
		t.Fatalf("default owner limit = %d", got)
	}
}

// 满表时陌生 key 立即拒绝，不在全局锁内额外全表扫描：已闲置的 key 由周期清扫
// （每 SweepInterval 至多一次）回收，满表路径不会让清扫频率随请求速率增长。
func TestFullTableRejectionDoesNotSweepBeforeTheSweepInterval(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := NewRateLimiter(RateLimitConfig{Capacity: 5, MaxKeys: 4, IdleTTL: 1200 * time.Millisecond, SweepInterval: time.Second})
	limiter.now = func() time.Time { return now }
	for owner := int64(1); owner <= 3; owner++ { // t=100.0，同时完成首次清扫
		if !limiter.Allow(RateLimitKey{OwnerID: owner}) {
			t.Fatalf("owner %d refused while the table had room", owner)
		}
	}
	now = now.Add(500 * time.Millisecond) // t=100.5，表满
	if !limiter.Allow(RateLimitKey{OwnerID: 4}) {
		t.Fatal("owner 4 refused while the table had room")
	}
	now = now.Add(500 * time.Millisecond) // t=101.0，周期清扫：此时没有闲置 key
	limiter.Allow(RateLimitKey{OwnerID: 4})
	now = now.Add(500 * time.Millisecond) // t=101.5，owner 1～3 已闲置 1.5s，距上次清扫 0.5s
	if limiter.Allow(RateLimitKey{OwnerID: 5}) {
		t.Fatalf("the full-table path swept before the sweep interval: %+v", limiter.Stats())
	}
	if st := limiter.Stats(); st.Keys != 4 || st.Evicted != 0 || st.CapacityRejected != 1 {
		t.Fatalf("stats after the full-table rejection: %+v", st)
	}
	now = now.Add(500 * time.Millisecond) // t=102.0，距上次清扫满 1s，周期清扫回收 owner 1～3
	if !limiter.Allow(RateLimitKey{OwnerID: 5}) {
		t.Fatalf("idle keys were not reclaimed by the periodic sweep: %+v", limiter.Stats())
	}
	if st := limiter.Stats(); st.Keys != 2 || st.Evicted != 3 {
		t.Fatalf("stats after the periodic sweep: %+v", st)
	}
}

// 满表后每个陌生 key 的成本应与 MaxKeys 无关（不在全局锁内 O(MaxKeys) 扫描）。
func BenchmarkRateLimiterFullTableUnseenKey(b *testing.B) {
	for _, keys := range []int{1_000, 100_000} {
		b.Run(fmt.Sprintf("keys=%d", keys), func(b *testing.B) {
			limiter := NewRateLimiter(RateLimitConfig{Capacity: 5, MaxKeys: keys, IdleTTL: time.Hour})
			for owner := 0; owner < keys; owner++ {
				limiter.Allow(RateLimitKey{OwnerID: int64(owner + 1)})
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				limiter.Allow(RateLimitKey{OwnerID: -int64(i + 1)})
			}
		})
	}
}
