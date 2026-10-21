package bench

import "time"

// TierRing 是 ssr 思路（固定档位 + 按相位分桶）的改写：每档一个环，环长 = 该档的格数，
// 登记只放进一格（相位 mod 环长），每格是切片、句柄记下标，撤销 O(1)（与末尾交换）。
// 每格推进时访问 10 个档各自的当前格。分辨率可选：50ms 与 ssr 的帧相同；10ms 把同档登记打散到 5 倍的格上。
//
// 与 SSRRing 的差别只在实现：触发时刻（分辨率 50ms 时）与 SSRRing 逐格相同，不再一项占多格、不用 map。
type TierRing struct {
	res    time.Duration
	frames [NumTiers]int
	cells  [NumTiers][][]*TierEntry
	frame  int64
	phaseM uint64
	count  int
}

// TierEntry 是 TierRing 的登记句柄。
type TierEntry struct {
	id   int64
	hash uint64
	tier Tier
	cell int32
	idx  int32
}

// NewTierRing 构造按档分环的结构；res 必须整除每个档位。
func NewTierRing(res time.Duration) *TierRing {
	r := &TierRing{res: res, phaseM: uint64(framesOf(MaxPeriod, res))}
	for t := range NumTiers {
		r.frames[t] = framesOf(Periods[t], res)
		r.cells[t] = make([][]*TierEntry, r.frames[t])
	}
	return r
}

func (r *TierRing) insert(e *TierEntry) {
	// 相位先对最长档取模（与 ssr 的 seed 相同），再对本档格数取模：各档按同一相位对齐。
	e.cell = int32(e.hash % r.phaseM % uint64(r.frames[e.tier]))
	cell := &r.cells[e.tier][e.cell]
	e.idx = int32(len(*cell))
	*cell = append(*cell, e)
}

func (r *TierRing) remove(e *TierEntry) {
	cell := &r.cells[e.tier][e.cell]
	last := len(*cell) - 1
	moved := (*cell)[last]
	(*cell)[e.idx] = moved
	moved.idx = e.idx
	(*cell)[last] = nil
	*cell = (*cell)[:last]
}

// Add 见 Scheduler。
func (r *TierRing) Add(id int64, hash uint64, tier Tier) *TierEntry {
	e := &TierEntry{id: id, hash: hash, tier: tier}
	r.insert(e)
	r.count++
	return e
}

// Remove 见 Scheduler。
func (r *TierRing) Remove(e *TierEntry) {
	r.remove(e)
	r.count--
}

// SetTier 见 Scheduler。
func (r *TierRing) SetTier(e *TierEntry, tier Tier) *TierEntry {
	r.remove(e)
	e.tier = tier
	r.insert(e)
	return e
}

// Step 见 Scheduler。
func (r *TierRing) Step(out []int64) []int64 {
	r.frame++
	for t := range NumTiers {
		for _, e := range r.cells[t][r.frame%int64(r.frames[t])] {
			out = append(out, e.id)
		}
	}
	return out
}

// Resolution 见 Scheduler。
func (r *TierRing) Resolution() time.Duration { return r.res }

// Len 见 Scheduler。
func (r *TierRing) Len() int { return r.count }
