package security

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type RateLimitKey struct {
	OwnerID int64
	Action  uint32
}

// DefaultMaxKeysPerOwner 是每个 OwnerID 默认可占用的 key 数。
//
// gateway.RateLimit 以 {PlayerID, MessageID} 为 key，合法玩家的 key 数不超过它在
// IdleTTL 内用到的请求协议数：game-demo 模板共 29 个请求 MessageID，协议生成器只要求
// ID 唯一、不限范围，所以协议号本身给不出上界。256 约为 demo 的 9 倍，留足大型游戏的
// 协议余量；同时单个主体最多占默认表（MaxKeys=100000）的 0.256%，要占满默认表需要
// 至少 391 个同时活跃、各自用满上限的已认证主体，这已是容量本身不足而非单点滥用
// （RR-20261005-NC-82）。
const DefaultMaxKeysPerOwner = 256

type RateLimitConfig struct {
	Capacity int64
	Refill   int64
	Interval time.Duration
	// MaxKeys 是全部主体共享的 key 表上限。
	MaxKeys int
	// MaxKeysPerOwner 是单个 OwnerID 可同时持有的 key 数，<=0 取
	// DefaultMaxKeysPerOwner，超过 MaxKeys 时按 MaxKeys 计。超出后只拒绝该主体
	// 尚未建立的新 key，已有 key 照常限速，其他主体不受影响。
	MaxKeysPerOwner int
	IdleTTL         time.Duration
	// SweepInterval 是闲置 key 清扫的最小间隔，也是满表时闲置名额最晚的回收延迟。
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
	if c.MaxKeysPerOwner <= 0 {
		c.MaxKeysPerOwner = DefaultMaxKeysPerOwner
	}
	if c.MaxKeysPerOwner > c.MaxKeys {
		c.MaxKeysPerOwner = c.MaxKeys
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
	Keys            int
	MaxKeys         int
	MaxKeysPerOwner int
	// CapacityRejected 统计因共享表满被拒绝的新 key。
	CapacityRejected uint64
	// OwnerCapacityRejected 统计因该主体自己的 key 达到上限被拒绝的新 key。
	OwnerCapacityRejected uint64
	Evicted               uint64
}

// RateLimiter is a keyed token bucket. The bucket arithmetic is
// golang.org/x/time/rate (continuous refill: Refill tokens spread evenly over
// Interval, burst Capacity); what this type adds is the part x/time/rate does
// not have — one bucket per key with a bounded key set, idle eviction and
// stats, so attacker-controlled key cardinality cannot grow memory.
//
// 容量按两层界定（RR-20261005-NC-82）：每个 OwnerID 至多 MaxKeysPerOwner 个 key，
// 一个主体变化 Action 只会耗尽自己的名额；全表至多 MaxKeys 个 key，满表时陌生 key
// 立即拒绝（O(1)），闲置 key 只由每 SweepInterval 至多一次的清扫回收，拒绝路径不会
// 让全表扫描频率随请求速率增长。
type RateLimiter struct {
	cfg    RateLimitConfig
	limit  rate.Limit
	burst  int
	mu     sync.Mutex
	bkt    map[RateLimitKey]*bucket
	owners map[int64]int // OwnerID → 该主体当前持有的 key 数，随 bkt 同步增删
	now    func() time.Time

	lastSweep             time.Time
	capacityRejected      uint64
	ownerCapacityRejected uint64
	evicted               uint64
}

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	cfg = cfg.Normalize()
	return &RateLimiter{
		cfg:    cfg,
		limit:  rate.Limit(float64(cfg.Refill) / cfg.Interval.Seconds()),
		burst:  clampInt(cfg.Capacity),
		bkt:    make(map[RateLimitKey]*bucket),
		owners: make(map[int64]int),
		now:    time.Now,
	}
}

func clampInt(v int64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < 0 {
		return 0
	}
	return int(v)
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
	// RR-20261004-NC-05：不可满足的需求不占 key，也不刷新 idle 活性。
	// 先按实际 burst 拒绝，避免把非法需求误计为 key 容量耗尽。
	if n > int64(l.burst) {
		return false
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
		// 新 key 先过主体自己的上限，再过共享表上限；活跃 key 从不为新 key 让位。
		// 满表时不再额外扫描：闲置 key 等上面的周期清扫回收，最晚晚一个 SweepInterval。
		if l.owners[key.OwnerID] >= l.cfg.MaxKeysPerOwner {
			l.ownerCapacityRejected++
			return false
		}
		if len(l.bkt) >= l.cfg.MaxKeys {
			l.capacityRejected++
			return false
		}
		b = &bucket{limiter: rate.NewLimiter(l.limit, l.burst), lastSeen: now}
		l.bkt[key] = b
		l.owners[key.OwnerID]++
	}
	b.lastSeen = now
	return b.limiter.AllowN(now, int(n))
}

func (l *RateLimiter) Stats() RateLimitStats {
	if l == nil {
		return RateLimitStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return RateLimitStats{
		Keys:                  len(l.bkt),
		MaxKeys:               l.cfg.MaxKeys,
		MaxKeysPerOwner:       l.cfg.MaxKeysPerOwner,
		CapacityRejected:      l.capacityRejected,
		OwnerCapacityRejected: l.ownerCapacityRejected,
		Evicted:               l.evicted,
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
			if l.owners[key.OwnerID] <= 1 {
				delete(l.owners, key.OwnerID)
			} else {
				l.owners[key.OwnerID]--
			}
			removed++
		}
	}
	return removed
}
