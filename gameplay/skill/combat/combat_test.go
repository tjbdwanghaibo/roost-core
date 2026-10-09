package combat

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestAttributeSetGrantRevokeIsExactlyReversible(t *testing.T) {
	set := NewAttributeSet()
	const armor AttributeID = 7
	set.SetBase(armor, 100)
	if set.Current(armor) != 100 {
		t.Fatalf("base current = %d", set.Current(armor))
	}
	set.Grant(1, Modifier{Attribute: armor, Flat: 50})
	set.Grant(2, Modifier{Attribute: armor, RateBP: 2000})
	if got := set.Current(armor); got != 180 { // (100+50) * 1.2
		t.Fatalf("stacked current = %d, want 180", got)
	}
	// Re-granting a handle replaces, never duplicates.
	set.Grant(1, Modifier{Attribute: armor, Flat: 10})
	if got := set.Current(armor); got != 132 { // (100+10) * 1.2
		t.Fatalf("replaced current = %d, want 132", got)
	}
	set.Revoke(1)
	set.Revoke(2)
	set.Revoke(99) // unknown handle: no-op
	if got := set.Current(armor); got != 100 {
		t.Fatalf("revoked current = %d, want 100", got)
	}
	set.SetBounds(armor, AttributeBounds{Minimum: 0, Maximum: 90})
	if got := set.Current(armor); got != 90 {
		t.Fatalf("bounded current = %d, want 90", got)
	}
	set.Grant(3, Modifier{Attribute: armor, RateBP: -20000}) // rate floor at 0
	if got := set.Current(armor); got != 0 {
		t.Fatalf("negative rate current = %d, want 0", got)
	}
}

