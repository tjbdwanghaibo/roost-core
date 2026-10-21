package bench

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"
)

// 对比一：调度结构微基准（不经 Nest）。命令见 docs/performance/HB-STRUCTURES-20261021.md。
//
//	op=add               在已有 n 个登记的结构上登记一个新登记（每 1024 个做一次不计时的撤销）
//	op=remove            撤销一个随机挑出的已有登记（每 1024 个做一次不计时的重新登记）
//	op=settier           在 100ms 与 1s 之间来回改档（开始 / 停止移动）
//	op=step              推进一个“模拟秒”（1s / 分辨率 格），ns/op = 每秒收集到期项的驱动耗时；
//	                     另报每格耗时 p50 / p99 / max 与每秒触发数
//	op=build             从空建到 n 个登记；另报建好并预热（见 warmup）后的常驻堆 liveB/reg

var benchSizes = []int{50_000, 500_000}

var benchMixes = []Mix{MixD20, MixAll100}

type benchImpl struct {
	name string
	run  func(b *testing.B, op string, n int, mix Mix)
}

func benchImpls() []benchImpl {
	return []benchImpl{
		{"ssr50", func(b *testing.B, op string, n int, mix Mix) {
			runSchedBench[SSRHandle](b, func() Scheduler[SSRHandle] { return NewSSRRing(0) }, op, n, mix)
		}},
		{"tier50", func(b *testing.B, op string, n int, mix Mix) {
			runSchedBench[*TierEntry](b, func() Scheduler[*TierEntry] { return NewTierRing(50 * time.Millisecond) }, op, n, mix)
		}},
		{"tier10", func(b *testing.B, op string, n int, mix Mix) {
			runSchedBench[*TierEntry](b, func() Scheduler[*TierEntry] { return NewTierRing(10 * time.Millisecond) }, op, n, mix)
		}},
		{"wheel10", func(b *testing.B, op string, n int, mix Mix) {
			runSchedBench[*WheelEntry](b, func() Scheduler[*WheelEntry] { return NewWheel(10 * time.Millisecond) }, op, n, mix)
		}},
		{"heap10", func(b *testing.B, op string, n int, mix Mix) {
			runSchedBench[*HeapEntry](b, func() Scheduler[*HeapEntry] { return NewMinHeap(10 * time.Millisecond) }, op, n, mix)
		}},
	}
}

func BenchmarkSched(b *testing.B) {
	for _, op := range []string{"add", "remove", "settier", "step", "build"} {
		for _, impl := range benchImpls() {
			for _, n := range benchSizes {
				for _, mix := range benchMixes {
					b.Run(fmt.Sprintf("op=%s/impl=%s/n=%d/mix=%s", op, impl.name, n, mix.Name), func(b *testing.B) {
						impl.run(b, op, n, mix)
					})
				}
			}
		}
	}
}

// BenchmarkSSRMapHint 报告 ssr 生产配置（每格 map 预分配 10000）下 1422 格的固定内存，与登记数无关。
func BenchmarkSSRMapHint(b *testing.B) {
	for _, hint := range []int{0, 10000} {
		b.Run(fmt.Sprintf("hint=%d", hint), func(b *testing.B) {
			var keep *SSRRing
			for b.Loop() {
				keep = NewSSRRing(hint)
			}
			b.StopTimer()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			keep = NewSSRRing(hint)
			runtime.GC()
			runtime.ReadMemStats(&after)
			b.ReportMetric(float64(after.HeapAlloc)-float64(before.HeapAlloc), "liveB")
			runtime.KeepAlive(keep)
		})
	}
}

func populate[H any](s Scheduler[H], n int, mix Mix) []H {
	hs := make([]H, n)
	for i := range n {
		hs[i] = s.Add(int64(i), PhaseHash(EntityID(int64(i+1))), mix.TierAt(i, n))
	}
	return hs
}

// advance 推进 d 的模拟时间，丢弃结果；用于建好后的预热（让切片容量与堆布局进入稳态）。
func advance[H any](s Scheduler[H], d time.Duration) {
	buf := make([]int64, 0, 1024)
	for range int(d / s.Resolution()) {
		buf = s.Step(buf[:0])
	}
}

// warmup 是计时前推进的模拟时间：时间轮要转满一圈（6000 格各自的切片容量才进入稳态，否则第一圈的
// 每格扩容会算进 step），其余结构 2 个模拟秒即可（堆转满一圈在 50 万登记下太慢，且堆的容量不随时间变化）。
func warmup[H any](s Scheduler[H]) time.Duration {
	if _, ok := any(s).(*Wheel); ok {
		return MaxPeriod + time.Second
	}
	return 2 * time.Second
}

