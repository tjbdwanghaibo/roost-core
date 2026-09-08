package combatcomponent

import (
	"errors"
	"math"
	"testing"

	"github.com/tjbdwanghaibo/roost-skill/skill"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

type mapResolver map[skill.EntityID]*CombatComponent

func (m mapResolver) CombatComponent(id skill.EntityID) (*CombatComponent, bool) {
	component, ok := m[id]
	return component, ok
}

type testRevision struct {
	revision skill.WorldRevision
	events   []EffectEvent
}

func (r *testRevision) CurrentRevision() skill.WorldRevision { return r.revision }
func (r *testRevision) CommitEffect(events []EffectEvent) skill.CommitReceipt {
	r.revision++
	r.events = append(r.events, events...)
	return skill.CommitReceipt{Revision: r.revision}
}

func newAdapterFixture() (*HostAdapter, *testRevision, *CombatComponent, *CombatComponent) {
	attacker := NewCombatComponent(NewCombatDao(1, "game"))
	attacker.dao.combatant = combat.Combatant{Alive: true, Health: 80, MaxHealth: 80, Penetration: 10}
	defender := NewCombatComponent(NewCombatDao(2, "game"))
	defender.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100, Shield: 20, Armor: 40}
	defender.dao.attributes.SetBase(5, 50) // mana pool
	revision := &testRevision{}
	adapter := &HostAdapter{
		Resolver:  mapResolver{1: attacker, 2: defender},
		Revision:  revision,
		Committer: &combatRecordingCommitter{},
		ResourceAttribute: func(resource string, handle skill.ResourceHandle) (combat.AttributeID, bool) {
			if resource == "mana" || handle == 5 {
				return 5, true
			}
			return 0, false
		},
	}
	return adapter, revision, attacker, defender
}

func TestHostAdapterAppliesDamageWithEvents(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	result, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 100, DamageType: 1, CanCritical: true}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	payload := result.Payload.(skill.DamageEffectResult)
	if !payload.Succeeded || payload.Result.HealthDamage == 0 || payload.Result.Absorbed != 20 {
		t.Fatalf("payload = %+v", payload)
	}
	if defender.Combatant().Shield != 0 {
		t.Fatal("shield not consumed")
	}
	kinds := make([]string, 0, len(revision.events))
	for _, event := range revision.events {
		kinds = append(kinds, event.Kind)
	}
	if len(kinds) != 3 || kinds[0] != "damage_resolved" || kinds[1] != "shield_absorbed" || kinds[2] != "shield_broken" {
		t.Fatalf("events = %v", kinds)
	}
	if result.Commit.Revision != 1 || !result.Commit.Changed {
		t.Fatalf("receipt = %+v", result.Commit)
	}
	// Unknown target fails without a revision bump.
	miss, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.DamageCommand{Source: 1, Target: 99, Amount: 10}})
	if err != nil || !handled || miss.Payload.(skill.DamageEffectResult).Succeeded || revision.revision != 1 {
		t.Fatalf("missing target: %+v revision=%d err=%v", miss.Payload, revision.revision, err)
	}
	// Non-combat payloads pass through unhandled.
	if _, handled, _ := adapter.Apply(skill.EffectCommand{Payload: skill.TeleportCommand{Target: 2}}); handled {
		t.Fatal("teleport handled by combat adapter")
	}
}

func TestHostAdapterHealShieldAndReads(t *testing.T) {
	adapter, revision, attacker, _ := newAdapterFixture()
	attacker.dao.combatant.Health -= 30
	healResult, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.HealCommand{Target: 1, Amount: 100}})
	if err != nil || !handled || healResult.Payload.(skill.HealEffectResult).Result.Effective != 30 {
		t.Fatalf("heal = %+v err=%v", healResult.Payload, err)
	}
	shieldResult, _, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 15, DurationTicks: 10}})
	if err != nil || shieldResult.Payload.(skill.ShieldEffectResult).Result.Added != 15 {
		t.Fatalf("shield = %+v err=%v", shieldResult.Payload, err)
	}
	if got := len(revision.events); got != 2 {
		t.Fatalf("events = %d", got)
	}
	read, handled, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
	if err != nil || !handled {
		t.Fatalf("read err=%v handled=%v", err, handled)
	}
	if value, ok := read.Value.Int(); !ok || value != 50 {
		t.Fatalf("mana read = %+v", read.Value)
	}
	if _, handled, _ := adapter.Read(skill.ReadRequest{Payload: skill.PositionRead{Entity: 2}}); handled {
		t.Fatal("position read handled by combat adapter")
	}
}

func TestHostAdapterShieldReportsEffectiveDeltaAndSkipsNoOpCommit(t *testing.T) {
	adapter, revision, attacker, _ := newAdapterFixture()
	attacker.dao.combatant.Shield = math.MaxInt64 - 5

	result, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 10}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	payload := result.Payload.(skill.ShieldEffectResult)
	if payload.Result.Added != 5 || attacker.Combatant().Shield != math.MaxInt64 {
		t.Fatalf("shield result=%+v state=%d", payload.Result, attacker.Combatant().Shield)
	}
	if !result.Commit.Changed || result.Commit.Revision != 1 || len(revision.events) != 1 {
		t.Fatalf("first receipt=%+v events=%d", result.Commit, len(revision.events))
	}

	noOp, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.ShieldCommand{Target: 1, Amount: 10}})
	if err != nil || !handled {
		t.Fatalf("no-op handled=%v err=%v", handled, err)
	}
	if noOp.Payload.(skill.ShieldEffectResult).Result.Added != 0 || noOp.Commit.Changed {
		t.Fatalf("no-op result=%+v receipt=%+v", noOp.Payload, noOp.Commit)
	}
	if revision.revision != 1 || len(revision.events) != 1 {
		t.Fatalf("no-op advanced authority: revision=%d events=%d", revision.revision, len(revision.events))
	}
}

func TestHostAdapterPayCostsIsAtomic(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	payment := skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 30}, {Resource: "mana", Amount: 15}}}
	receipt, err := adapter.PayCosts(payment)
	if err != nil || !receipt.Changed || defender.AttributeBase(5) != 5 {
		t.Fatalf("pay = %+v base=%d err=%v", receipt, defender.AttributeBase(5), err)
	}
	// Insufficient totals must not partially deduct.
	if _, err := adapter.PayCosts(payment); !errors.Is(err, skill.ErrInsufficientResource) {
		t.Fatalf("expected insufficient, got %v", err)
	}
	if defender.AttributeBase(5) != 5 {
		t.Fatalf("partial deduction: base=%d", defender.AttributeBase(5))
	}
	if _, err := adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "unknown", Amount: 1}}}); err == nil {
		t.Fatal("unmapped resource accepted")
	}
	if revision.revision != 1 {
		t.Fatalf("revision = %d, want 1", revision.revision)
	}
}
