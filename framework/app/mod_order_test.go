package app

import (
	"reflect"
	"testing"

	"github.com/spf13/viper"
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