const benchBatch = 1024

// pseudoRandomOrder 是 0..n-1 的一个固定排列的前 min(n, 65536) 项（无重复；按下标取模循环使用，
// 连续 1024 次不会取到同一个登记）。
func pseudoRandomOrder(n int) []int32 {
	perm := make([]int32, n)
	for i := range perm {
		perm[i] = int32(i)
	}
	x := uint64(88172645463325252)
	for i := n - 1; i > 0; i-- {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		j := int(x % uint64(i+1))
		perm[i], perm[j] = perm[j], perm[i]
	}
	return perm[:min(n, 1<<16)]
}

func runSchedBench[H any](b *testing.B, newS func() Scheduler[H], op string, n int, mix Mix) {
	switch op {
	case "build":
		b.ReportAllocs()
		var keep Scheduler[H]
		for b.Loop() {
			s := newS()
			populate(s, n, mix)
			keep = s
		}
		b.StopTimer()
		keep = nil
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		s := newS()
		hs := populate(s, n, mix)
		advance(s, warmup(s))
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric((float64(after.HeapAlloc)-float64(before.HeapAlloc))/float64(n), "liveB/reg")
		runtime.KeepAlive(s)
		runtime.KeepAlive(hs)
		runtime.KeepAlive(keep)
		return
	}
	s := newS()
	hs := populate(s, n, mix)
	advance(s, warmup(s))
	switch op {
	case "add":
		// b.Loop：本函数只调用一次，n 个登记的建表与预热只做一次；每 1024 次登记后不计时地撤销。
		extra := make([]H, benchBatch)
		k := 0
		b.ReportAllocs()
		for b.Loop() {
			id := n + k
			extra[k] = s.Add(int64(id), PhaseHash(EntityID(int64(id+1))), mix.TierAt(id, n))
			if k++; k == benchBatch {
				b.StopTimer()
				for j := range k {
					s.Remove(extra[j])
				}
				k = 0
				b.StartTimer()
			}
		}
	case "remove":
		// 撤销随机挑出的已有登记（冷数据，不是刚登记的那批）；每 1024 次后不计时地按原参数登记回去。
		order := pseudoRandomOrder(n)
		k, i := 0, 0
		picked := make([]int32, benchBatch)
		b.ReportAllocs()
		for b.Loop() {
			idx := order[i%len(order)]
			i++
			s.Remove(hs[idx])
			picked[k] = idx
			if k++; k == benchBatch {
				b.StopTimer()
				for _, idx := range picked[:k] {
					hs[idx] = s.Add(int64(idx), PhaseHash(EntityID(int64(idx+1))), mix.TierAt(int(idx), n))
				}
				k = 0
				b.StartTimer()
			}
		}
		for _, idx := range picked[:k] {
			hs[idx] = s.Add(int64(idx), PhaseHash(EntityID(int64(idx+1))), mix.TierAt(int(idx), n))
		}
	case "settier":
		// 固定伪随机顺序挑登记，在 100ms 与 1s 之间来回改。
		order := pseudoRandomOrder(n)
		tiers := make([]Tier, n)
		for i := range tiers {
			tiers[i] = mix.TierAt(i, n)
		}
		i := 0
		b.ReportAllocs()
		for b.Loop() {
			k := order[i%len(order)]
			i++
			next := Tier100ms
			if tiers[k] == Tier100ms {
				next = Tier1s
			}
			hs[k] = s.SetTier(hs[k], next)
			tiers[k] = next
		}
	case "step":
		steps := int(time.Second / s.Resolution())
		buf := make([]int64, 0, n)
		durations := make([]time.Duration, 0, steps*64) // 最多保留前 64 个模拟秒的每格耗时
		fires, ops := 0, 0
		b.ReportAllocs()
		for b.Loop() {
			for range steps {
				start := time.Now()
				buf = s.Step(buf[:0])
				d := time.Since(start)
				if len(durations) < cap(durations) {
					durations = append(durations, d)
				}
				fires += len(buf)
			}
			ops++
		}
		slices.Sort(durations)
		q := func(p float64) float64 {
			return float64(durations[min(len(durations)-1, int(p*float64(len(durations))))])
		}
		b.ReportMetric(q(0.50), "p50-ns/step")
		b.ReportMetric(q(0.99), "p99-ns/step")
		b.ReportMetric(float64(durations[len(durations)-1]), "max-ns/step")
		b.ReportMetric(float64(fires)/float64(ops), "fires/op")
	default:
		b.Fatalf("unknown op %q", op)
	}
	runtime.KeepAlive(hs)
}
