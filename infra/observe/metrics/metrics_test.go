// 指标注册表：快照、Prometheus 文本与转义、序列基数上限与丢弃可见、直方图分位数界、按标签删除序列。

package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRegistrySnapshot(t *testing.T) {
	reg := NewRegistry()
	reg.IncCounter("requests", Labels{"handler": "login"}, 2)
	reg.SetGauge("online", nil, 10)
	reg.ObserveDuration("cost", Labels{"handler": "login"}, 3*time.Millisecond)
	snap := reg.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	foundTimer := false
	for _, metric := range snap {
		if metric.Name == "cost" {
			foundTimer = metric.Count == 1 && metric.TotalNanos == int64(3*time.Millisecond)
		}
	}
	if !foundTimer {
		t.Fatalf("timer not found in %+v", snap)
	}
}

func TestPrometheusTextDoesNotDoubleTotalSuffix(t *testing.T) {
	reg := NewRegistry()
	reg.IncCounter("bus.dispatch.total", nil, 1)
	reg.IncCounter("bus_dead_letter_total", nil, 1)

	text := string(PrometheusText(reg.Snapshot()))

	if strings.Contains(text, "total_total") {
		t.Fatalf("prometheus text should not double total suffix:\n%s", text)
	}
	if !strings.Contains(text, "bus_dispatch_total 1") || !strings.Contains(text, "bus_dead_letter_total 1") {
		t.Fatalf("prometheus text missing normalized counters:\n%s", text)
	}
}

func TestRegistryLimitsMetricSeriesCardinality(t *testing.T) {
	reg := NewRegistry(WithMaxSeriesPerMetric(2))

	reg.IncCounter("player.event.total", Labels{"player_id": "1"}, 1)
	reg.IncCounter("player.event.total", Labels{"player_id": "2"}, 1)
	reg.IncCounter("player.event.total", Labels{"player_id": "3"}, 1)

	snap := reg.Snapshot()
	series := 0
	for _, metric := range snap {
		if metric.Name == "player.event.total" {
			series++
		}
	}
	if series != 2 {
		t.Fatalf("series count = %d, want 2 snapshot=%+v", series, snap)
	}
	if dropped := reg.DroppedSeries(); dropped != 1 {
		t.Fatalf("dropped series = %d, want 1", dropped)
	}
}

func TestSeriesLimitDropIsVisible(t *testing.T) {
	reg := NewRegistry(WithMaxSeriesPerMetric(1))
	reg.IncCounter("demo.metric", Labels{"k": "a"}, 1)
	reg.IncCounter("demo.metric", Labels{"k": "b"}, 1) // over budget: dropped
	reg.IncCounter("demo.metric", Labels{"k": "c"}, 1) // dropped again
	if reg.DroppedSeries() != 2 {
		t.Fatalf("dropped = %d, want 2", reg.DroppedSeries())
	}
	for _, metric := range reg.Snapshot() {
		if metric.Name == "obs.series.dropped" &&
			metric.Kind == KindCounter &&
			metric.Labels["metric"] == "demo.metric" &&
			metric.Value == 2 {
			return
		}
	}
	t.Fatalf("obs.series.dropped series missing: %+v", reg.Snapshot())
}

func TestHistogramObserveQuantileAndExport(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 90; i++ {
		reg.ObserveHistogram("robot.call", Labels{"msg": "ping"}, 3*time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		reg.ObserveHistogram("robot.call", Labels{"msg": "ping"}, 300*time.Millisecond)
	}
	p50 := reg.HistogramQuantile("robot.call", Labels{"msg": "ping"}, 0.50)
	p99 := reg.HistogramQuantile("robot.call", Labels{"msg": "ping"}, 0.99)
	if p50 <= 0 || p50 > 8*time.Millisecond {
		t.Fatalf("p50 = %v, want within the 2-4ms bucket range", p50)
	}
	if p99 < 100*time.Millisecond || p99 > 600*time.Millisecond {
		t.Fatalf("p99 = %v, want within the ~256-512ms bucket range", p99)
	}
	// Unknown series is zero, not a panic.
	if q := reg.HistogramQuantile("missing", nil, 0.5); q != 0 {
		t.Fatalf("missing series quantile = %v", q)
	}
	var found *Metric
	for _, m := range reg.Snapshot() {
		if m.Kind == KindHistogram && m.Name == "robot.call" {
			found = &m
			break
		}
	}
	if found == nil || found.Count != 100 {
		t.Fatalf("histogram snapshot missing: %+v", found)
	}
	text := string(PrometheusText([]Metric{*found}))
	for _, want := range []string{`robot_call_bucket{msg="ping",le="+Inf"} 100`, "robot_call_count", "robot_call_sum_nanos"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prometheus export missing %q:\n%s", want, text)
		}
	}
	// Cumulative buckets must be monotonically non-decreasing.
	last := int64(-1)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "robot_call_bucket") {
			var v int64
			if _, err := fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &v); err != nil {
				t.Fatal(err)
			}
			if v < last {
				t.Fatalf("buckets not cumulative:\n%s", text)
			}
			last = v
		}
	}
}

