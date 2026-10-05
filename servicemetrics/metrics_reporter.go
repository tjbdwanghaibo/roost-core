package servicemetrics

import "github.com/tjbdwanghaibo/roost-core/metrics"

// 默认的生产 Reporter（维护者决定 C6，docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）。
//
// 服务事件写进进程的 metrics 注册表，由 kit ops 的 /metrics 以 Prometheus 文本导出。指标名只有下面
// 六个，服务、操作、原因与对象都放在标签里——之前队列 key、看板 ID 拼在深度名字里，每个对象就是一个新
// 指标名（N12 O1）。注册表没有删除序列的入口，所以这里不加按运行 / 请求 / 实体变化的标签。
const (
	// MetricAccepted counts Accepted events (labels service, op).
	MetricAccepted = "service.accepted.total"
	// MetricRefused counts Refused events (labels service, op, reason).
	MetricRefused = "service.refused.total"
	// MetricReplayed counts Replayed events (labels service, op).
	MetricReplayed = "service.replayed.total"
	// MetricDropped sums Dropped counts (labels service, op).
	MetricDropped = "service.dropped.total"
	// MetricConflict counts Conflict events (labels service, op).
	MetricConflict = "service.conflict.total"
	// MetricDepth is the Depth / DepthOf gauge (labels service, name, and key
	// for a keyed depth).
	MetricDepth = "service.depth"
)

// MetricsReporter is the production Reporter: it writes every event into the
// process's metrics registry, which kit ops serves on /metrics.
//
// It resolves the registry on every report (metrics.DefaultRegistry) rather
// than when it is built. Collaborators are called while the App is being
// assembled, before App.Run creates the registry it exports and makes it the
// default; a registry captured at construction would be one nobody reads.
type MetricsReporter struct {
	service string
}

// NewMetricsReporter returns the Reporter for one service; service becomes the
// service label of every series it writes.
func NewMetricsReporter(service string) *MetricsReporter {
	return &MetricsReporter{service: service}
}

func (r *MetricsReporter) labels(pairs ...string) metrics.Labels {
	labels := metrics.Labels{"service": r.service}
	for i := 0; i+1 < len(pairs); i += 2 {
		labels[pairs[i]] = pairs[i+1]
	}
	return labels
}

// Accepted implements Reporter.
func (r *MetricsReporter) Accepted(op string) {
	metrics.IncCounter(MetricAccepted, r.labels("op", op), 1)
}

// Refused implements Reporter.
func (r *MetricsReporter) Refused(op, reason string) {
	metrics.IncCounter(MetricRefused, r.labels("op", op, "reason", reason), 1)
}

// Replayed implements Reporter.
func (r *MetricsReporter) Replayed(op string) {
	metrics.IncCounter(MetricReplayed, r.labels("op", op), 1)
}

// Dropped implements Reporter. A drop count accumulates: two sweeps that each
// dropped three dropped six.
func (r *MetricsReporter) Dropped(op string, count int) {
	if count > 0 {
		metrics.IncCounter(MetricDropped, r.labels("op", op), int64(count))
	}
}

// Conflict implements Reporter.
func (r *MetricsReporter) Conflict(op string) {
	metrics.IncCounter(MetricConflict, r.labels("op", op), 1)
}

// Depth implements Reporter: a gauge named by name.
func (r *MetricsReporter) Depth(name string, value int64) {
	metrics.SetGauge(MetricDepth, r.labels("name", name), value)
}

// DepthOf implements KeyedReporter: the same gauge, one series per key. The
// key's cardinality is the caller's — a match queue, a rank board — and must
// be a bounded set; see KeyedReporter.
func (r *MetricsReporter) DepthOf(name, key string, value int64) {
	metrics.SetGauge(MetricDepth, r.labels("name", name, "key", key), value)
}

var (
	_ Reporter      = (*MetricsReporter)(nil)
	_ KeyedReporter = (*MetricsReporter)(nil)
)
