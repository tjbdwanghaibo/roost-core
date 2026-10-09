package skill

import (
	"reflect"
	"testing"
)

func TestInspectReturnsStableCopies(t *testing.T) {
	program := mustCompileProgram(t, mustParseFixture(t, "simple_damage.json"))
	first := Inspect(program)
	second := Inspect(program)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("inspector is unstable: %#v %#v", first, second)
	}
	first.Phases[0].ID = "mutated"
	first.Input.Slots[0].Name = "mutated"
	if reflect.DeepEqual(first, Inspect(program)) {
		t.Fatal("inspector returned aliased program storage")
	}
}

func programViewsEqual(left, right ProgramView) bool { return reflect.DeepEqual(left, right) }
