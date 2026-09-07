package security

import (
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
