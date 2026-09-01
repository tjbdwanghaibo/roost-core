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
