package metrics

// RR-20261006-27：直方图分位数的估计不能落在观测到的最小 / 最大值之外。
//
// 桶是 1ms 起翻倍的 17 个（上界最大 65.536s），估计在命中的桶里线性插值。旧实现插值区间就是桶的
// [下界, 上界]：排名落在某桶的最后一个样本时返回桶的上界，哪怕所有样本都远低于它——10 个样本时
// p95 的排名是 10，于是 p95 等于“最慢样本所在桶的上界”。真实进程演练里两个 sid 同时跑，整次运行
// 9.636s 结束，10 个机器人的场景耗时都不超过它，loadtest 却报 p90=15.018s、p95=p99=16.384s，
// 超过 -max-p95 16 判失败。另一头，超过最大桶上界的样本只记溢出计数，旧实现对排名落在溢出里的
// 分位数返回 65.536s——比每个样本都小，阈值大于 65.536s 时会把更慢的运行判通过。
// 任何分位数的真值都在 [最小样本, 最大样本] 里，估计不应越出这个区间。

import (
	"testing"
	"time"
)

func TestHistogramQuantileNeverExceedsTheSlowestObservation(t *testing.T) {
	reg := NewRegistry()
	labels := Labels{"run": "two-sids"}
	// 演练的形状：10 个样本都在 (8.192s, 16.384s] 这个桶里，最慢 9.6s。
	samples := []time.Duration{
		8300 * time.Millisecond, 8400 * time.Millisecond, 8600 * time.Millisecond, 8700 * time.Millisecond,
		8900 * time.Millisecond, 9000 * time.Millisecond, 9100 * time.Millisecond, 9300 * time.Millisecond,
		9500 * time.Millisecond, 9600 * time.Millisecond,
	}
	for _, d := range samples {
		reg.ObserveHistogram("robot.runner.scenario.cost", labels, d)
	}
	for _, q := range []float64{0.5, 0.9, 0.95, 0.99} {
		got := reg.HistogramQuantile("robot.runner.scenario.cost", labels, q)
		if got < samples[0] || got > samples[len(samples)-1] {
			t.Errorf("q%.2f = %v, outside the observed range [%v, %v]", q, got, samples[0], samples[len(samples)-1])
		}
	}
	// 10 个样本时 p95 的排名就是最后一个：估计应当正好是最慢的样本。
	if got := reg.HistogramQuantile("robot.runner.scenario.cost", labels, 0.95); got != 9600*time.Millisecond {
		t.Errorf("p95 of 10 samples = %v, want the slowest sample 9.6s", got)
	}
}

func TestHistogramQuantileInTheOverflowIsNotUnderstated(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 10; i++ {
		reg.ObserveHistogram("slow", nil, 100*time.Second)
	}
	if got := reg.HistogramQuantile("slow", nil, 0.95); got != 100*time.Second {
		t.Fatalf("p95 of ten 100s samples = %v, want 100s (the largest bucket bound is 65.536s)", got)
	}
}

func TestHistogramQuantileOfIdenticalSamplesIsThatSample(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 50; i++ {
		reg.ObserveHistogram("flat", nil, 3*time.Millisecond)
	}
	for _, q := range []float64{0.01, 0.5, 0.99} {
		if got := reg.HistogramQuantile("flat", nil, q); got != 3*time.Millisecond {
			t.Errorf("q%.2f of fifty 3ms samples = %v, want 3ms", q, got)
		}
	}
}
