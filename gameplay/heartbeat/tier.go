package heartbeat

import (
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/misc"
)

// Resolution 是驱动的一格：每 10ms 推进一格，每格把到期的登记一次交出（维护者 2026-10-21 压测后决定）。
const Resolution = 10 * time.Millisecond

// Periods 是全部固定档位（维护者决定 ⑥）：50ms / 100ms / 200ms / 500ms / 1s / 2s / 5s / 10s / 30s / 60s。
// 不支持任意周期；都是 Resolution 的整数倍，也都整除最长档。
var Periods = [numTiers]time.Duration{
	50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second,
}

const numTiers = 10

// tier 是 Periods 的下标。
type tier uint8

// maxPeriod 是最长档；相位先对它的格数（6000）取模，再对本档格数取模，各档按同一相位对齐。
const maxPeriod = 60 * time.Second

var (
	tierFrames  [numTiers]int64
	phaseFrames = uint64(maxPeriod / Resolution)
)

func init() {
	for i, p := range Periods {
		if p%Resolution != 0 || maxPeriod%p != 0 {
			panic(fmt.Sprintf("heartbeat: period %v is not a multiple of %v or does not divide %v", p, Resolution, maxPeriod))
		}
		tierFrames[i] = int64(p / Resolution)
	}
}

// tierOf 返回周期对应的档位；不是固定档位之一时报错，并列出可选值。
func tierOf(period time.Duration) (tier, error) {
	for i, p := range Periods {
		if p == period {
			return tier(i), nil
		}
	}
	return 0, fmt.Errorf("%w: %v (supported: %v)", ErrUnsupportedPeriod, period, Periods)
}

func (t tier) period() time.Duration { return Periods[t] }

// phaseHash 是按实体 ID 算一次的 64 位散列（SplitMix64，与 Nest 的 worker 散列同一函数）。同一实体的全部登记
// 共用它，所以同档的登记落在同一格；实体重载后相位不变。
func phaseHash(entityID int64) uint64 { return misc.Hash64(uint64(entityID)) }

// cellOf 是登记在本档环上的格：相位 = hash mod 6000，再对本档格数取模。登记只进这一格，环每转到它触发一次。
func cellOf(hash uint64, t tier) int64 {
	return int64(hash % phaseFrames % uint64(tierFrames[t]))
}

// tierRing 是按档分环的调度结构（原型 internal/bench 的 TierRing 升级而来，压测见
// docs/performance/HB-STRUCTURES-20261021.md）：每档一个环，环长 = 该档格数；登记只放进一格，格是切片、
// 登记记自己的下标，撤销 O(1)（与末尾交换）。每推进一格访问 10 个档各自的当前格。
//
// 不加锁：只在 Driver.mu 下使用。
type tierRing struct {
	cells [numTiers][][]*Reg
	frame int64
	count int
}

func newTierRing() *tierRing {
	r := &tierRing{}
	for t := range numTiers {
		r.cells[t] = make([][]*Reg, tierFrames[t])
	}
	return r
}

func (r *tierRing) insert(reg *Reg) {
	t := reg.ring.tier
	reg.ring.cell = int32(cellOf(reg.host.hash, t))
	cell := &r.cells[t][reg.ring.cell]
	reg.ring.idx = int32(len(*cell))
	*cell = append(*cell, reg)
	r.count++
}

func (r *tierRing) remove(reg *Reg) {
	cell := &r.cells[reg.ring.tier][reg.ring.cell]
	last := len(*cell) - 1
	moved := (*cell)[last]
	(*cell)[reg.ring.idx] = moved
	moved.ring.idx = reg.ring.idx
	(*cell)[last] = nil
	*cell = (*cell)[:last]
	r.count--
}

// step 推进一格，把这一格到期的登记追加到 out 返回。
func (r *tierRing) step(out []*Reg) []*Reg {
	r.frame++
	for t := range numTiers {
		out = append(out, r.cells[t][r.frame%tierFrames[t]]...)
	}
	return out
}
