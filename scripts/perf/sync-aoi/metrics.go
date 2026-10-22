package main

import (
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// interestMetrics 把进程内全局指标表里 Interest 的指标（entitysync.interest.* 与 entitysync_interest_*）导出到
// server.json，供 ApplySync 预算对照读 log_backlog（运行末的值）、budget_stop（累计次数）与政策阶段 / 结算耗时
// 直方图。累计口径从进程启动算起（含预热），直方图分位来自固定指数桶（1ms 起翻倍），只适合看量级。
func interestMetrics() map[string]any {
	out := map[string]any{}
	for _, m := range metrics.Snapshot() {
		if !strings.HasPrefix(m.Name, "entitysync.interest.") && !strings.HasPrefix(m.Name, "entitysync_interest_") {
			continue
		}
		row := map[string]any{"kind": string(m.Kind), "value": m.Value, "count": m.Count}
		if m.Count > 0 {
			row["mean_ms"] = milliseconds(time.Duration(m.TotalNanos / m.Count))
			row["max_ms"] = milliseconds(time.Duration(m.MaxNanos))
		}
		if m.Kind == metrics.KindHistogram {
			row["p50_ms"] = milliseconds(metrics.HistogramQuantile(m.Name, m.Labels, 0.50))
			row["p95_ms"] = milliseconds(metrics.HistogramQuantile(m.Name, m.Labels, 0.95))
			row["p99_ms"] = milliseconds(metrics.HistogramQuantile(m.Name, m.Labels, 0.99))
		}
		out[m.Name] = row
	}
	return out
}
