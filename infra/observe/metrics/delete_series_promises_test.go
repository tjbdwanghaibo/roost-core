package metrics

import (
	"strings"
	"testing"
	"time"
)

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
