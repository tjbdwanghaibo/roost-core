package combatcomponent

import (
	"context"
	"math"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

func TestNoChangeCombatDoesNotMarkDirty(t *testing.T) {
	for _, action := range []string{"ignored_buff", "dodged", "zero_damage"} {
		t.Run(action, func(t *testing.T) {
			_, _, _, target := newAdapterFixture()
			spec := combat.BuffSpec{ID: 1, StackPolicy: combat.BuffIgnore}
			if action == "ignored_buff" {
				target.dao.buffs.Apply(spec, 0, 1)
			}
			if action == "dodged" {
				target.dao.combatant.Dodge = true
			}
			target.dao.CleanDirty()
			_, err := nest.RunDetachedTransaction(context.Background(), &combatRecordingCommitter{}, "combat_no_change", func() (any, error) {
				if action == "ignored_buff" {
					target.ApplyBuff(spec, 0, 1)
				} else {
					amount := int64(5)
					if action == "zero_damage" {
						amount = 0
					}
					target.ApplyDamage(nil, combat.DamageInput{Amount: amount}, nil)
				}
				if target.dao.tracker.HasSyncDirty() {
					t.Errorf("%s marked unchanged DAO dirty", action)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAbsentResourceMappingReturnsErrors(t *testing.T) {
	for _, action := range []string{"read", "pay", "apply"} {
		t.Run(action, func(t *testing.T) {
			defer func() {
				if value := recover(); value != nil {
					t.Fatalf("nil mapping panicked: %v", value)
				}
			}()
			adapter, _, _, _ := newAdapterFixture()
			adapter.ResourceAttribute = nil
			var err error
			switch action {
			case "read":
				_, _, err = adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
			case "pay":
				_, err = adapter.PayCosts(skill.CostPayment{Entity: 2, Entries: []skill.CostEntry{{Resource: "mana", Amount: 1}}})
			case "apply":
				_, _, err = adapter.Apply(skill.EffectCommand{Payload: skill.ResourceCommand{Target: 2, Resource: 5, Operation: "add", Amount: 1}})
			}
			if err == nil {
				t.Fatal("missing mapping accepted")
			}
		})
	}
}

func TestResourceReadMatchesSpendableBase(t *testing.T) {
	adapter, _, _, target := newAdapterFixture()
	target.dao.attributes.Grant(1, combat.Modifier{Attribute: 5, Flat: 30})
	read, _, err := adapter.Read(skill.ReadRequest{Payload: skill.ResourceRead{Entity: 2, Resource: "mana"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read.Value.Int(); got != 50 {
		t.Fatalf("reported spendable pool=%d want=50", got)
	}
}

func TestHostAdapterRejectsUnsupportedCombatCatalog(t *testing.T) {
	for _, kind := range []string{"formula", "damage", "element"} {
		t.Run(kind, func(t *testing.T) {
			adapter, _, _, target := newAdapterFixture()
			command := skill.DamageCommand{Source: 1, Target: 2, Amount: 5, DamageType: 1}
			switch kind {
			case "formula":
				adapter.Catalog.Combat.FormulaPolicy = "unknown"
			case "damage":
				adapter.Catalog.Combat.DamageTypes = []skill.DamageTypeHandle{2}
			case "element":
				adapter.Catalog.Elements.Entries = []skill.ElementCatalogEntry{{Handle: 1, Key: "fire"}}
			}
			_, _, err := adapter.Apply(skill.EffectCommand{Payload: command})
			if err == nil || target.Combatant().Shield != 20 {
				t.Fatalf("unsupported %s accepted: %v", kind, err)
			}
		})
	}
}

func TestStatusStackOverflowAndPermanentRefresh(t *testing.T) {
	for _, action := range []string{"add_stacks", "refresh"} {
		t.Run(action, func(t *testing.T) {
			bridge, _, target, _ := newBridgeFixture()
			id, _ := target.dao.buffs.Apply(combat.BuffSpec{ID: 10, MaxStacks: 3, DurationTicks: 0}, 0, 1)
			_, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.ModifyStatusInstanceCommand{Owner: 1, Status: skill.StatusInstanceRef{ID: skill.NewStatusInstanceID(uint64(id)), Target: 2}, Operation: action, Value: math.MaxInt64}})
			if err != nil {
				t.Fatal(err)
			}
			active := target.ActiveBuffs()
			if len(active) != 1 {
				t.Fatalf("overflow removed buff: %v", active)
			}
			if action == "add_stacks" && active[0].Stacks != 3 {
				t.Fatalf("stacks=%d", active[0].Stacks)
			}
			if action == "refresh" && active[0].DueTick != 0 {
				t.Fatalf("permanent expires at %d", active[0].DueTick)
			}
		})
	}
}
