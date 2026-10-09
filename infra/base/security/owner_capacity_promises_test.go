// RR-20261005-NC-82：key 表的容量不能被一个主体用完，满表拒绝不能变成全表扫描放大器。
package security

import (
	"fmt"
	"testing"
	"time"
)

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

// 满表后每个陌生 key 的成本应与 MaxKeys 无关（修前在全局锁内 O(MaxKeys) 扫描）。
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
