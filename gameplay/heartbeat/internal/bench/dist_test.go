package bench

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// 每格到期数量的分布（检验相位 = hash(实体 ID) 的打散效果）。转满一圈（60s）统计每格到期数：
// 最大 / 平均、p99 / 平均、变异系数（标准差 / 平均）；“随机放置”的变异系数约为 1/√平均，作参照。
// 50ms 结构（ssr50，与 tier50 逐格相同）与 10ms 结构（wheel10，与 tier10 / heap10 逐格相同，见
// TestSchedulersMatchReferenceRule）各算一次；10ms 另按 50ms 窗口合并，与 50ms 帧同口径比较。
//
// HB_DIST_OUT=<文件> 时把表写到文件（Markdown），否则只做断言。

type distRow struct {
	impl, mix          string
	n                  int
	window             time.Duration
	mean, max, p99, cv float64
}

func TestPhaseDistribution(t *testing.T) {
	sizes := []int{50_000}
	if os.Getenv("HB_DIST_OUT") != "" {
		sizes = benchSizes
	}
	var rows []distRow
	for _, n := range sizes {
		for _, mix := range benchMixes {
			ssr := NewSSRRing(0)
			populate[SSRHandle](ssr, n, mix)
			rows = append(rows, distOf("ssr50", mix, n, collectCounts[SSRHandle](ssr), 1, ssrResolution))
			wheel := NewWheel(10 * time.Millisecond)
			populate[*WheelEntry](wheel, n, mix)
			counts := collectCounts[*WheelEntry](wheel)
			rows = append(rows, distOf("wheel10", mix, n, counts, 1, 10*time.Millisecond))
			rows = append(rows, distOf("wheel10", mix, n, counts, 5, 10*time.Millisecond))
		}
	}
	for _, r := range rows {
		// 随机放置下 6000 格的最大值约在平均值 + 4.5σ；超过平均 1.5 倍说明相位明显聚集。
		if r.max/r.mean > 1.5 {
			t.Errorf("%s %s n=%d window=%v: max/mean %.3f", r.impl, r.mix, r.n, r.window, r.max/r.mean)
		}
	}
	if path := os.Getenv("HB_DIST_OUT"); path != "" {
		var b strings.Builder
		b.WriteString("| 结构 | 分布 | 登记数 | 窗口 | 每窗平均到期 | 最大 / 平均 | p99 / 平均 | 变异系数 | 随机放置参照 1/√平均 |\n")
		b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "| %s | %s | %d | %v | %.0f | %.3f | %.3f | %.4f | %.4f |\n",
				r.impl, r.mix, r.n, r.window, r.mean, r.max/r.mean, r.p99/r.mean, r.cv, 1/math.Sqrt(r.mean))
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func collectCounts[H any](s Scheduler[H]) []int {
	steps := int(MaxPeriod / s.Resolution())
	counts := make([]int, steps)
	buf := make([]int64, 0, 1024)
	for i := range steps {
		buf = s.Step(buf[:0])
		counts[i] = len(buf)
	}
	return counts
}

func distOf(impl string, mix Mix, n int, counts []int, merge int, res time.Duration) distRow {
	var vals []float64
	for i := 0; i+merge <= len(counts); i += merge {
		sum := 0
		for _, c := range counts[i : i+merge] {
			sum += c
		}
		vals = append(vals, float64(sum))
	}
	mean := 0.0
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	variance := 0.0
	for _, v := range vals {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(vals))
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	return distRow{
		impl: impl, mix: mix.Name, n: n, window: res * time.Duration(merge),
		mean: mean, max: sorted[len(sorted)-1], p99: sorted[int(0.99*float64(len(sorted)-1))], cv: math.Sqrt(variance) / mean,
	}
}
