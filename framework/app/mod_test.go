// Mod 排序与校验、能力注册表。

package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type orderedTestMod struct {
	name     ModName
	hard     []ModName
	optional []ModName
}

func (m *orderedTestMod) Name() ModName                { return m.name }
func (m *orderedTestMod) Init(*viper.Viper) error      { return nil }
func (m *orderedTestMod) Provide(*Registry) error      { return nil }
func (m *orderedTestMod) Start() error                 { return nil }
func (m *orderedTestMod) Stop()                        {}
func (m *orderedTestMod) DependsOn() []ModName         { return m.hard }
func (m *orderedTestMod) OptionalDependsOn() []ModName { return m.optional }

func TestSortModsOrdersPresentOptionalDependencies(t *testing.T) {
	consumer := &orderedTestMod{name: "consumer", optional: []ModName{"optional"}}
	dependency := &orderedTestMod{name: "optional"}

	got, err := sortMods([]Mod{consumer, dependency}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := modNames(got); !reflect.DeepEqual(names, []ModName{"optional", "consumer"}) {
		t.Fatalf("order = %v", names)
	}
}

func TestSortModsIgnoresAbsentOptionalDependencies(t *testing.T) {
	consumer := &orderedTestMod{name: "consumer", optional: []ModName{"not-installed"}}

	got, err := sortMods([]Mod{consumer}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := modNames(got); !reflect.DeepEqual(names, []ModName{"consumer"}) {
		t.Fatalf("order = %v", names)
	}
}

func TestSortModsDetectsOptionalDependencyCycle(t *testing.T) {
	a := &orderedTestMod{name: "a", optional: []ModName{"b"}}
	b := &orderedTestMod{name: "b", optional: []ModName{"a"}}

	if _, err := sortMods([]Mod{a, b}, nil); err == nil {
		t.Fatal("expected optional dependency cycle to fail")
	}
}

func modNames(mods []Mod) []ModName {
	out := make([]ModName, len(mods))
	for i, mod := range mods {
		out[i] = mod.Name()
	}
	return out
}

// 单 Mod 与多个 Mod 一样校验名字、缺失依赖与环，非法依赖不进入真实 App.Init。

func TestSingleModValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mods      []Mod
		external  map[ModName]struct{}
		wantError bool
	}{
		{"empty", nil, nil, false},
		{"valid_single", []Mod{&orderedTestMod{name: "a"}}, nil, false},
		{"single_nil", []Mod{nil}, nil, true},
		{"single_unnamed", []Mod{&orderedTestMod{}}, nil, true},
		{"single_missing_hard", []Mod{&orderedTestMod{name: "a", hard: []ModName{"missing"}}}, nil, true},
		{"single_hard_self_cycle", []Mod{&orderedTestMod{name: "a", hard: []ModName{"a"}}}, nil, true},
		{"single_optional_self_cycle", []Mod{&orderedTestMod{name: "a", optional: []ModName{"a"}}}, nil, true},
		{"missing_optional_allowed", []Mod{&orderedTestMod{name: "a", optional: []ModName{"missing"}}}, nil, false},
		{"external_hard_allowed", []Mod{&orderedTestMod{name: "a", hard: []ModName{"shared"}}}, map[ModName]struct{}{"shared": {}}, false},
		{"pair_missing_hard_rejected", []Mod{&orderedTestMod{name: "a", hard: []ModName{"missing"}}, &orderedTestMod{name: "b"}}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sortMods(tc.mods, tc.external)
			t.Logf("mods=%d error=%v", len(tc.mods), err)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
		})
	}
}

type boundaryInitTrackingMod struct {
	orderedTestMod
	initialized bool
}

func (m *boundaryInitTrackingMod) Init(*viper.Viper) error { m.initialized = true; return nil }

// Exercise Execute rather than only the private ordering helper. Existing
// newTestApp keeps logging in t.TempDir and uses the real App implementation.
func TestSingleModExecuteFailsBeforeInit(t *testing.T) {
	for _, scope := range []string{"shared", "service"} {
		t.Run(scope, func(t *testing.T) {
			mod := &boundaryInitTrackingMod{orderedTestMod: orderedTestMod{name: "consumer", hard: []ModName{"missing"}}}
			a := newTestApp(t, &errService{})
			if scope == "shared" {
				a.Mods(mod)
			} else {
				a.services["game"].mods = []Mod{mod}
			}
			a.RootCmd().SetArgs([]string{"game"})
			err := a.Execute()
			t.Logf("scope=%s initialized=%v error=%v", scope, mod.initialized, err)
			if err == nil || !strings.Contains(err.Error(), "unknown mod dependency") || mod.initialized {
				t.Fatalf("invalid dependency graph reached Init: initialized=%v err=%v", mod.initialized, err)
			}
		})
	}
}

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