func TestAttributeSetObserverAndSnapshotOrder(t *testing.T) {
	set := NewAttributeSet()
	var touched []AttributeID
	set.Observe(func(id AttributeID) { touched = append(touched, id) })
	set.SetBase(5, 10)
	set.SetBase(5, 10) // unchanged: no notification
	set.Grant(1, Modifier{Attribute: 2, Flat: 1}, Modifier{Attribute: 9, RateBP: 100})
	if !reflect.DeepEqual(touched, []AttributeID{5, 2, 9}) {
		t.Fatalf("observer saw %v", touched)
	}
	snapshot := set.Snapshot()
	want := []AttributeValue{{Attribute: 2, Base: 0, Current: 1}, {Attribute: 5, Base: 10, Current: 10}, {Attribute: 9, Base: 0, Current: 0}}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestBuffContainerStackRefreshDispelAndImmunity(t *testing.T) {
	attributes := NewAttributeSet()
	container := NewBuffContainer()
	container.LinkAttributes(attributes)
	const haste AttributeID = 3
	spec := BuffSpec{ID: 10, Tags: []Tag{"magic"}, MaxStacks: 3, DurationTicks: 100, Modifiers: []Modifier{{Attribute: haste, Flat: 5}}}

	id, outcome := container.Apply(spec, 0, 1)
	if outcome != BuffApplied || id == 0 || attributes.Current(haste) != 5 {
		t.Fatalf("apply = %v haste=%d", outcome, attributes.Current(haste))
	}
	if _, outcome = container.Apply(spec, 10, 1); outcome != BuffStacked || attributes.Current(haste) != 10 {
		t.Fatalf("stack = %v haste=%d", outcome, attributes.Current(haste))
	}
	if _, outcome = container.Apply(spec, 20, 1); outcome != BuffStacked {
		t.Fatalf("stack 3 = %v", outcome)
	}
	if _, outcome = container.Apply(spec, 30, 1); outcome != BuffRefreshed || attributes.Current(haste) != 15 {
		t.Fatalf("capped stack = %v haste=%d", outcome, attributes.Current(haste))
	}
	active := container.Active()
	if len(active) != 1 || active[0].Stacks != 3 || active[0].DueTick != 130 {
		t.Fatalf("active = %+v", active)
	}
	// Immunity granted by another buff blocks matching tags.
	immunity := BuffSpec{ID: 11, GrantsImmunityTo: []Tag{"magic"}, DurationTicks: 50}
	if _, outcome = container.Apply(immunity, 30, 2); outcome != BuffApplied {
		t.Fatalf("immunity apply = %v", outcome)
	}
	if _, outcome = container.Apply(spec, 31, 1); outcome != BuffBlockedImmune {
		t.Fatalf("immune apply = %v", outcome)
	}
	// Dispel removes by tag and revokes modifiers.
	removed := container.Dispel("magic", 0)
	if len(removed) != 1 || removed[0].Spec.ID != 10 || attributes.Current(haste) != 0 {
		t.Fatalf("dispel = %+v haste=%d", removed, attributes.Current(haste))
	}
}

func TestBuffContainerTenacityAndExpiry(t *testing.T) {
	container := NewBuffContainer()
	container.SetTenacityBP(4000) // -40% duration
	stun := BuffSpec{ID: 20, Tags: []Tag{"control"}, DurationTicks: 10, TenacityAffected: true}
	container.Apply(stun, 100, 1)
	active := container.Active()
	if len(active) != 1 || active[0].DueTick != 106 {
		t.Fatalf("tenacity due = %+v", active)
	}
	// Floors at one tick, never zero.
	container.SetTenacityBP(10000)
	container.Apply(BuffSpec{ID: 21, DurationTicks: 10, TenacityAffected: true}, 100, 1)
	if due := container.Active()[1].DueTick; due != 101 {
		t.Fatalf("floored due = %d", due)
	}
	if expired := container.Tick(105); len(expired) != 1 || expired[0].Spec.ID != 21 {
		t.Fatalf("expired = %+v", expired)
	}
	if expired := container.Tick(106); len(expired) != 1 || expired[0].Spec.ID != 20 {
		t.Fatalf("expired = %+v", expired)
	}
	if len(container.Active()) != 0 {
		t.Fatal("container not empty")
	}
}

func TestBuffExtendPolicyAccumulatesDuration(t *testing.T) {
	container := NewBuffContainer()
	spec := BuffSpec{ID: 30, MaxStacks: 5, StackPolicy: BuffExtend, DurationTicks: 10}
	container.Apply(spec, 0, 1)
	container.Apply(spec, 5, 1)
	if due := container.Active()[0].DueTick; due != 20 {
		t.Fatalf("extended due = %d, want 20", due)
	}
}

func TestResolveDamageFullPipeline(t *testing.T) {
	source := &Combatant{Alive: true, Health: 50, MaxHealth: 100, Penetration: 20, DamageDealtBP: 12000, VampBP: 1000, ForceCritical: true, CriticalMultiplierBP: 20000}
	target := &Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: 30, Armor: 70, DamageTakenBP: 5000}
	outcome, ok := ResolveDamage(source, target, DamageInput{Amount: 1000, Type: DamageTypePhysical, CanCritical: true}, nil)
	if !ok {
		t.Fatal("resolve failed")
	}
	// armor 70-20=50 -> 1000*10000/15000=666; dealt 1.2 -> 799 taken 0.5 -> 399
	// (element default), crit 2.0 -> 798; shield 30 absorbed, 100 health -> dead at 100.
	if outcome.Mitigated != 666 || !outcome.Critical || outcome.Absorbed != 30 || outcome.HealthDamage != 100 || !outcome.Killed {
		t.Fatalf("outcome = %+v", outcome)
	}
	if target.Health != 0 || target.Shield != 0 || target.Alive {
		t.Fatalf("target = %+v", target)
	}
	if outcome.VampHeal != 10 || source.Health != 60 {
		t.Fatalf("vamp = %d source health = %d", outcome.VampHeal, source.Health)
	}
	if outcome.Result != ResultKilled {
		t.Fatalf("result = %q", outcome.Result)
	}
}

func TestResolveDamageAvoidanceImmunityAndFloors(t *testing.T) {
	target := &Combatant{Alive: true, Health: 100, MaxHealth: 100, Dodge: true}
	outcome, _ := ResolveDamage(nil, target, DamageInput{Amount: 50}, nil)
	if !outcome.Dodged || outcome.HealthDamage != 0 || outcome.Result != ResultDodged {
		t.Fatalf("dodge outcome = %+v", outcome)
	}
	target = &Combatant{Alive: true, Health: 100, MaxHealth: 100, SpellShield: true}
	outcome, _ = ResolveDamage(nil, target, DamageInput{Amount: 50}, nil)
	if !outcome.Immune || target.SpellShield || outcome.Result != ResultImmune {
		t.Fatalf("spell shield outcome = %+v target = %+v", outcome, target)
	}
	target = &Combatant{Alive: true, Health: 100, MaxHealth: 100, Block: true, MinimumDamage: 40, DamageCap: 45}
	outcome, _ = ResolveDamage(nil, target, DamageInput{Amount: 60}, nil)
	if !outcome.Blocked || outcome.HealthDamage != 40 {
		t.Fatalf("block+floor outcome = %+v", outcome)
	}
	if _, ok := ResolveDamage(nil, &Combatant{Alive: false}, DamageInput{Amount: 1}, nil); ok {
		t.Fatal("dead target accepted")
	}
	if _, ok := ResolveDamage(nil, nil, DamageInput{Amount: 1}, nil); ok {
		t.Fatal("nil target accepted")
	}
}

