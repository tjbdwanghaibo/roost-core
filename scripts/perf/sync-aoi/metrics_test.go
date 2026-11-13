package main

import (
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 运行中的积压采样读的是 Interest 设置的那个 Gauge 的此刻值（不是累计、不是别的序列），逐 tick 的样本汇成
// 分位与非零 tick 数，汇总不改动样本的顺序。
func TestLogBacklogIsSampledFromTheInterestGauge(t *testing.T) {
	metrics.SetGauge(logBacklogMetric, nil, 7)
	if got, found := logBacklogGauge(); !found || got != 7 {
		t.Fatalf("logBacklogGauge after setting 7 = (%d, %v)", got, found)
	}
	metrics.SetGauge(logBacklogMetric, nil, 0)
	if got, found := logBacklogGauge(); !found || got != 0 {
		t.Fatalf("logBacklogGauge after setting 0 = (%d, %v), want (0, true): a drained log is a sample too", got, found)
	}

	samples := []float64{0, 3, 0, 5}
	report := backlogReport(samples)
	if report["samples"] != 4 || report["nonzero_ticks"] != 2 {
		t.Fatalf("backlogReport counts = %v", report)
	}
	if entries := report["entries"].(map[string]float64); entries["max"] != 5 || entries["min"] != 0 {
		t.Fatalf("backlogReport entries = %v", entries)
	}
	if !slices.Equal(samples, []float64{0, 3, 0, 5}) {
		t.Fatalf("backlogReport reordered the samples: %v", samples)
	}
	if backlogReport(nil) != nil {
		t.Fatal("no samples should leave log_backlog_per_tick out of server.json")
	}
}
