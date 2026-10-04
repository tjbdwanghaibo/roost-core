package security

import (
	"math"
	"testing"
	"time"
)

// RR-20261004-NC-05：不可满足的需求不能占 key、续 idle 活性或计作 key 容量拒绝。
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