type recordingHooks struct {
	spell, critical, death string
	consumed               []string
	absorbed               int64
}

func (hooks *recordingHooks) PeekSpellShield() (string, bool) { return hooks.spell, hooks.spell != "" }
func (hooks *recordingHooks) PeekCriticalOverride() (string, bool) {
	return hooks.critical, hooks.critical != ""
}
func (hooks *recordingHooks) PeekDeathPrevention() (string, bool) {
	return hooks.death, hooks.death != ""
}
func (hooks *recordingHooks) ConsumeHook(hook string)         { hooks.consumed = append(hooks.consumed, hook) }
func (hooks *recordingHooks) OnShieldAbsorbed(absorbed int64) { hooks.absorbed += absorbed }

func TestResolveDamageHookInterceptions(t *testing.T) {
	hooks := &recordingHooks{spell: "spell_shield"}
	target := &Combatant{Alive: true, Health: 100, MaxHealth: 100}
	outcome, _ := ResolveDamage(nil, target, DamageInput{Amount: 50, SpellTagged: true}, hooks)
	if !outcome.Immune || outcome.Result != "spell_shield" || target.Health != 100 || len(hooks.consumed) != 1 {
		t.Fatalf("spell shield hook = %+v consumed=%v", outcome, hooks.consumed)
	}

	hooks = &recordingHooks{critical: "critical_override", death: "death_prevention"}
	target = &Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: 10}
	outcome, _ = ResolveDamage(nil, target, DamageInput{Amount: 200, CanCritical: true}, hooks)
	if !outcome.Critical || outcome.Killed || target.Health != 1 || target.Alive != true {
		t.Fatalf("hooked outcome = %+v target = %+v", outcome, target)
	}
	if outcome.Result != "death_prevention" || hooks.absorbed != 10 {
		t.Fatalf("result = %q absorbed = %d", outcome.Result, hooks.absorbed)
	}
	if !reflect.DeepEqual(hooks.consumed, []string{"critical_override", "death_prevention"}) {
		t.Fatalf("consumed = %v", hooks.consumed)
	}
}

func TestResolveHealAndAddShield(t *testing.T) {
	target := &Combatant{Alive: true, Health: 80, MaxHealth: 100}
	outcome, ok := ResolveHeal(target, 50)
	if !ok || outcome.Attempted != 50 || outcome.Effective != 20 || target.Health != 100 {
		t.Fatalf("heal = %+v target = %+v", outcome, target)
	}
	if _, ok := ResolveHeal(&Combatant{Alive: false}, 5); ok {
		t.Fatal("dead target healed")
	}
	added, ok := AddShield(target, 25)
	if !ok || added != 25 || target.Shield != 25 {
		t.Fatalf("shield = %d target = %+v", added, target)
	}
	target.Shield = math.MaxInt64 - 5
	if added, _ := AddShield(target, 10); added != 5 || target.Shield != math.MaxInt64 {
		t.Fatalf("saturating shield = %d %d", added, target.Shield)
	}
	if added, _ := AddShield(target, 10); added != 0 || target.Shield != math.MaxInt64 {
		t.Fatalf("full shield no-op = %d %d", added, target.Shield)
	}
}

func TestSaturatingMathAtInt64Extremes(t *testing.T) {
	if got := saturatingInt64Add(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("add = %d", got)
	}
	if got := saturatingInt64Sub(math.MinInt64, 1); got != math.MinInt64 {
		t.Fatalf("sub = %d", got)
	}
	if got := saturatingInt64Mul(math.MaxInt64, 2); got != math.MaxInt64 {
		t.Fatalf("mul = %d", got)
	}
	if got := saturatingInt64Mul(math.MinInt64, -1); got != math.MaxInt64 {
		t.Fatalf("mul min*-1 = %d", got)
	}
	if got := ScaleBasisPoints(math.MaxInt64, 20000); got != math.MaxInt64/10000 {
		t.Fatalf("scale = %d", got)
	}
}

