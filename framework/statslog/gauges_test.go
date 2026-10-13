package statslog

import (
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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
	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(app.ModName("entity.runtime"), countingRuntime{entities: []entity.IThreadSafeEntity{statEntity{kind: 1}, statEntity{kind: 2}, statEntity{kind: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := connectTestLogger(mod, registry); err != nil {
		t.Fatal(err)
	}
	reg, ok := app.Lookup[*metrics.Registry](registry, app.ModMetrics)
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

// 某个 kind / category 的实体全部卸载后对应 gauge 回到 0：JSONL、/metrics 与面板说的是同一件事，不能把上一次的非零值留在 gauge 上。
// 历史：RR-20261005-NC-164。

// mutableRuntime 让用例在两次采集之间换掉实体集合。
type mutableRuntime struct {
	mu       sync.Mutex
	entities []entity.IThreadSafeEntity
}

func (r *mutableRuntime) set(entities ...entity.IThreadSafeEntity) {
	r.mu.Lock()
	r.entities = entities
	r.mu.Unlock()
}
func (r *mutableRuntime) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entities)
}
func (r *mutableRuntime) Range(fn func(entity.IThreadSafeEntity) bool) {
	r.mu.Lock()
	entities := append([]entity.IThreadSafeEntity(nil), r.entities...)
	r.mu.Unlock()
	for _, e := range entities {
		if !fn(e) {
			return
		}
	}
}

type categoryEntity struct {
	entity.IThreadSafeEntity
	category entity.EntityCategory
	kind     entity.EntityKind
}

func (e categoryEntity) GetEntityCategory() entity.EntityCategory { return e.category }
func (e categoryEntity) GetEntityKind() entity.EntityKind         { return e.kind }

func TestEntityGaugesReturnToZeroWhenAKindEmpties(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 7)
	cfg.Set("stats_log.enabled", true)
	cfg.Set("stats_log.dir", t.TempDir())
	cfg.Set("stats_log.interval", "1h")
	mod := New()
	if err := configureTestLogger(mod, cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	runtime := &mutableRuntime{}
	runtime.set(categoryEntity{category: 1, kind: 1}, categoryEntity{category: 2, kind: 2}, categoryEntity{category: 2, kind: 2})
	if err := registry.Register(app.ModName("entity.runtime"), runtime); err != nil {
		t.Fatal(err)
	}
	if err := connectTestLogger(mod, registry); err != nil {
		t.Fatal(err)
	}
	reg, _ := app.Lookup[*metrics.Registry](registry, app.ModMetrics)
	if err := mod.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	if got, _ := gaugeValue(t, reg, "entity.count_by_kind", metrics.Labels{"kind": "2"}); got != 2 {
		t.Fatalf("fixture: kind 2 gauge = %d, want 2", got)
	}

	// 所有 kind 2（category 2）的实体卸载了。
	runtime.set(categoryEntity{category: 1, kind: 1})
	if err := mod.FlushOnce(); err != nil {
		t.Fatal(err)
	}
	record := mod.collect()
	if _, inRecord := record.Entity.ByKind["2"]; inRecord {
		t.Fatalf("fixture: the JSONL record still lists kind 2: %+v", record.Entity.ByKind)
	}
	if got, present := gaugeValue(t, reg, "entity.count_by_kind", metrics.Labels{"kind": "2"}); present && got != 0 {
		t.Errorf("entity.count_by_kind{kind=2} = %d after every kind-2 entity unloaded; the record says 0", got)
	}
	if got, present := gaugeValue(t, reg, "entity.count_by_category", metrics.Labels{"category": "2"}); present && got != 0 {
		t.Errorf("entity.count_by_category{category=2} = %d after the category emptied; the record says 0", got)
	}
	if got, _ := gaugeValue(t, reg, "entity.count_by_kind", metrics.Labels{"kind": "1"}); got != 1 {
		t.Errorf("entity.count_by_kind{kind=1} = %d, want 1", got)
	}
}
