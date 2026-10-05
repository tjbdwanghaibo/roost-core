package statslog

// RR-20261005-NC-164：某个 kind / category 的实体全部卸载后，对应 gauge 要回到 0。
//
// publishGauges 注释承诺 JSONL、/metrics 与面板说的是同一件事。collectEntityStats 只为当前
// 还有实体的 kind / category 计数，计数为 0 的键根本不在 map 里，publishGauges 只遍历 map，
// 于是上一次的非零值永远留在 gauge 上：JSONL 里这个 kind 已经消失（等于 0），/metrics 却一直
// 报最后一次的人数（例如所有玩家下线后 entity.count_by_kind 仍报 N）。

import (
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

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
	mod := NewStatsLogMod()
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(cfg)
	runtime := &mutableRuntime{}
	runtime.set(categoryEntity{category: 1, kind: 1}, categoryEntity{category: 2, kind: 2}, categoryEntity{category: 2, kind: 2})
	if err := registry.Register(mods.ModEntityRuntime, runtime); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	reg, _ := app.Lookup[*metrics.Registry](registry, mods.ModMetrics)
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
