package loadtest_test

// RR-20261006-27：阈值判定用的分位数不能超过真实样本；判失败时要说清哪条阈值、实际值多少。
//
// 真实进程演练里两个 sid 同时跑，10 个机器人全部成功、整次运行 9.636s 结束，默认分位数（场景耗时
// 直方图）却给出 p95=16.384s——最慢样本所在桶的上界——超过 -max-p95 16 判 failed/threshold，
// 退出码 1。生成的 loadtest 最后只打 “run … ended failed (threshold)”，不说是哪条阈值、实际值多少，
// 要翻上面的 JSON 才知道，JSON 里的 16.384 也看不出只是桶上界。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
)

// costRunner 在“运行”结束前把给定的场景耗时记进默认直方图，标签与 runner 写入的一致。
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
