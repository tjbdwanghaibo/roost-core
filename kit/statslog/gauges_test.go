package statslog

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

// countingRuntime stands in for the Nest entity runtime: two entities of
// category 1, kinds 1 and 2.
type countingRuntime struct{ entities []entity.IThreadSafeEntity }

func (r countingRuntime) Len() int { return len(r.entities) }
func (r countingRuntime) Range(fn func(entity.IThreadSafeEntity) bool) {
	for _, e := range r.entities {
		if !fn(e) {
			return
		}
	}
}

type statEntity struct {
	entity.IThreadSafeEntity
	kind entity.EntityKind
}

func (e statEntity) GetEntityCategory() entity.EntityCategory { return 1 }
func (e statEntity) GetEntityKind() entity.EntityKind         { return e.kind }

func gaugeValue(t *testing.T, reg *metrics.Registry, name string, labels metrics.Labels) (int64, bool) {
	t.Helper()
	for _, metric := range reg.Snapshot() {
		if metric.Name != name {
			continue
		}
		match := true
		for key, want := range labels {
			if metric.Labels[key] != want {
				match = false
			}
		}
		if match {
			return metric.Value, true
		}
	}
	return 0, false
}

// Every observation the stats log writes is also published as gauges, so the
// JSONL file, /metrics and the dashboards agree — and so a process's
// goroutine count, heap and entity population are watchable without reading
// a file on the box.
func TestEachCollectionPublishesRuntimeAndEntityGauges(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 7)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", t.TempDir())
	cfg.Set("stats_log.interval", "1h")
	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModEntityRuntime, countingRuntime{entities: []entity.IThreadSafeEntity{statEntity{kind: 1}, statEntity{kind: 2}, statEntity{kind: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	reg, ok := app.Lookup[*metrics.Registry](registry, mods.ModMetrics)
	if !ok || reg == nil {
		t.Fatal("the app registry publishes no metrics registry")
	}
	if err := mod.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()

	if got, ok := gaugeValue(t, reg, "runtime.goroutines", nil); !ok || got <= 0 {
		t.Fatalf("runtime.goroutines gauge = %d (present=%v)", got, ok)
	}
	if got, ok := gaugeValue(t, reg, "runtime.heap_alloc_bytes", nil); !ok || got <= 0 {
		t.Fatalf("runtime.heap_alloc_bytes gauge = %d (present=%v)", got, ok)
	}
	if got, ok := gaugeValue(t, reg, "entity.count", nil); !ok || got != 3 {
		t.Fatalf("entity.count gauge = %d (present=%v), want 3", got, ok)
	}
	if got, ok := gaugeValue(t, reg, "entity.count_by_kind", metrics.Labels{"kind": "2"}); !ok || got != 2 {
		t.Fatalf("entity.count_by_kind{kind=2} = %d (present=%v), want 2", got, ok)
	}
	if got, ok := gaugeValue(t, reg, "entity.count_by_category", metrics.Labels{"category": "1"}); !ok || got != 3 {
		t.Fatalf("entity.count_by_category{category=1} = %d (present=%v), want 3", got, ok)
	}
	record, ok := mod.CollectStats().(StatsRecord)
	if !ok || record.Entity.Total != 3 {
		t.Fatalf("CollectStats = %#v", mod.CollectStats())
	}
}
