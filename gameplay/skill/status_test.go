package skill

import "testing"

func TestStatusRefreshStacksAndExpiresAtLogicalTick(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	apply := func(duration Tick) {
		_, err := host.Apply(EffectCommand{Payload: StatusCommand{SourceOwner: 1, Target: 1, Status: 1, DurationTicks: duration, Stacks: 2}})
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(10)
	if got := host.StatusStacksForTest(1, 1); got != 1 {
		t.Fatalf("stacks = %d", got)
	}
	if _, err := host.Advance(5); err != nil {
		t.Fatal(err)
	}
	apply(10)
	if _, err := host.Advance(10); err != nil || host.StatusStacksForTest(1, 1) != 1 {
		t.Fatalf("status expired before refreshed due tick: %v", err)
	}
	if _, err := host.Advance(15); err != nil || host.StatusStacksForTest(1, 1) != 0 {
		t.Fatalf("status did not expire at due tick: %v", err)
	}
}

func TestAttributeModifierUsesAddThenMultiplyOrdering(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Attributes: map[AttributeHandle]int64{2: 100}})
	commands := []AttributeModifierCommand{
		{SourceOwner: 1, Target: 1, Attribute: 2, Operation: "mul_bp", Value: 5000, DurationTicks: 10},
		{SourceOwner: 1, Target: 1, Attribute: 2, Operation: "add", Value: 20, DurationTicks: 10},
	}
	for _, command := range commands {
		if _, err := host.Apply(EffectCommand{Payload: command}); err != nil {
			t.Fatal(err)
		}
	}
	if got := host.EffectiveAttributeForTest(1, 2); got != 60 {
		t.Fatalf("effective attribute = %d, want 60", got)
	}
	if _, err := host.Advance(10); err != nil || host.EffectiveAttributeForTest(1, 2) != 100 {
		t.Fatalf("modifier did not expire: %v", err)
	}
}

func TestAttributeModifierUsesCatalogRounding(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Attributes: map[AttributeHandle]int64{2: 101}})
	if _, err := host.Apply(EffectCommand{Payload: AttributeModifierCommand{SourceOwner: 1, Target: 1, Attribute: 2, Operation: "mul_bp", Value: 5000, DurationTicks: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := host.EffectiveAttributeForTest(1, 2); got != 51 {
		t.Fatalf("half-away rounding = %d, want 51", got)
	}
}

func TestStatusHonorsImmunityTenacitySourceRemovalAndDispel(t *testing.T) {
	catalog := defaultGameplayCatalog()
	catalog.Statuses.Entries[0].ImmunityTags = []GameplayTagHandle{1}
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(catalog)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, TenacityBP: 5000, GameplayTags: map[GameplayTagHandle]bool{1: true}})
	immune, err := host.Apply(EffectCommand{Payload: StatusCommand{SourceOwner: 2, Target: 1, Status: 1, DurationTicks: 10, Stacks: 1}})
	if err != nil || !immune.Payload.(StatusEffectResult).Result.Immune {
		t.Fatalf("status immunity = %#v, %v", immune, err)
	}

	entity := MemoryEntity{ID: 1, Alive: true, TenacityBP: 5000}
	host.UpsertEntity(entity)
	for _, owner := range []EntityID{2, 3} {
		if _, err := host.Apply(EffectCommand{Payload: StatusCommand{SourceOwner: owner, Target: 1, Status: 1, DurationTicks: 10, Stacks: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := host.StatusStacksForTest(1, 1); got != 2 {
		t.Fatalf("source-owned instances = %d", got)
	}
	if _, err := host.Apply(EffectCommand{Payload: RemoveStatusCommand{SourceOwner: 2, Target: 1, Status: 1}}); err != nil || host.StatusStacksForTest(1, 1) != 1 {
		t.Fatalf("source-owned removal failed: %v", err)
	}
	if _, err := host.Apply(EffectCommand{Payload: DispelStatusCommand{Target: 1, Category: "debuff", Count: 1}}); err != nil || host.StatusStacksForTest(1, 1) != 0 {
		t.Fatalf("dispel failed: %v", err)
	}

	if _, err := host.Apply(EffectCommand{Payload: StatusCommand{SourceOwner: 2, Target: 1, Status: 1, DurationTicks: 10, Stacks: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Advance(4); err != nil || host.StatusStacksForTest(1, 1) != 1 {
		t.Fatalf("tenacity expired too early: %v", err)
	}
	if _, err := host.Advance(5); err != nil || host.StatusStacksForTest(1, 1) != 0 {
		t.Fatalf("tenacity did not scale duration to tick 5: %v", err)
	}
}
