package security

import (
	"sync"
	"time"
)

type RateLimitKey struct {
	OwnerID int64
	Action  uint32
}

type RateLimitConfig struct {
	Capacity      int64
	Refill        int64
	Interval      time.Duration
	MaxKeys       int
	IdleTTL       time.Duration
	SweepInterval time.Duration
}

func (c RateLimitConfig) Normalize() RateLimitConfig {
	if c.Capacity <= 0 {
		c.Capacity = 20
	}
	if c.Refill <= 0 {
		c.Refill = c.Capacity
	}
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.MaxKeys <= 0 {
		c.MaxKeys = 100_000
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = 10 * time.Minute
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = time.Minute
	}
	if c.SweepInterval > c.IdleTTL {
		c.SweepInterval = c.IdleTTL
	}
	return c
}

type RateLimitStats struct {
	Keys             int
	MaxKeys          int
	CapacityRejected uint64
	Evicted          uint64
}

type RateLimiter struct {
	cfg RateLimitConfig
	mu  sync.Mutex
	bkt map[RateLimitKey]*bucket
	now func() time.Time

	lastSweep        time.Time
	capacityRejected uint64
	evicted          uint64
}

type bucket struct {
	tokens   int64
	at       time.Time
	lastSeen time.Time
}

func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	cfg = cfg.Normalize()
	return &RateLimiter{
		cfg: cfg,
		bkt: make(map[RateLimitKey]*bucket),
		now: time.Now,
	}
}

func (l *RateLimiter) Allow(key RateLimitKey) bool {
	return l.AllowN(key, 1)
}

func (l *RateLimiter) AllowN(key RateLimitKey, n int64) bool {
	if l == nil {
		return true
	}
	if n <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSweep.IsZero() || now.Sub(l.lastSweep) >= l.cfg.SweepInterval {
		l.evicted += uint64(l.gcLocked(now.Add(-l.cfg.IdleTTL)))
		l.lastSweep = now
	}
	b := l.bkt[key]
	if b == nil {
		if len(l.bkt) >= l.cfg.MaxKeys {
			// A key flood must not grow the map without bound. Try one full idle
			// sweep before rejecting a previously unseen key; active keys are
			// never evicted to make room for attacker-controlled cardinality.
			l.evicted += uint64(l.gcLocked(now.Add(-l.cfg.IdleTTL)))
		}
		if len(l.bkt) >= l.cfg.MaxKeys {
			l.capacityRejected++
			return false
		}
		b = &bucket{tokens: l.cfg.Capacity, at: now, lastSeen: now}
		l.bkt[key] = b
	}
	b.lastSeen = now
	l.refill(b, now)
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

func (l *RateLimiter) Stats() RateLimitStats {
	if l == nil {
		return RateLimitStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return RateLimitStats{
		Keys:             len(l.bkt),
		MaxKeys:          l.cfg.MaxKeys,
		CapacityRejected: l.capacityRejected,
		Evicted:          l.evicted,
	}
}

func (l *RateLimiter) Size() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bkt)
}

func (l *RateLimiter) GC(idle time.Duration) int {
	if l == nil || idle <= 0 {
		return 0
	}
	cutoff := l.now().Add(-idle)
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := l.gcLocked(cutoff)
	l.evicted += uint64(removed)
	return removed
}

func (l *RateLimiter) gcLocked(cutoff time.Time) int {
	removed := 0
	for key, b := range l.bkt {
		if !b.lastSeen.After(cutoff) {
			delete(l.bkt, key)
			removed++
		}
	}
	return removed
}

func (l *RateLimiter) refill(b *bucket, now time.Time) {
	if b == nil {
		return
	}
	if b.at.IsZero() {
		b.at = now
	}
	elapsed := now.Sub(b.at)
	if elapsed < l.cfg.Interval {
		return
	}
	steps := int64(elapsed / l.cfg.Interval)
	add := steps * l.cfg.Refill
	b.tokens += add
	if b.tokens > l.cfg.Capacity {
		b.tokens = l.cfg.Capacity
	}
	b.at = b.at.Add(time.Duration(steps) * l.cfg.Interval)
}
