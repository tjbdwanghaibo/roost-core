package attribute

import "testing"

// The attribute profile is generated code plus two hand-written helpers, so
// the test covers what a project would get wrong: that the derived attribute
// is the formula's alone, that the dirty mask reports what moved, and that a
// container hands out copies.
func TestDerivedPowerFollowsItsInputs(t *testing.T) {
	profile := ForLevel(1)
	if profile.HP != 110 || profile.Attack != 12 {
		t.Fatalf("level 1 = HP %d attack %d", profile.HP, profile.Attack)
	}
	if want := profile.Attack*2 + profile.HP/10; profile.Power != want {
		t.Fatalf("power = %d, want %d", profile.Power, want)
	}
	profile.ClearDirty()

	moved := profile.LevelUp(2)
	if moved&AttrMaskHP == 0 || moved&AttrMaskAttack == 0 || moved&AttrMaskPower == 0 {
		t.Fatalf("level-up reported mask %d, expected hp, attack and power", moved)
	}
	if want := profile.Attack*2 + profile.HP/10; profile.Power != want {
		t.Fatalf("power did not follow the level-up: %d want %d", profile.Power, want)
	}
	// A derived attribute is the formula's: nothing may set it directly.
	if profile.SetDirectAttr(AttrPower, 9999) {
		t.Error("SetDirectAttr wrote the derived attribute")
	}
}

func TestContainerHandsOutACopy(t *testing.T) {
	container := NewContainer()
	live := ForLevel(3)
	container.Install(Base, live)

	read, ok := CombatIn(container, Base)
	if !ok {
		t.Fatal("the typed accessor found nothing at the base layer")
	}
	read.SetHP(1)
	if live.HP == 1 {
		t.Fatal("writing to a snapshot reached the profile in the container")
	}
	if _, ok := CombatIn(container, Selector{Layer: "buffs"}); ok {
		t.Error("an empty layer produced a profile")
	}
}
