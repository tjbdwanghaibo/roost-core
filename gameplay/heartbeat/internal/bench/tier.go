// Package bench 是 D20 心跳（HB）压测用的**原型**，不是正式框架，不被任何生产代码引用。
//
// 维护者 2026-10-21 决定 ②⑪：投递方式与调度结构由压测决定。本包实现四种调度结构供微基准对照
// （sched_bench_test.go），并在测试里用真实 Nest 引擎对照三种投递方式（delivery_test.go）。
// 结论见 docs/performance/HB-STRUCTURES-20261021.md。原型只追求“各结构按各自思路写得合理、可比”，
// 不含正式实现必须有的实例核对、令牌、事务内启停延后、指标等。
//
// 已定前提（不在本包比较）：固定 10 档、同一实体的多个登记不合并、跳拍打 Warn 并计数、至多一拍在途。
package bench

import (
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/misc"
)

// Tier 是固定档位的下标（0..NumTiers-1），对应 Periods。
type Tier uint8

// NumTiers 是固定档位数。
const NumTiers = 10

// Periods 是维护者决定 ⑥ 的固定档位：50ms / 100ms / 200ms / 500ms / 1s / 2s / 5s / 10s / 30s / 60s。
// 都是 50ms 的整数倍，也都整除 60s，ssr 的多环结构依赖这两条。
var Periods = [NumTiers]time.Duration{
	50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second,
}

// 常用档位下标。
const (
	Tier50ms  Tier = 0
	Tier100ms Tier = 1
	Tier200ms Tier = 2
	Tier1s    Tier = 4
	Tier60s   Tier = 9
)

// Period 返回档位周期。
func (t Tier) Period() time.Duration { return Periods[t] }

// MaxPeriod 是最长档位，决定 ssr 相位模数（1200 个 50ms 帧）与时间轮长度（6000 个 10ms 格）。
const MaxPeriod = 60 * time.Second

// PhaseHash 是按实体 ID 计算一次的 64 位散列；各结构按自己的分辨率取模得到相位。
// 用 SplitMix64（与 Nest 的 worker 散列同一函数），ssr 用的是自己的 Hash64，效果同类。
func PhaseHash(entityID int64) uint64 { return misc.Hash64(uint64(entityID)) }

// framesOf 返回周期在分辨率 res 下的格数；周期必须是 res 的整数倍。
func framesOf(period, res time.Duration) int {
	if period%res != 0 {
		panic("bench: period is not a multiple of the resolution")
	}
	return int(period / res)
}

// firstFire 返回 cur 之后第一个满足 f mod p == phase 的格号（严格大于 cur）：首拍落在 (0, 周期] 内，
// 之后每 p 格一次。四种结构都用这条规则，保证同一登记在各结构上的触发时刻相同（分辨率相同时）。
func firstFire(cur int64, p int, phase int) int64 {
	next := cur + 1
	d := (int64(phase) - next%int64(p) + int64(p)) % int64(p)
	return next + d
}

// Scheduler 是四种调度结构的共同接口，H 是各自的登记句柄（撤销 / 改档用）。
// 全部方法只由驱动 goroutine 调用（结构本身不加锁；分片时每片一个结构）。
type Scheduler[H any] interface {
	// Add 登记一个周期项；id 是 Step 返回的键（原型里是登记序号），hash 是 PhaseHash(实体 ID)。
	Add(id int64, hash uint64, tier Tier) H
	// Remove 撤销登记。
	Remove(h H)
	// SetTier 改档：等价于撤销 + 按新档重新登记（相位规则不变），返回新句柄。
	SetTier(h H, tier Tier) H
	// Step 推进一格，把本格到期的 id 追加到 out 并返回。
	Step(out []int64) []int64
	// Resolution 是一格的时长。
	Resolution() time.Duration
	// Len 是当前登记数。
	Len() int
}
