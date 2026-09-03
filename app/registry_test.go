package app

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"

	"github.com/spf13/viper"
)

func TestNewRegistryInstallsRuntimeObsRegistry(t *testing.T) {
	old := metrics.DefaultRegistry()
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })
	reg := NewRegistry(viper.New())
	registry := MustLookup[*metrics.Registry](reg, ModMetrics)

	registry.IncCounter("app_registry_test_total", metrics.Labels{"case": "runtime"}, 1)

	found := false
	for _, metric := range registry.Snapshot() {
		if metric.Name == "app_registry_test_total" && metric.Value == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("package obs facade did not write into app runtime registry")
	}
}

func TestRegistryRegisterBatchIsAtomic(t *testing.T) {
	reg := NewRegistry(viper.New())
	if err := reg.Register("occupied", 1); err != nil {
		t.Fatal(err)
	}
	err := reg.RegisterBatch(
		Capability{Name: "new_capability", Value: 2},
		Capability{Name: "occupied", Value: 3},
	)
	if err == nil {
		t.Fatal("RegisterBatch succeeded with an occupied capability")
	}
	if _, exists := reg.Get("new_capability"); exists {
		t.Fatal("RegisterBatch published a partial capability set")
	}
}
