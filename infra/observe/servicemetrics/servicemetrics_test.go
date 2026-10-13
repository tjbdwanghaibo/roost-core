package servicemetrics

import (
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type recorder struct {
	mu       sync.Mutex
	accepted []string
	refused  [][2]string
	replayed []string
	dropped  map[string]int
	conflict []string
	depth    map[string]int64
}

func newRecorder() *recorder {
	return &recorder{dropped: map[string]int{}, depth: map[string]int64{}}
}

func (r *recorder) Accepted(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepted = append(r.accepted, op)
}

func (r *recorder) Refused(op, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = append(r.refused, [2]string{op, reason})
}

func (r *recorder) Replayed(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replayed = append(r.replayed, op)
}

func (r *recorder) Dropped(op string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped[op] += count
}

func (r *recorder) Conflict(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conflict = append(r.conflict, op)
}

func (r *recorder) Depth(name string, value int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.depth[name] = value
}

// A nil reporter must never be a reason for an operation to fail. Every call
// site is unconditional precisely so a report cannot be forgotten, which only
// works if the nil case is safe.
func TestANilReporterIsSafeOnEveryMethod(t *testing.T) {
	sink := Wrap(nil)
	sink.Accepted("op")
	sink.Refused("op", "why")
	sink.Replayed("op")
	sink.Dropped("op", 3)
	sink.Conflict("op")
	sink.Depth("queue", 7)
	if sink.Enabled() {
		t.Fatal("a nil reporter reports itself as enabled")
	}
}

func TestEveryEventReachesTheReporter(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	if !sink.Enabled() {
		t.Fatal("a wrapped reporter reports itself as disabled")
	}
	sink.Accepted("publish")
	sink.Refused("publish", "denied")
	sink.Replayed("publish")
	sink.Dropped("retention", 4)
	sink.Conflict("append")
	sink.Depth("queue", 12)

	if len(recorder.accepted) != 1 || recorder.accepted[0] != "publish" {
		t.Fatalf("accepted = %v", recorder.accepted)
	}
	if len(recorder.refused) != 1 || recorder.refused[0] != [2]string{"publish", "denied"} {
		t.Fatalf("refused = %v", recorder.refused)
	}
	if len(recorder.replayed) != 1 {
		t.Fatalf("replayed = %v", recorder.replayed)
	}
	if recorder.dropped["retention"] != 4 {
		t.Fatalf("dropped = %v", recorder.dropped)
	}
	if len(recorder.conflict) != 1 {
		t.Fatalf("conflict = %v", recorder.conflict)
	}
	if recorder.depth["queue"] != 12 {
		t.Fatalf("depth = %v", recorder.depth)
	}
}

// A zero or negative drop count is not an event. Reporting it would put noise
// in the one signal that says data was lost.
func TestAZeroDropCountIsNotReported(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	sink.Dropped("retention", 0)
	sink.Dropped("retention", -1)
	if len(recorder.dropped) != 0 {
		t.Fatalf("a non-positive drop count was reported: %v", recorder.dropped)
	}
	sink.Dropped("retention", 1)
	if recorder.dropped["retention"] != 1 {
		t.Fatal("a real drop was not reported")
	}
}

// A Sink is copied by value into services, so it must be safe to share across
// goroutines — every service reports from concurrent request paths.
func TestASinkIsSafeToShareAcrossGoroutines(t *testing.T) {
	recorder := newRecorder()
	sink := Wrap(recorder)
	var wait sync.WaitGroup
	for writer := 0; writer < 8; writer++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 50; i++ {
				sink.Accepted("op")
				sink.Conflict("op")
			}
		}()
	}
	wait.Wait()
	if len(recorder.accepted) != 400 {
		t.Fatalf("accepted %d events, want 400", len(recorder.accepted))
	}
	if len(recorder.conflict) != 400 {
		t.Fatalf("conflict %d events, want 400", len(recorder.conflict))
	}
}

// Depth is a gauge, so the Recorder must replace rather than accumulate.
// Summing successive readings of a gauge produces a number that means nothing,
// and a test asserting on that number would pass for the wrong reason.
func TestRecorderTreatsDepthAsAGauge(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Depth("queue.a", 3)
	sink.Depth("queue.a", 5)
	if got := recorder.Count("depth:queue.a"); got != 5 {
		t.Fatalf("depth reported %d after readings of 3 then 5, want 5; %s", got, recorder.Events())
	}
}

// Dropped accumulates, because a drop count is a rate: two sweeps that each
// lost three tickets lost six tickets.
func TestRecorderAccumulatesDrops(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Dropped("ticket.expired", 3)
	sink.Dropped("ticket.expired", 3)
	if got := recorder.Count("dropped:ticket.expired"); got != 6 {
		t.Fatalf("two drops of 3 reported %d, want 6; %s", got, recorder.Events())
	}
}

// A refusal's reason is part of its identity: "a client is probing" and "the
// policy rejects everyone" have nothing in common but the operation name.
func TestRecorderKeepsRefusalReasonsApart(t *testing.T) {
	recorder := NewRecorder()
	sink := Wrap(recorder)
	sink.Refused("publish", "policy")
	sink.Refused("publish", "system_only")
	if got := recorder.Count("refused:publish:policy"); got != 1 {
		t.Fatalf("policy refusals reported %d, want 1; %s", got, recorder.Events())
	}
	if got := recorder.Count("refused:publish:system_only"); got != 1 {
		t.Fatalf("system-only refusals reported %d, want 1; %s", got, recorder.Events())
	}
}

// An empty Recorder has to render something a failure message can print,
// because "reported: " followed by nothing reads as a truncated log line.
func TestAnEmptyRecorderRendersReadably(t *testing.T) {
	if got := NewRecorder().Events(); got == "" {
		t.Fatal("an empty recorder rendered an empty string")
	}
}

// C6（维护者决定，docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：默认的生产 Reporter 把服务事件
// 写进进程的 metrics 注册表，Prometheus 导出上能看到；承诺：
//   - 六类事件各落到一个固定的指标名，服务 / 操作 / 原因 / 对象都在标签里，对象 key 不进指标名；
//   - 计数累加（Dropped 按条数），Depth / DepthOf 是 gauge（替换）；Dropped(0) 不建序列；
//   - 每次上报解析“当时”的默认注册表：先建 Reporter、后由 App 换上自己的注册表，事件仍落到 App 的那个；
//   - Sink.DepthOf 对没实现 KeyedReporter 的 Reporter 退回 Depth(name+"."+key)。

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
