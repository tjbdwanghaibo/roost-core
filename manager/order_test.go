package manager

import (
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
)

type orderManager struct {
	name      string
	dependsOn []string
}

func (o orderManager) Name() string              { return o.name }
func (o orderManager) Start(*app.Registry) error { return nil }
func (o orderManager) Stop()                     {}
func (o orderManager) DependsOn() []string       { return o.dependsOn }

func names(managers []app.IManager) []string {
	out := make([]string, 0, len(managers))
	for _, manager := range managers {
		out = append(out, manager.Name())
	}
	return out
}

// A cycle must be reported by name. Returning nil, or logging and carrying on,
// leaves the operator with a service that will not start and no idea which
// manager to look at.
func TestOrderRejectsCyclesByName(t *testing.T) {
	for _, testCase := range []struct {
		label    string
		managers []app.IManager
		want     string
	}{
		{
			label: "two-manager cycle",
			managers: []app.IManager{
				orderManager{name: "a", dependsOn: []string{"b"}},
				orderManager{name: "b", dependsOn: []string{"a"}},
			},
			want: "dependency cycle",
		},
		{
			label: "three-manager cycle",
			managers: []app.IManager{
				orderManager{name: "a", dependsOn: []string{"b"}},
				orderManager{name: "b", dependsOn: []string{"c"}},
				orderManager{name: "c", dependsOn: []string{"a"}},
			},
			want: "dependency cycle",
		},
		{
			label:    "self dependency",
			managers: []app.IManager{orderManager{name: "a", dependsOn: []string{"a"}}, orderManager{name: "b"}},
			want:     "dependency cycle at \"a\"",
		},
	} {
		_, err := Order(testCase.managers)
		if err == nil {
			t.Fatalf("%s: accepted", testCase.label)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: error %q does not contain %q", testCase.label, err, testCase.want)
		}
	}
}

// Depending on a manager that was never registered is a wiring mistake, and
// the message has to name both sides or the operator cannot act on it.
func TestOrderRejectsMissingDependencyNamingBothSides(t *testing.T) {
	_, err := Order([]app.IManager{
		orderManager{name: "nest", dependsOn: []string{"entity"}},
	})
	if err == nil {
		t.Fatal("a dependency on an unregistered manager was accepted")
	}
	for _, want := range []string{"nest", "entity", "missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestOrderRejectsDuplicateAndInvalidEntries(t *testing.T) {
	for _, testCase := range []struct {
		label    string
		managers []app.IManager
		want     string
	}{
		{"duplicate name", []app.IManager{orderManager{name: "a"}, orderManager{name: "a"}}, "duplicate manager \"a\""},
		{"empty name", []app.IManager{orderManager{name: "a"}, orderManager{name: ""}}, "empty name"},
		{"nil entry", []app.IManager{orderManager{name: "a"}, nil}, "entry 1 is nil"},
		{"single nil entry", []app.IManager{nil}, "entry 0 is nil"},
	} {
		_, err := Order(testCase.managers)
		if err == nil {
			t.Fatalf("%s: accepted", testCase.label)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: error %q does not contain %q", testCase.label, err, testCase.want)
		}
	}
}

// A diamond must place the shared dependency once, before both dependents,
// and keep registration order between the two independent middles.
func TestOrderHandlesDiamondDependencies(t *testing.T) {
	sorted, err := Order([]app.IManager{
		orderManager{name: "top", dependsOn: []string{"left", "right"}},
		orderManager{name: "left", dependsOn: []string{"base"}},
		orderManager{name: "right", dependsOn: []string{"base"}},
		orderManager{name: "base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := names(sorted)
	want := []string{"base", "left", "right", "top"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("diamond order: got %v want %v", got, want)
	}
}

// DependsOn order must not change the result: the sort normalises it to
// registration order so the sequence is reproducible.
func TestOrderIgnoresDependsOnListingOrder(t *testing.T) {
	forward, err := Order([]app.IManager{
		orderManager{name: "top", dependsOn: []string{"a", "b"}},
		orderManager{name: "a"},
		orderManager{name: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := Order([]app.IManager{
		orderManager{name: "top", dependsOn: []string{"b", "a"}},
		orderManager{name: "a"},
		orderManager{name: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(forward), ",") != strings.Join(names(reversed), ",") {
		t.Fatalf("DependsOn listing order changed the result: %v vs %v", names(forward), names(reversed))
	}
}

// An empty DependsOn entry is tolerated rather than treated as a manager
// named "": assembly code builds these lists conditionally.
func TestOrderSkipsEmptyDependencyNames(t *testing.T) {
	sorted, err := Order([]app.IManager{
		orderManager{name: "top", dependsOn: []string{"", "base", ""}},
		orderManager{name: "base"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(sorted), ","); got != "base,top" {
		t.Fatalf("order with empty dependency names: %s", got)
	}
}

func TestOrderAcceptsEmptyAndSingleInput(t *testing.T) {
	if sorted, err := Order(nil); err != nil || len(sorted) != 0 {
		t.Fatalf("nil input: %v %v", sorted, err)
	}
	sorted, err := Order([]app.IManager{orderManager{name: "only"}})
	if err != nil || len(sorted) != 1 || sorted[0].Name() != "only" {
		t.Fatalf("single input: %v %v", names(sorted), err)
	}
}
