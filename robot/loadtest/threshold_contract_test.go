package loadtest_test

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
	"strings"
	"testing"
	"time"
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

type costRunner struct {
	labels metrics.Labels
	costs  []time.Duration
}

func (r *costRunner) Run(context.Context) error {
	for _, d := range r.costs {
		metrics.ObserveHistogram("robot.runner.scenario.cost", r.labels, d)
	}
	return nil
}
func (r *costRunner) Stop(context.Context) error { return nil }
func (r *costRunner) Stats() runner.Stats {
	n := int64(len(r.costs))
	return runner.Stats{Started: n, Success: n}
}

func runWithCosts(t *testing.T, thresholds []loadtest.Threshold, costs []time.Duration) loadtest.RunSnapshot {
	t.Helper()
	old := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(metrics.NewRegistry())
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	m := loadtest.New(loadtest.Config{
		Profiles: map[string]loadtest.Profile{
			"demo": {Run: runner.Config{Count: len(costs), Scenario: "demo"}, Thresholds: thresholds},
		},
		RunnerFactory: func(runID string, profile string, _ loadtest.Profile) loadtest.Runner {
			return &costRunner{
				labels: metrics.Labels{"profile": profile, "run": runID, "scenario": "demo", "result": "ok"},
				costs:  costs,
			}
		},
	})
	if _, err := m.Start(context.Background(), loadtest.StartRequest{Profile: "demo"}); err != nil {
		t.Fatal(err)
	}
	return waitDone(t, m)
}

// 演练的形状：10 个机器人，耗时 8.3～9.6s，阈值 p95 ≤ 10s。真实 p95（最慢的那个）是 9.6s。
var twoSidCosts = []time.Duration{
	8300 * time.Millisecond, 8400 * time.Millisecond, 8600 * time.Millisecond, 8700 * time.Millisecond,
	8900 * time.Millisecond, 9000 * time.Millisecond, 9100 * time.Millisecond, 9300 * time.Millisecond,
	9500 * time.Millisecond, 9600 * time.Millisecond,
}

func TestQuantileThresholdJudgesTheObservedCostsNotABucketBound(t *testing.T) {
	run := runWithCosts(t, []loadtest.Threshold{{Metric: "p95", Max: 10}}, twoSidCosts)
	if run.State != loadtest.StateFinished {
		t.Fatalf("every cost ≤ 9.6s but p95 ≤ 10s judged %s/%s thresholds=%+v quantiles_ms=%v",
			run.State, run.StopReason, run.Thresholds, run.QuantilesMS)
	}
	if got := run.QuantilesMS["p95"]; got != 9600 {
		t.Fatalf("p95 of 10 robots = %dms, want the slowest robot 9600ms", got)
	}
}

func TestThresholdFailureNamesTheThresholdAndTheActualValue(t *testing.T) {
	run := runWithCosts(t, []loadtest.Threshold{{Metric: "error_rate", Max: 0}, {Metric: "p95", Max: 9}}, twoSidCosts)
	if run.State != loadtest.StateFailed || run.StopReason != loadtest.StopReasonThreshold {
		t.Fatalf("p95 9.6s > 9s ended %s/%s", run.State, run.StopReason)
	}
	// 失败说明要点名违反的那条（p95）、实际值、上限与样本数，不提没违反的 error_rate。
	for _, want := range []string{"p95", "9.6", "max 9", "10 samples"} {
		if !strings.Contains(run.Error, want) {
			t.Errorf("failure explanation %q does not mention %q", run.Error, want)
		}
	}
	if strings.Contains(run.Error, "error_rate") {
		t.Errorf("failure explanation %q names a threshold that passed", run.Error)
	}
	var p95 *loadtest.ThresholdResult
	for i := range run.Thresholds {
		if run.Thresholds[i].Metric == "p95" {
			p95 = &run.Thresholds[i]
		}
	}
	if p95 == nil || p95.Samples != 10 {
		t.Fatalf("p95 verdict %+v; want samples=10", p95)
	}
}
