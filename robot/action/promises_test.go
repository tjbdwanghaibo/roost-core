package action_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
)

type namedAction string

func (n namedAction) Name() string                                   { return string(n) }
func (n namedAction) Run(context.Context, *robot.Context, any) error { return nil }

// Actions are looked up by normalized name from scenario specs; a nameless or
// duplicated action would make the spec bind to the wrong code.
func TestActionRegistryRefusesNamelessAndDuplicateActions(t *testing.T) {
	r := action.NewRegistry()
	for _, tc := range []struct {
		name string
		a    action.Action
		text string
	}{
		{"nil action", nil, "action is nil"},
		{"blank name", namedAction("   "), "name is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.a); err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Register = %v, want %q", err, tc.text)
			}
		})
	}
	if err := r.Register(namedAction("Login")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(namedAction("login")); err == nil || !strings.Contains(err.Error(), `duplicate "login"`) {
		t.Fatalf("duplicate = %v", err)
	}
}