func TestResolveDamageClampsNegativeRatesAndShield(t *testing.T) {
	// Regression: a "damage taken" debuff stacked below -10000 used to make
	// Absorbed negative — being hit granted shield.
	target := &Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: 5, DamageTakenBP: -8000}
	outcome, _ := ResolveDamage(nil, target, DamageInput{Amount: 50}, nil)
	if outcome.Absorbed != 0 || outcome.HealthDamage != 0 || target.Shield != 5 || target.Health != 100 {
		t.Fatalf("negative taken rate leaked: %+v target=%+v", outcome, target)
	}
	// Regression: a negative shield pool used to amplify damage severalfold.
	target = &Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: -40}
	outcome, _ = ResolveDamage(nil, target, DamageInput{Amount: 10}, nil)
	if outcome.Absorbed != 0 || outcome.HealthDamage != 10 || target.Shield != -40 {
		t.Fatalf("negative shield amplified damage: %+v target=%+v", outcome, target)
	}
	// Negative dealt rate and negative critical multiplier clamp the same way.
	source := &Combatant{Alive: true, Health: 10, MaxHealth: 10, DamageDealtBP: -100, ForceCritical: true, CriticalMultiplierBP: -5000}
	target = &Combatant{Alive: true, Health: 100, MaxHealth: 100}
	outcome, _ = ResolveDamage(source, target, DamageInput{Amount: 50, CanCritical: true}, nil)
	if outcome.HealthDamage != 0 || target.Health != 100 {
		t.Fatalf("negative dealt/critical rates leaked: %+v", outcome)
	}
}

func TestBuffIndependentPolicyNeverMerges(t *testing.T) {
	container := NewBuffContainer()
	spec := BuffSpec{ID: 40, StackPolicy: BuffIndependent, DurationTicks: 10}
	container.Apply(spec, 0, 1)
	container.Apply(spec, 5, 2)
	active := container.Active()
	if len(active) != 2 || active[0].DueTick != 10 || active[1].DueTick != 15 {
		t.Fatalf("independent instances = %+v", active)
	}
	if expired := container.Tick(10); len(expired) != 1 || expired[0].Source != 1 {
		t.Fatalf("expired = %+v", expired)
	}
}

func TestChanceRollIsDeterministicAndPurposeScoped(t *testing.T) {
	key := []byte("match-seed")
	first := RollValue(key, "crit", 1, 2, 3)
	if first != RollValue(key, "crit", 1, 2, 3) {
		t.Fatal("same coordinates rolled differently")
	}
	if first == RollValue(key, "dodge", 1, 2, 3) && first == RollValue(key, "crit", 1, 2, 4) {
		t.Fatal("purpose and coordinates do not separate rolls")
	}
	if ChanceRoll(key, "crit", 0, 1) || !ChanceRoll(key, "crit", 10000, 1) {
		t.Fatal("chance bounds broken")
	}
	// Uniformity sanity: ~25% chance over 4000 coordinates lands near 1000.
	hits := 0
	for coordinate := uint64(0); coordinate < 4000; coordinate++ {
		if ChanceRoll(key, "crit", 2500, coordinate) {
			hits++
		}
	}
	if hits < 850 || hits > 1150 {
		t.Fatalf("2500bp chance hit %d/4000", hits)
	}
}

// Regression: RestoreBuffContainer trusted persisted data — an instance id
// beyond the sequence (or a duplicate) would later collide with a freshly
// issued id, and ids double as attribute grant handles, corrupting modifier
// bookkeeping far from the corrupt snapshot.
func TestRestoreBuffContainerRejectsCorruptState(t *testing.T) {
	valid := BuffContainerState{Sequence: 2, Active: []BuffInstance{{Instance: 1, Spec: BuffSpec{ID: 1}, Stacks: 1}, {Instance: 2, Spec: BuffSpec{ID: 2}, Stacks: 1}}}
	if _, err := RestoreBuffContainer(valid); err != nil {
		t.Fatal(err)
	}
	cases := map[string]BuffContainerState{
		"id beyond sequence": {Sequence: 1, Active: []BuffInstance{{Instance: 5, Spec: BuffSpec{ID: 1}, Stacks: 1}}},
		"duplicate id":       {Sequence: 3, Active: []BuffInstance{{Instance: 2, Spec: BuffSpec{ID: 1}, Stacks: 1}, {Instance: 2, Spec: BuffSpec{ID: 2}, Stacks: 1}}},
		"zero id":            {Sequence: 3, Active: []BuffInstance{{Instance: 0, Spec: BuffSpec{ID: 1}, Stacks: 1}}},
	}
	for name, state := range cases {
		if _, err := RestoreBuffContainer(state); !errors.Is(err, ErrBuffStateCorrupt) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
