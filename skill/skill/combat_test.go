package skill

import "testing"

func TestCombatPipelineResolvesMitigationElementCriticalShieldAndDeath(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, ForceCritical: true, Penetration: 50})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 20, MaxHealth: 20, Shield: 3, Armor: 100, ElementMultipliers: map[ElementHandle]int64{2: 20000}})
	result, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Owner: 1, Target: 2, Amount: 10, DamageType: 1, Element: 2, CanCritical: true, Event: newRootEvent(10)}})
	if err != nil {
		t.Fatal(err)
	}
	damage := mustDamageResult(t, result)
	if damage.Mitigated != 6 || !damage.Critical || damage.Absorbed != 3 || damage.HealthDamage != 15 || damage.Killed {
		t.Fatalf("unexpected damage pipeline result: %#v", damage)
	}
	if got := host.HealthForTest(2); got != 5 {
		t.Fatalf("health = %d", got)
	}
	eventsAfterHit := host.Events(0)
	hitEvent := eventsAfterHit[len(eventsAfterHit)-1]
	if hitEvent.Revision != result.Commit.Revision || hitEvent.Context.WorldRevision != result.Commit.Revision {
		t.Fatalf("damage commit/event revision mismatch: %#v %#v", result.Commit, hitEvent)
	}
	criticalTags := hitEvent.Context.GameplayTags()
	if len(criticalTags) != 1 || criticalTags[0] != 3 {
		t.Fatalf("critical runtime tag missing: %v", criticalTags)
	}

	kill, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Owner: 1, Target: 2, Amount: 10, DamageType: 3, Element: 1, Event: newRootEvent(20)}})
	if err != nil || !mustDamageResult(t, kill).Killed {
		t.Fatalf("kill = %#v, %v", kill, err)
	}
	killedEvents := 0
	for _, event := range host.Events(0) {
		if event.Context.Result == "killed" {
			killedEvents++
		}
	}
	if killedEvents != 1 {
		t.Fatalf("killed events = %d", killedEvents)
	}
}

func TestDamageImmuneIsSuccessfulAndTagsDoNotAlias(t *testing.T) {
	host := newCombatTestHost()
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, DamageImmune: true})
	tags := []GameplayTagHandle{2, 1}
	result, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Owner: 1, Target: 2, Amount: 10, DamageType: 1, Element: 1, Tags: tags, Event: newRootEvent(1)}})
	if err != nil || !mustDamageResult(t, result).Immune {
		t.Fatalf("immune result = %#v, %v", result, err)
	}
	tags[0] = 99
	events := host.Events(0)
	if got := events[len(events)-1].Context.GameplayTags(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("event tags aliased command: %v", got)
	}
	if host.HealthForTest(2) != 100 {
		t.Fatal("immune damage changed health")
	}
	invalid, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Target: 999, Amount: 1, DamageType: 1, Element: 1}})
	payload, ok := invalid.Payload.(DamageEffectResult)
	if err != nil || !ok || payload.Succeeded || payload.FailureReason != ExpectedFailureInvalidTarget {
		t.Fatalf("invalid target = %#v, %v", invalid, err)
	}
}

func newCombatTestHost() *MemoryHost {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.ConfigureGameplayCatalog(defaultGameplayCatalog())
	return host
}

func mustDamageResult(t *testing.T, result EffectResult) DamageResult {
	t.Helper()
	payload, ok := result.Payload.(DamageEffectResult)
	if !ok {
		t.Fatalf("payload = %T", result.Payload)
	}
	return payload.Result
}
