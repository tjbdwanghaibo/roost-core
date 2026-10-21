package bench

import (
	"fmt"
	"testing"
	"time"
)

// 四种结构的触发时刻必须与参考规则逐格相同：首拍是 cur 之后第一个 f ≡ 相位 (mod 周期格数)，之后每周期一次；
// 撤销后不再触发，改档后从改档那一格起按新档的相位规则触发。先保证原型对，再比快慢。

type schedCase struct {
	name string
	run  func(t *testing.T)
}

func schedCases() []schedCase {
	return []schedCase{
		{"ssr50", func(t *testing.T) { checkScheduler[SSRHandle](t, NewSSRRing(0)) }},
		{"tier50", func(t *testing.T) { checkScheduler[*TierEntry](t, NewTierRing(50*time.Millisecond)) }},
		{"tier10", func(t *testing.T) { checkScheduler[*TierEntry](t, NewTierRing(10*time.Millisecond)) }},
		{"wheel10", func(t *testing.T) { checkScheduler[*WheelEntry](t, NewWheel(10*time.Millisecond)) }},
		{"heap10", func(t *testing.T) { checkScheduler[*HeapEntry](t, NewMinHeap(10*time.Millisecond)) }},
	}
}

func TestSchedulersMatchReferenceRule(t *testing.T) {
	for _, c := range schedCases() {
		t.Run(c.name, c.run)
	}
}

type refReg struct {
	hash        uint64
	tier        Tier
	from        int64 // 从这一格之后（不含）按 tier 触发
	removedAt   int64 // >0 时：这一格之后不再触发
	changedAt   int64 // >0 时：这一格之后改为 newTier
	newTier     Tier
	fires       []int64
	expectFires []int64
}

func checkScheduler[H any](t *testing.T, s Scheduler[H]) {
	t.Helper()
	res := s.Resolution()
	phaseM := uint64(framesOf(MaxPeriod, res))
	const n = 3000
	horizon := int64(framesOf(2*MaxPeriod, res)) + 7
	regs := make([]*refReg, n)
	handles := make([]H, n)
	for i := range regs {
		tier := MixAllTiers.TierAt(i, n)
		r := &refReg{hash: PhaseHash(EntityID(int64(i + 1))), tier: tier}
		regs[i] = r
		switch i % 5 {
		case 1:
			r.removedAt = int64(i%int(horizon/2)) + 1
		case 2:
			r.changedAt = int64(i%int(horizon/2)) + 1
			r.newTier = Tier((int(tier) + 3) % NumTiers)
		}
	}
	// 一半在第 0 格登记，另一半在第 17 格登记，覆盖“首拍从非零格起算”。
	for cur := int64(0); cur <= horizon; cur++ {
		for i, r := range regs {
			start := int64(0)
			if i%2 == 1 {
				start = 17
			}
			if cur == start {
				r.from = cur
				handles[i] = s.Add(int64(i), r.hash, r.tier)
			}
			if r.removedAt > 0 && cur == max(r.removedAt, start) && r.removedAt >= start {
				s.Remove(handles[i])
			}
			if r.changedAt > 0 && cur == max(r.changedAt, start) && r.changedAt >= start {
				handles[i] = s.SetTier(handles[i], r.newTier)
			}
		}
		if cur == horizon {
			break
		}
		for _, id := range s.Step(nil) {
			regs[id].fires = append(regs[id].fires, cur+1)
		}
	}
	for i, r := range regs {
		start := int64(0)
		if i%2 == 1 {
			start = 17
		}
		tier := r.tier
		end := horizon
		if r.removedAt >= start && r.removedAt > 0 {
			end = max(r.removedAt, start)
		}
		segEnd := end
		if r.changedAt >= start && r.changedAt > 0 {
			segEnd = max(r.changedAt, start)
		}
		appendExpected := func(from, to int64, tier Tier) {
			p := framesOf(tier.Period(), res)
			phase := int(r.hash % phaseM % uint64(p))
			for f := firstFire(from, p, phase); f <= to; f += int64(p) {
				r.expectFires = append(r.expectFires, f)
			}
		}
		appendExpected(start, segEnd, tier)
		if segEnd < end {
			appendExpected(segEnd, end, r.newTier)
		}
		if fmt.Sprint(r.fires) != fmt.Sprint(r.expectFires) {
			t.Fatalf("reg %d tier %v: fires %v, want %v", i, tier.Period(), head(r.fires), head(r.expectFires))
		}
	}
	if s.Len() == 0 {
		t.Fatal("expected live registrations at the end")
	}
}

func head(v []int64) []int64 {
	if len(v) > 12 {
		return v[:12]
	}
	return v
}

// SSRRing 与 50ms TierRing 的触发时刻逐格相同（同一相位规则，只是存储不同）。
func TestSSRRingAndTierRingFireIdentically(t *testing.T) {
	a, b := NewSSRRing(0), NewTierRing(50*time.Millisecond)
	const n = 20000
	for i := range n {
		h := PhaseHash(EntityID(int64(i + 1)))
		tier := MixAllTiers.TierAt(i, n)
		a.Add(int64(i), h, tier)
		b.Add(int64(i), h, tier)
	}
	for step := range 2400 {
		x, y := countSet(a.Step(nil)), countSet(b.Step(nil))
		if len(x) != len(y) {
			t.Fatalf("step %d: ssr %d fires, tier ring %d", step, len(x), len(y))
		}
		for id := range x {
			if !y[id] {
				t.Fatalf("step %d: id %d fired only in ssr", step, id)
			}
		}
	}
}

func countSet(ids []int64) map[int64]bool {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}
