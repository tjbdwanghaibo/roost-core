package combat

import (
	"math"
	"testing"
)

func TestAvoidancePreservesCriticalHook(t *testing.T) {
	for _, dodge := range []bool{false, true} {
		hooks := &recordingHooks{critical: "one-hit"}
		target := &Combatant{Alive: true, Health: 100, Dodge: dodge, Parry: !dodge}
		out, _ := ResolveDamage(nil, target, DamageInput{Amount: 20, CanCritical: true}, hooks)
		if out.Critical || len(hooks.consumed) != 0 {
			t.Fatalf("avoided hit consumed critical: %+v hooks=%v", out, hooks.consumed)
		}
	}
}

func TestVampOnlyHealsMissingHealthOfLivingSource(t *testing.T) {
	for _, source := range []Combatant{{Alive: false, Health: 0, MaxHealth: 100, VampBP: 10000}, {Alive: true, Health: 150, MaxHealth: 100, VampBP: 10000}, {Alive: true, Health: 95, MaxHealth: 100, VampBP: 10000}} {
		before := source.Health
		target := &Combatant{Alive: true, Health: 100}
		out, _ := ResolveDamage(&source, target, DamageInput{Amount: 50}, nil)
		want := int64(0)
		if source.Alive && before < source.MaxHealth {
			want = 5
		}
		if source.Health != before+want || out.VampHeal != want {
			t.Errorf("source before=%d after=%d vamp=%d want=%d", before, source.Health, out.VampHeal, want)
		}
	}
}

func TestDamageRejectsInvalidNegativeHealth(t *testing.T) {
	target := &Combatant{Alive: true, Health: -1}
	if out, ok := ResolveDamage(nil, target, DamageInput{Amount: 5}, nil); ok || out.HealthDamage != 0 || target.Health != -1 {
		t.Fatalf("invalid target mutated: out=%+v target=%+v ok=%v", out, target, ok)
	}
}

func TestModifierOverflowRemainsReversibleAndOrderIndependent(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		set := NewAttributeSet()
		mods := []int64{math.MaxInt64, math.MaxInt64, -math.MaxInt64}
		if reverse {
			mods[0], mods[2] = mods[2], mods[0]
		}
		for i, v := range mods {
			set.Grant(ModifierHandle(i+1), Modifier{Attribute: 1, Flat: v})
		}
		want := ScaleBasisPoints(math.MaxInt64, BasisPointScale)
		if got := set.Current(1); got != want {
			t.Errorf("order %v current=%d want=%d", reverse, got, want)
		}
		for i := range mods {
			set.Revoke(ModifierHandle(i + 1))
		}
		if got := set.Current(1); got != 0 {
			t.Errorf("revoke current=%d", got)
		}
		set.Grant(10, Modifier{Attribute: 1, Flat: math.MaxInt64}, Modifier{Attribute: 1, Flat: math.MaxInt64})
		if got := set.Current(1); got <= 0 {
			t.Errorf("positive sum wrapped: %d", got)
		}
	}
}

func TestReapplyBuffPermanentAndLowerStackLimit(t *testing.T) {
	for _, policy := range []BuffStackPolicy{BuffRefresh, BuffExtend} {
		buffs := NewBuffContainer()
		spec := BuffSpec{ID: 1, MaxStacks: 5, DurationTicks: 10, StackPolicy: policy}
		for range 4 {
			buffs.Apply(spec, 0, 1)
		}
		spec.DurationTicks, spec.MaxStacks = 0, 2
		buffs.Apply(spec, 5, 1)
		got := buffs.Active()[0]
		if got.DueTick != 0 || got.Stacks != 2 {
			t.Errorf("policy=%d due=%d stacks=%d", policy, got.DueTick, got.Stacks)
		}
	}
}
