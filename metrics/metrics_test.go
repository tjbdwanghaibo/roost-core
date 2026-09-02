package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRegistrySnapshot(t *testing.T) {
	reg := NewRegistry()
	reg.IncCounter("requests", Labels{"handler": "login"}, 2)
	reg.SetGauge("online", nil, 10)
	reg.ObserveDuration("cost", Labels{"handler": "login"}, 3*time.Millisecond)
	snap := reg.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	foundTimer := false
	for _, metric := range snap {
		if metric.Name == "cost" {
			foundTimer = metric.Count == 1 && metric.TotalNanos == int64(3*time.Millisecond)
		}
	}
	if !foundTimer {
		t.Fatalf("timer not found in %+v", snap)
	}
}

func TestPrometheusTextDoesNotDoubleTotalSuffix(t *testing.T) {
	reg := NewRegistry()
	reg.IncCounter("bus.dispatch.total", nil, 1)
	reg.IncCounter("bus_dead_letter_total", nil, 1)

	text := string(PrometheusText(reg.Snapshot()))

	if strings.Contains(text, "total_total") {
		t.Fatalf("prometheus text should not double total suffix:\n%s", text)
	}
	if !strings.Contains(text, "bus_dispatch_total 1") || !strings.Contains(text, "bus_dead_letter_total 1") {
		t.Fatalf("prometheus text missing normalized counters:\n%s", text)
	}
}

func TestRegistryLimitsMetricSeriesCardinality(t *testing.T) {
	reg := NewRegistry(WithMaxSeriesPerMetric(2))

	reg.IncCounter("player.event.total", Labels{"player_id": "1"}, 1)
	reg.IncCounter("player.event.total", Labels{"player_id": "2"}, 1)
	reg.IncCounter("player.event.total", Labels{"player_id": "3"}, 1)

	snap := reg.Snapshot()
	series := 0
	for _, metric := range snap {
		if metric.Name == "player.event.total" {
			series++
		}
	}
	if series != 2 {
		t.Fatalf("series count = %d, want 2 snapshot=%+v", series, snap)
	}
	if dropped := reg.DroppedSeries(); dropped != 1 {
		t.Fatalf("dropped series = %d, want 1", dropped)
	}
}

func TestSeriesLimitDropIsVisible(t *testing.T) {
	reg := NewRegistry(WithMaxSeriesPerMetric(1))
	reg.IncCounter("demo.metric", Labels{"k": "a"}, 1)
	reg.IncCounter("demo.metric", Labels{"k": "b"}, 1) // over budget: dropped
	reg.IncCounter("demo.metric", Labels{"k": "c"}, 1) // dropped again
	if reg.DroppedSeries() != 2 {
		t.Fatalf("dropped = %d, want 2", reg.DroppedSeries())
	}
	for _, metric := range reg.Snapshot() {
		if metric.Name == "obs.series.dropped" &&
			metric.Kind == KindCounter &&
			metric.Labels["metric"] == "demo.metric" &&
			metric.Value == 2 {
			return
		}
	}
	t.Fatalf("obs.series.dropped series missing: %+v", reg.Snapshot())
}

func TestHistogramObserveQuantileAndExport(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < 90; i++ {
		reg.ObserveHistogram("robot.call", Labels{"msg": "ping"}, 3*time.Millisecond)
	}
	for i := 0; i < 10; i++ {
		reg.ObserveHistogram("robot.call", Labels{"msg": "ping"}, 300*time.Millisecond)
	}
	p50 := reg.HistogramQuantile("robot.call", Labels{"msg": "ping"}, 0.50)
	p99 := reg.HistogramQuantile("robot.call", Labels{"msg": "ping"}, 0.99)
	if p50 <= 0 || p50 > 8*time.Millisecond {
		t.Fatalf("p50 = %v, want within the 2-4ms bucket range", p50)
	}
	if p99 < 100*time.Millisecond || p99 > 600*time.Millisecond {
		t.Fatalf("p99 = %v, want within the ~256-512ms bucket range", p99)
	}
	// Unknown series is zero, not a panic.
	if q := reg.HistogramQuantile("missing", nil, 0.5); q != 0 {
		t.Fatalf("missing series quantile = %v", q)
	}
	var found *Metric
	for _, m := range reg.Snapshot() {
		if m.Kind == KindHistogram && m.Name == "robot.call" {
			found = &m
			break
		}
	}
	if found == nil || found.Count != 100 {
		t.Fatalf("histogram snapshot missing: %+v", found)
	}
	text := string(PrometheusText([]Metric{*found}))
	for _, want := range []string{`robot_call_bucket{msg="ping",le="+Inf"} 100`, "robot_call_count", "robot_call_sum_nanos"} {
		if !strings.Contains(text, want) {
			t.Fatalf("prometheus export missing %q:\n%s", want, text)
		}
	}
	// Cumulative buckets must be monotonically non-decreasing.
	last := int64(-1)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "robot_call_bucket") {
			var v int64
			if _, err := fmt.Sscanf(line[strings.LastIndex(line, " ")+1:], "%d", &v); err != nil {
				t.Fatal(err)
			}
			if v < last {
				t.Fatalf("buckets not cumulative:\n%s", text)
			}
			last = v
		}
	}
}
