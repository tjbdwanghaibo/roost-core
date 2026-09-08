package combatcomponent

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/skill"
)

// Resource commands touch attribute bases directly; every refusal below is
// what keeps a skill from minting or overspending a pool. Each is pinned, and
// each must leave the base and the revision untouched.
func TestResourceCommandRefusesEachIllegalOperation(t *testing.T) {
	cases := []struct {
		name    string
		command skill.ResourceCommand
		check   func(t *testing.T, err error)
	}{
		{"negative spend", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: -1}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"spend beyond the pool", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: 51}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"set below zero", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "set", Amount: -3}, func(t *testing.T, err error) {
			if !errors.Is(err, skill.ErrInsufficientResource) {
				t.Fatalf("err = %v, want ErrInsufficientResource", err)
			}
		}},
		{"unknown operation", skill.ResourceCommand{Target: 2, Resource: 5, Operation: "multiply", Amount: 2}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), `unsupported resource operation "multiply"`) {
				t.Fatalf("err = %v", err)
			}
		}},
		{"unmapped handle", skill.ResourceCommand{Target: 2, Resource: 9, Operation: "add", Amount: 1}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "resource handle 9 has no attribute mapping") {
				t.Fatalf("err = %v", err)
			}
		}},
		{"entity without combat component", skill.ResourceCommand{Target: 99, Resource: 5, Operation: "add", Amount: 1}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "entity 99 has no combat component") {
				t.Fatalf("err = %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, revision, _, defender := newAdapterFixture()
			_, handled, err := adapter.Apply(skill.EffectCommand{Payload: tc.command})
			if !handled {
				t.Fatal("resource command not handled")
			}
			tc.check(t, err)
			if defender.AttributeBase(5) != 50 || revision.revision != 0 {
				t.Fatalf("a refused resource command changed state: base=%d revision=%d", defender.AttributeBase(5), revision.revision)
			}
		})
	}
}

// PayCosts is atomic and strict: a negative entry, a total that overflows, or
// an entity without a component must refuse before any deduction.
func TestPayCostsRefusesNegativeOverflowingAndComponentlessPayments(t *testing.T) {
	cases := []struct {
		name    string
		payment skill.CostPayment
		text    string
	}{
		{"negative cost", skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: -5}}}, "negative cost"},
		{"overflowing total", skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: math.MaxInt64}, {Resource: "mana", Amount: 1}}}, "cost total overflows"},
		{"entity without component", skill.CostPayment{Entity: 99, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}}, "entity 99 has no combat component"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, revision, _, defender := newAdapterFixture()
			_, err := adapter.PayCosts(tc.payment)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("PayCosts = %v, want %q", err, tc.text)
			}
			if defender.AttributeBase(5) != 50 || revision.revision != 0 {
				t.Fatalf("a refused payment changed state: base=%d revision=%d", defender.AttributeBase(5), revision.revision)
			}
		})
	}
}
