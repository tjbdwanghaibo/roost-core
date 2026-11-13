package main

import (
	"slices"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// interestMetrics 把进程内全局指标表里 Interest 的指标（entitysync.interest.* 与 entitysync_interest_*）导出到
// server.json，供 ApplySync 预算对照读 log_backlog（运行末的值；运行中逐 tick 的值见 logBacklogGauge）、budget_stop（累计次数）与政策阶段 / 结算耗时
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

// logBacklogMetric 是 Interest 同步模式每次政策阶段结束时日志里剩下的条目数（事实 + 命令）的 Gauge
// （framework/sync/entitysync/policy 的同名指标）。
const logBacklogMetric = "entitysync.interest.log_backlog"

// logBacklogGauge 读此刻的日志积压。测量循环在每个 tick 的 Flush 之后读一次，写进 server.json 的 log_backlog_per_tick，
// 看运行中的积压而不只是运行末的值（KNOWN-LIMITS“Interest 单写者 ⑧”：运行末为 0 不说明运行中没有积压）。
// 读在 tick 的计时之外。指标从未设置过（on_change 之外的模式还没跑过政策阶段、或没有 Interest）时 found 为 false。
func logBacklogGauge() (value int64, found bool) {
	for _, m := range metrics.Snapshot() {
		if m.Name == logBacklogMetric && m.Kind == metrics.KindGauge {
			return m.Value, true
		}
	}
	return 0, false
}

// backlogReport 把逐 tick 的积压样本汇成 server.json 的一段：分位、非零 tick 数与样本数；没有样本时返回 nil。
func backlogReport(samples []float64) map[string]any {
	if len(samples) == 0 {
		return nil
	}
	nonzero := 0
	for _, v := range samples {
		if v > 0 {
			nonzero++
		}
	}
	return map[string]any{"samples": len(samples), "nonzero_ticks": nonzero, "entries": quantiles(slices.Clone(samples))}
}