func TestMetricKeyRetainsCanonicalEscapingAndOrder(t *testing.T) {
	cases := []struct {
		labels Labels
		want   string
	}{
		{nil, "sample"},
		{Labels{"handler": "login"}, `sample{handler="login"}`},
		{Labels{"z": "line\nquote\"", "a": "x\\y"}, `sample{a="x\\y",z="line\nquote\""}`},
	}
	for _, tc := range cases {
		if got := metricKey("sample", tc.labels); got != tc.want {
			t.Fatalf("key=%q want=%q", got, tc.want)
		}
	}
}

// 维护者第十二轮决定（metrics 按标签删除，N12 方向判断“gauge 生命周期”）：带动态标签的序列由对象的
// 拥有者在销毁时删除。删除后序列不再导出，名额还给该指标；空 match 不删任何东西。
func TestDeleteSeriesRemovesEveryKindByLabelAndReturnsTheQuota(t *testing.T) {
	reg := NewRegistry(WithMaxSeriesPerMetric(2))
	for _, run := range []string{"a", "b"} {
		labels := Labels{"run": run, "scenario": "x"}
		reg.IncCounter("robot.runner.scenario.total", labels, 1)
		reg.SetGauge("robot.runner.target", labels, 3)
		reg.ObserveHistogram("robot.runner.scenario.cost", labels, time.Millisecond)
		reg.ObserveDuration("robot.loadtest.run.duration", labels, time.Second)
	}
	reg.IncCounter("unlabeled.total", nil, 1)

	if got := reg.DeleteSeries("", nil); got != 0 {
		t.Fatalf("an empty match deleted %d series; only Reset may clear everything", got)
	}
	if got := reg.DeleteSeries("robot.runner.target", Labels{"run": "a"}); got != 1 {
		t.Fatalf("deleting one name for run a removed %d series, want 1", got)
	}
	if got := reg.DeleteSeries("", Labels{"run": "a"}); got != 3 {
		t.Fatalf("deleting run a across names removed %d series, want the remaining 3", got)
	}
	text := string(PrometheusText(reg.Snapshot()))
	if strings.Contains(text, `run="a"`) {
		t.Fatalf("run a is still exported after DeleteSeries:\n%s", text)
	}
	for _, name := range []string{"robot_runner_scenario_total", "robot_runner_target", "robot_runner_scenario_cost_count", "robot_loadtest_run_duration_count", "unlabeled_total"} {
		if !strings.Contains(text, name) {
			t.Fatalf("%s disappeared together with run a:\n%s", name, text)
		}
	}
	if got := reg.SeriesCount(); got != 5 {
		t.Fatalf("SeriesCount = %d, want 5 (four for run b, one unlabeled)", got)
	}

	// 名额归还：每指标上限 2，run a 删掉之后第三个 run 不会被丢弃。
	reg.IncCounter("robot.runner.scenario.total", Labels{"run": "c", "scenario": "x"}, 1)
	if dropped := reg.DroppedSeries(); dropped != 0 {
		t.Fatalf("a new run was dropped (%d) although run a gave its series back", dropped)
	}
	if !strings.Contains(string(PrometheusText(reg.Snapshot())), `run="c"`) {
		t.Fatal("run c was not recorded")
	}
	// 删掉后再写，是从零开始的新序列。
	reg.IncCounter("robot.runner.scenario.total", Labels{"run": "c", "scenario": "x"}, 1)
	reg.DeleteSeries("", Labels{"run": "c"})
	reg.IncCounter("robot.runner.scenario.total", Labels{"run": "c", "scenario": "x"}, 1)
	for _, metric := range reg.Snapshot() {
		if metric.Labels["run"] == "c" && metric.Value != 1 {
			t.Fatalf("a re-created series continued from %d instead of starting over", metric.Value)
		}
	}
}

// 直方图分位数的估计不落在观测到的最小 / 最大值之外：桶是 1ms 起翻倍的 17 个，估计在命中的桶里线性插值，插值区间夹到实际样本范围；
// 排名落在溢出里的分位数不被低估成最大桶上界。任何分位数的真值都在 [最小样本, 最大样本] 里。
// 历史：RR-20261006-27。

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

// Prometheus 文本格式的标签值只有三种转义：`\\`、`\"`、`\n`，其余字符（含制表符与非 ASCII）原样写出，内容须是合法 UTF-8；不能用
// strconv.Quote（`\t`、`\u....`、`\x..` 都不在 exposition 格式的转义集合里，抓取器会解析失败或读成另一个字符串）。
// 历史：RR-20261005-NC-264。
func TestPrometheusLabelValuesUseTheExpositionEscapes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "game-1", `v="game-1"`},
		{"quote backslash newline", "a\"b\\c\nd", `v="a\"b\\c\nd"`},
		{"tab stays raw", "a\tb", "v=\"a\tb\""},
		{"non-ascii stays raw", "区服​1", "v=\"区服​1\""},
		{"invalid utf-8 is replaced", "a\xffb", "v=\"a�b\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(PrometheusText([]Metric{{Name: "m", Kind: KindGauge, Labels: Labels{"v": tc.value}, Value: 1}}))
			want := "m{" + tc.want + "} 1\n"
			if out != want {
				t.Errorf("exposition = %q, want %q", out, want)
			}
			if strings.Contains(out, `\t`) || strings.Contains(out, `\x`) || strings.Contains(out, `\u`) {
				t.Errorf("exposition %q uses an escape the text format does not define", out)
			}
		})
	}
}
