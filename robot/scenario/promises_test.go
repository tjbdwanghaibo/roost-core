package scenario_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
)

type namedScenario string

func (n namedScenario) Name() string                              { return string(n) }
func (n namedScenario) Run(context.Context, *robot.Context) error { return nil }

func expectText(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want it to contain %q", err, text)
	}
}

// Scenarios are addressed by (normalized) name from specs and admin commands;
// an unnamed or duplicated one would be unreachable or ambiguous.
func TestScenarioRegistryRefusesNamelessAndDuplicateScenarios(t *testing.T) {
	r := scenario.NewRegistry()
	expectText(t, r.Register(nil), "scenario is nil")
	expectText(t, r.Register(namedScenario("  ")), "name is required")
	if err := r.Register(namedScenario("Raid")); err != nil {
		t.Fatal(err)
	}
	expectText(t, r.Register(namedScenario("raid")), `duplicate "raid"`)
}

// The broken-document test only checked that *some* error came back; pin each
// refusal to its message so a dropped rule turns exactly one case red.
func TestSpecRefusesEachBrokenDocumentByMessage(t *testing.T) {
	cases := map[string]struct{ spec, text string }{
		"no scenarios":   {"scenarios: []\n", "spec has no scenarios"},
		"no name":        {"scenarios:\n  - node: {action: a}\n", "spec scenario without a name"},
		"duplicate":      {"scenarios:\n  - name: x\n    node: {action: a}\n  - name: X\n    node: {action: a}\n", `duplicate spec scenario "x"`},
		"empty node":     {"scenarios:\n  - name: x\n    node: {}\n", "empty node"},
		"two node kinds": {"scenarios:\n  - name: x\n    node: {action: a, wait: 1s}\n", "exactly one node kind"},
		"unknown key":    {"scenarios:\n  - name: x\n    node: {action: a}\n    typo: 1\n", "parse spec"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := scenario.ParseSpec([]byte(tc.spec))
			expectText(t, err, tc.text)
		})
	}
}
