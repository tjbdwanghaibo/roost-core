package servicemetrics

// C6（维护者决定，docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：默认的生产 Reporter 把服务事件
// 写进进程的 metrics 注册表，Prometheus 导出上能看到；承诺：
//   - 六类事件各落到一个固定的指标名，服务 / 操作 / 原因 / 对象都在标签里，对象 key 不进指标名；
//   - 计数累加（Dropped 按条数），Depth / DepthOf 是 gauge（替换）；Dropped(0) 不建序列；
//   - 每次上报解析“当时”的默认注册表：先建 Reporter、后由 App 换上自己的注册表，事件仍落到 App 的那个；
//   - Sink.DepthOf 对没实现 KeyedReporter 的 Reporter 退回 Depth(name+"."+key)。

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// withRegistry installs a fresh default registry for the test, as App.Run
// does, and restores the previous one afterwards.
func withRegistry(t *testing.T) *metrics.Registry {
	t.Helper()
	previous := metrics.DefaultRegistry()
	registry := metrics.NewRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })
	return registry
}

func TestMetricsReporterExportsEveryEventUnderAFixedName(t *testing.T) {
	// Built before the registry exists, like a generated collaborator.
	sink := Wrap(NewMetricsReporter("match"))
	registry := withRegistry(t)

	sink.Accepted("enqueue")
	sink.Accepted("enqueue")
	sink.Refused("enqueue", "full")
	sink.Replayed("enqueue")
	sink.Dropped("ticket.expired", 3)
	sink.Dropped("ticket.expired", 3)
	sink.Dropped("ticket.expired", 0)
	sink.Conflict("commit")
	sink.DepthOf("queue", "ranked:2:eu", 5)
	sink.DepthOf("queue", "ranked:2:eu", 2)
	sink.Depth("pending", 7)

	text := string(metrics.PrometheusText(registry.Snapshot()))
	for _, want := range []string{
		`service_accepted_total{op="enqueue",service="match"} 2`,
		`service_refused_total{op="enqueue",reason="full",service="match"} 1`,
		`service_replayed_total{op="enqueue",service="match"} 1`,
		`service_dropped_total{op="ticket.expired",service="match"} 6`,
		`service_conflict_total{op="commit",service="match"} 1`,
		`service_depth{key="ranked:2:eu",name="queue",service="match"} 2`,
		`service_depth{name="pending",service="match"} 7`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Fatalf("the export lacks %s:\n%s", want, text)
		}
	}
	// No metric NAME carries an object: everything before '{' is one of the
	// six fixed names.
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		name, _, _ := strings.Cut(line, "{")
		switch name {
		case "service_accepted_total", "service_refused_total", "service_replayed_total",
			"service_dropped_total", "service_conflict_total", "service_depth":
		default:
			t.Fatalf("exported metric %q is not one of the fixed names: %s", name, line)
		}
		if strings.Contains(name, "ranked") || strings.Contains(name, "eu") {
			t.Fatalf("a queue key leaked into a metric name: %s", line)
		}
	}
	if got := len(strings.Split(strings.TrimSpace(text), "\n")); got != 7 {
		t.Fatalf("%d series, want 7 (Dropped(0) must not create one):\n%s", got, text)
	}
}

// depthOnly is a project's own Reporter written before KeyedReporter existed.
type depthOnly struct{ names []string }

func (*depthOnly) Accepted(string)              {}
func (*depthOnly) Refused(string, string)       {}
func (*depthOnly) Replayed(string)              {}
func (*depthOnly) Dropped(string, int)          {}
func (*depthOnly) Conflict(string)              {}
func (r *depthOnly) Depth(name string, _ int64) { r.names = append(r.names, name) }

func TestDepthOfFallsBackToTheOldNameForAReporterWithoutKeys(t *testing.T) {
	reporter := &depthOnly{}
	Wrap(reporter).DepthOf("board", "arena", 1)
	if len(reporter.names) != 1 || reporter.names[0] != "board.arena" {
		t.Fatalf("a Reporter without DepthOf received %v, want [board.arena] as before", reporter.names)
	}
	Wrap(nil).DepthOf("board", "arena", 1) // nil-safe like every other Sink method
}
