package loadtest_test

// RR-20261005-NC-161：没有样本的阈值不能判“通过”。
//
// 阈值是 CI 读取的门禁（Threshold 注释：any violation marks the run failed）。旧 evaluate 把
// “没有数据”当成 0：一个场景都没跑完时 error_rate 的分母为 0、实际值记 0；成功场景的耗时直方图
// 不存在时 HistogramQuantile 返回 0——两者都 ≤ Max，于是一次什么都没测到的运行判为 finished。
// 两条真实触发路径：-duration 短于场景耗时（所有机器人被取消，Success=Failure=0）；单进程跑过足够
// 多次后，带 run 标签的直方图序列触到 metrics 每指标序列上限被丢弃（成功场景有，直方图没有）。

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

func TestThresholdsWithoutSamplesFailTheRun(t *testing.T) {
	// 5 个机器人全部在场景中途被取消：没有一个完成的场景。
	fake := &fakeRunner{delay: 10 * time.Millisecond, stats: runner.Stats{Started: 5, Canceled: 5}}
	m := newManager(t, fake, []loadtest.Threshold{
		{Metric: "error_rate", Max: 0},
		{Metric: "p95", Max: 1},
	}, 0)
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	run := waitDone(t, m)
	if run.State != loadtest.StateFailed || run.StopReason != loadtest.StopReasonThreshold {
		t.Fatalf("a run with no completed scenario ended %s/%s thresholds=%+v; want failed/threshold", run.State, run.StopReason, run.Thresholds)
	}
	for _, result := range run.Thresholds {
		if !result.Violated {
			t.Errorf("threshold %s passed without a single sample: %+v", result.Metric, result)
		}
	}
}

func TestQuantileThresholdFailsWhenTheHistogramSeriesWasDropped(t *testing.T) {
	// 每指标只允许 1 条序列，并且已被另一次运行占用：本次运行的耗时直方图会被丢弃。
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry(metrics.WithMaxSeriesPerMetric(1))
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	reg.ObserveHistogram("robot.runner.scenario.cost", metrics.Labels{"run": "earlier", "result": "ok"}, time.Second)

	scenarios := scenario.NewRegistry()
	scenarios.MustRegister(scenario.New("instant", scenario.NodeFunc(func(context.Context, *robot.Context) error {
		return nil
	})))
	m := loadtest.New(loadtest.Config{
		Profiles: map[string]loadtest.Profile{
			"smoke": {
				Run:        runner.Config{Count: 3, Scenario: "instant"},
				Thresholds: []loadtest.Threshold{{Metric: "p95", Max: 10}},
			},
		},
		RunnerOptions: []runner.Option{runner.WithScenarioRegistry(scenarios)},
	})
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "smoke"}); err != nil {
		t.Fatal(err)
	}
	run := waitDone(t, m)
	if run.Stats.Success != 3 {
		t.Fatalf("fixture: success=%d, want 3", run.Stats.Success)
	}
	if reg.DroppedSeries() == 0 {
		t.Fatal("fixture: the run's histogram series was not dropped")
	}
	if run.State != loadtest.StateFailed || len(run.Thresholds) != 1 || !run.Thresholds[0].Violated {
		t.Fatalf("p95 judged on a dropped histogram: state=%s thresholds=%+v; want violated", run.State, run.Thresholds)
	}
}
