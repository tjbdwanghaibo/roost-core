package bench

// Mix 是登记的档位分布（按登记计，不合并）。
type Mix struct {
	Name    string
	Weights [NumTiers]int // 各档登记数的相对权重
}

// MixD20 取自 DESIGN-TODO D20 §7 的示例分布（5 万实体中 32% 挂心跳，不合并时共 3.4 万个登记）：
// 移动 100ms 8000 个（玩家 2000 + 怪物 6000）、AI 200ms 1 万个（怪物 6000 + 4000）、
// Buff / 回复 1s 1.6 万个（玩家 2000 + 4000、怪物 6000 + 4000）。按这个比例放大到 N 个登记。
var MixD20 = Mix{Name: "d20mix", Weights: [NumTiers]int{Tier100ms: 8000, Tier200ms: 10000, Tier1s: 16000}}

// MixAll100 是全部登记都是 100ms（例如所有实体都在移动）。
var MixAll100 = Mix{Name: "all100", Weights: [NumTiers]int{Tier100ms: 1}}

// MixAllTiers 是十档均分，只用于覆盖每一档的正确性测试与分布检查。
var MixAllTiers = Mix{Name: "alltiers", Weights: [NumTiers]int{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}}

// TierAt 按权重把第 i 个登记（0..n-1）确定性地分到一档：前缀和区间，相邻登记交错，不按档聚集 ID。
func (m Mix) TierAt(i, n int) Tier {
	total := 0
	for _, w := range m.Weights {
		total += w
	}
	// 用乘法散列把 i 打乱后再落到权重区间，避免“ID 小的全在 100ms 档”。
	x := int((uint64(i)*0x9E3779B97F4A7C15)>>11) % total
	for t, w := range m.Weights {
		if x < w {
			return Tier(t)
		}
		x -= w
	}
	return Tier(NumTiers - 1)
}

// FiresPerSecond 是 n 个登记按该分布每秒触发的总次数。
func (m Mix) FiresPerSecond(n int) float64 {
	total := 0
	for _, w := range m.Weights {
		total += w
	}
	sum := 0.0
	for t, w := range m.Weights {
		sum += float64(w) / float64(total) * float64(n) / Periods[t].Seconds()
	}
	return sum
}

// EntityID 构造一个形状与正式 EntityID 相同的 ID（[unique][kind 8 位][category 2 位]），只用于散列与键。
func EntityID(unique int64) int64 { return unique<<10 | 201<<2 | 1 }
