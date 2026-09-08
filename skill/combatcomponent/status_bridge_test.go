package combatcomponent

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-skill/skill"

	"github.com/tjbdwanghaibo/roost-skill/combat"
)

func newBridgeFixture() (*StatusBridge, *testRevision, *CombatComponent, *skill.Tick) {
	target := NewCombatComponent(NewCombatDao(2, "game"))
	target.dao.combatant = combat.Combatant{Alive: true, Health: 100, MaxHealth: 100}
	target.dao.attributes.SetBase(3, 100) // haste channel
	revision := &testRevision{}
	tick := skill.Tick(0)
	bridge := &StatusBridge{
		Resolver:  mapResolver{2: target},
		Revision:  revision,
		Committer: &combatRecordingCommitter{},
		Catalog: skill.GameplayCatalog{Statuses: skill.StatusCatalog{Entries: []skill.StatusCatalogEntry{
			{Handle: 10, Key: "burn", Category: "dot", DispelCategory: "magic", Dispellable: true, MaxStacks: 3,
				AttributeModifiers: []skill.StatusAttributeModifier{{Attribute: 3, Operation: "mul_bp", Value: 12000}}},
			{Handle: 11, Key: "stun", Category: "control", DispelCategory: "control", Dispellable: true,
				TenacityPolicy: "scale_duration", MaximumDurationTicks: 6, ImmunityTags: []skill.GameplayTagHandle{7}},
			{Handle: 12, Key: "fresh", RefreshPolicy: "replace", MaxStacks: 5},
		}}},
		CurrentTick: func() skill.Tick { return tick },
	}
	return bridge, revision, target, &tick
}

func TestStatusBridgeAppliesStacksAndModifiers(t *testing.T) {
	bridge, revision, target, _ := newBridgeFixture()
	result, handled, err := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 2}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	payload := result.Payload.(skill.StatusEffectResult)
	if !payload.Succeeded || !payload.Result.Applied || payload.Result.PreviousStacks != 0 || payload.Result.CurrentStacks != 2 || payload.Result.DueTick != 20 {
		t.Fatalf("payload = %+v", payload)
	}
	// Two stacks of ×1.2 aggregate additively: 100 * (1 + 0.2*2) = 140.
	if got := target.AttributeCurrent(3); got != 140 {
		t.Fatalf("modifier aggregation = %d, want 140", got)
	}
	if len(revision.events) != 1 || revision.events[0].Kind != "status_applied" {
		t.Fatalf("events = %+v", revision.events)
	}
	// Stacking beyond MaxStacks caps at 3.
	again, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 5}})
	if got := again.Payload.(skill.StatusEffectResult).Result.CurrentStacks; got != 3 {
		t.Fatalf("capped stacks = %d, want 3", got)
	}
}

func TestStatusBridgeImmunityTenacityAndReplace(t *testing.T) {
	bridge, revision, target, _ := newBridgeFixture()
	bridge.HasGameplayTag = func(entity skill.EntityID, tag skill.GameplayTagHandle) bool {
		return entity == 2 && tag == 7
	}
	// Immunity tag blocks the stun without touching the container.
	result, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 11, DurationTicks: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if payload := result.Payload.(skill.StatusEffectResult); !payload.Result.Immune || len(target.ActiveBuffs()) != 0 {
		t.Fatalf("immunity leaked: %+v buffs=%d", payload, len(target.ActiveBuffs()))
	}
	if revision.events[0].Kind != "status_immune" {
		t.Fatalf("events = %+v", revision.events)
	}
	// Without the tag: tenacity scales the duration, then the policy cap clamps.
	bridge.HasGameplayTag = nil
	target.Dao().buffs.SetTenacityBP(5000) // -50%: 10 -> 5, below the 6-tick cap
	applied, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 11, DurationTicks: 10}})
	if due := applied.Payload.(skill.StatusEffectResult).Result.DueTick; due != 5 {
		t.Fatalf("tenacity due = %d, want 5", due)
	}
	// replace policy: re-application discards the old instance entirely.
	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 12, DurationTicks: 10, Stacks: 4}})
	replaced, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 12, DurationTicks: 10, Stacks: 1}})
	if got := replaced.Payload.(skill.StatusEffectResult).Result.CurrentStacks; got != 1 {
		t.Fatalf("replace stacks = %d, want 1", got)
	}
	if errored, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 99, DurationTicks: 1}}); err == nil {
		t.Fatalf("unknown status accepted: %+v", errored)
	}
	if _, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 10, DurationTicks: 0}}); err == nil {
		t.Fatal("zero duration accepted")
	}
}

func TestStatusBridgeRemoveAndDispel(t *testing.T) {
	bridge, revision, target, _ := newBridgeFixture()
	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 2}})
	// Remove scoped to a different source owner is a no-op.
	miss, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.RemoveStatusCommand{SourceOwner: 9, Target: 2, Status: 10}})
	if payload := miss.Payload.(skill.StatusEffectResult); payload.Result.Removed || payload.Result.CurrentStacks != 2 {
		t.Fatalf("scoped remove hit: %+v", payload)
	}
	removed, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.RemoveStatusCommand{SourceOwner: 1, Target: 2, Status: 10}})
	if payload := removed.Payload.(skill.StatusEffectResult); !payload.Result.Removed || payload.Result.RemovedStacks != 2 || target.AttributeCurrent(3) != 100 {
		t.Fatalf("remove = %+v haste=%d", payload, target.AttributeCurrent(3))
	}
	// Dispel by category.
	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20}})
	dispelled, _, _ := bridge.Apply(skill.EffectCommand{Payload: skill.DispelStatusCommand{Target: 2, Category: "magic", Count: 0}})
	if payload := dispelled.Payload.(skill.StatusEffectResult); !payload.Result.Removed || payload.Result.CurrentStacks != 0 {
		t.Fatalf("dispel = %+v", payload)
	}
	kinds := []string{}
	for _, event := range revision.events {
		kinds = append(kinds, event.Kind)
	}
	want := []string{"status_applied", "status_removed", "status_applied", "status_dispelled"}
	for index, kind := range want {
		if kinds[index] != kind {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
}

func TestStatusBridgeAttributeModifierIsIndependent(t *testing.T) {
	bridge, _, target, tickPtr := newBridgeFixture()
	command := skill.AttributeModifierCommand{SourceOwner: 1, Target: 2, Attribute: 3, Operation: "mul_bp", Value: 12000, DurationTicks: 10}
	bridge.Apply(skill.EffectCommand{Payload: command})
	*tickPtr = 4
	bridge.Apply(skill.EffectCommand{Payload: command})
	// Two independent ×1.2 grants aggregate additively: 100 * 1.4 = 140.
	if got := target.AttributeCurrent(3); got != 140 {
		t.Fatalf("modifiers = %d, want 140", got)
	}
	// Each keeps its own expiry: the first lapses at 10, the second at 14.
	if expired := tickBuffsInTransaction(t, bridge, target, 10); len(expired) != 1 || target.AttributeCurrent(3) != 120 {
		t.Fatalf("first expiry: expired=%d haste=%d", len(expired), target.AttributeCurrent(3))
	}
	if expired := tickBuffsInTransaction(t, bridge, target, 14); len(expired) != 1 || target.AttributeCurrent(3) != 100 {
		t.Fatalf("second expiry: expired=%d haste=%d", len(expired), target.AttributeCurrent(3))
	}
	if _, _, err := bridge.Apply(skill.EffectCommand{Payload: skill.AttributeModifierCommand{Target: 2, Attribute: 3, Operation: "pow", Value: 2, DurationTicks: 1}}); err == nil {
		t.Fatal("unsupported operation accepted")
	}
}

func TestHostAdapterResourceCommand(t *testing.T) {
	adapter, revision, _, defender := newAdapterFixture()
	spend, handled, err := adapter.Apply(skill.EffectCommand{Payload: skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: 20}})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if value, _ := spend.Value.Int(); value != 30 || defender.AttributeBase(5) != 30 {
		t.Fatalf("spend result = %d base=%d", value, defender.AttributeBase(5))
	}
	if _, _, err := adapter.Apply(skill.EffectCommand{Payload: skill.ResourceCommand{Target: 2, Resource: 5, Operation: "spend", Amount: 99}}); !errors.Is(err, skill.ErrInsufficientResource) {
		t.Fatalf("overspend accepted: %v", err)
	}
	// No-op change commits nothing.
	noop, _, _ := adapter.Apply(skill.EffectCommand{Payload: skill.ResourceCommand{Target: 2, Resource: 5, Operation: "add", Amount: 0}})
	if noop.Commit.Changed || revision.revision != 1 {
		t.Fatalf("no-op bumped revision: %+v revision=%d", noop.Commit, revision.revision)
	}
	if len(revision.events) != 1 || revision.events[0].Kind != "resource_changed" {
		t.Fatalf("events = %+v", revision.events)
	}
}

func statusInstanceCommand(owner, target skill.EntityID, id combat.BuffInstanceID, operation string, value int64) skill.ModifyStatusInstanceCommand {
	return skill.ModifyStatusInstanceCommand{
		Owner: owner, Status: skill.StatusInstanceRef{ID: skill.NewStatusInstanceID(uint64(id)), Target: target},
		Operation: operation, Value: value,
	}
}

func TestStatusBridgeModifyInstanceAuthorizationAndStacks(t *testing.T) {
	bridge, _, target, _ := newBridgeFixture()
	// Catalog entry 10 (burn): SourceOwnership "" => source-owned; dispellable.
	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 2}})
	instanceID := target.ActiveBuffs()[0].Instance

	// A stranger may not edit a source-owned instance.
	denied, _, err := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(5, 2, instanceID, "add_stacks", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if payload := denied.Payload.(skill.StatusEffectResult); payload.Succeeded || payload.FailureReason != skill.ExpectedFailurePermissionDenied {
		t.Fatalf("stranger edit accepted: %+v", payload)
	}
	// The source stacks it up to the cap (MaxStacks 3).
	stacked, _, _ := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(1, 2, instanceID, "add_stacks", 5)})
	if payload := stacked.Payload.(skill.StatusEffectResult); payload.Result.CurrentStacks != 3 || payload.Result.PreviousStacks != 2 {
		t.Fatalf("add_stacks = %+v", payload.Result)
	}
	if got := target.AttributeCurrent(3); got != 160 { // 100 * (1 + 0.2*3)
		t.Fatalf("modifier rescale = %d, want 160", got)
	}
	// The target itself may remove a dispellable instance.
	removed, _, _ := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(2, 2, instanceID, "remove", 0)})
	if payload := removed.Payload.(skill.StatusEffectResult); !payload.Result.Removed || target.AttributeCurrent(3) != 100 {
		t.Fatalf("self remove = %+v haste=%d", payload.Result, target.AttributeCurrent(3))
	}
	// Vanished instances answer reference-expired.
	expired, _, _ := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(1, 2, instanceID, "add_stacks", 1)})
	if payload := expired.Payload.(skill.StatusEffectResult); payload.Succeeded || payload.FailureReason != skill.ExpectedFailureReferenceExpired {
		t.Fatalf("expired ref = %+v", payload)
	}
}

func TestStatusBridgeModifyInstanceDurationsAndTransfer(t *testing.T) {
	bridge, _, target, tickPtr := newBridgeFixture()
	// Make burn transferable+copyable with a duration whitelist and a cap.
	entries := bridge.Catalog.Statuses.Entries
	entries[0].Copyable, entries[0].Transferable, entries[0].Stealable = true, true, true
	entries[0].DurationOperations = []string{"add_duration", "refresh"}
	entries[0].MaximumDurationTicks = 30
	other := NewCombatComponent(NewCombatDao(3, "game"))
	other.dao.combatant = combat.Combatant{Alive: true, Health: 50, MaxHealth: 50}
	other.dao.attributes.SetBase(3, 100)
	bridge.Resolver = mapResolver{2: target, 3: other}

	bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20}})
	instanceID := target.ActiveBuffs()[0].Instance
	*tickPtr = 5

	// add_duration: remaining 15 + 100 clamps at the 30-tick policy cap.
	extended, _, _ := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(1, 2, instanceID, "add_duration", 100)})
	if payload := extended.Payload.(skill.StatusEffectResult); payload.Result.DueTick != 35 || payload.Result.PreviousDueTick != 20 {
		t.Fatalf("add_duration = %+v", payload.Result)
	}
	// set_duration is not whitelisted.
	blocked, _, _ := bridge.Apply(skill.EffectCommand{Payload: statusInstanceCommand(1, 2, instanceID, "set_duration", 5)})
	if payload := blocked.Payload.(skill.StatusEffectResult); payload.Succeeded || payload.FailureReason != skill.ExpectedFailurePolicyRejected {
		t.Fatalf("whitelist bypassed: %+v", payload)
	}
	// Steal: a stranger transfers it because the policy is stealable.
	command := statusInstanceCommand(9, 2, instanceID, "transfer_to", 0)
	command.Target = 3
	command.OwnershipPolicy = "new_source"
	stolen, _, err := bridge.Apply(skill.EffectCommand{Payload: command})
	if err != nil {
		t.Fatal(err)
	}
	payload := stolen.Payload.(skill.StatusEffectResult)
	if !payload.Result.Removed || payload.Result.Created.Target != 3 || payload.Result.Created.ID.OpaqueID() == 0 {
		t.Fatalf("transfer = %+v", payload.Result)
	}
	if len(target.ActiveBuffs()) != 0 || target.AttributeCurrent(3) != 100 {
		t.Fatalf("source kept the buff: buffs=%d haste=%d", len(target.ActiveBuffs()), target.AttributeCurrent(3))
	}
	adopted := other.ActiveBuffs()
	if len(adopted) != 1 || adopted[0].Source != 9 || other.AttributeCurrent(3) != 120 {
		t.Fatalf("destination state: %+v haste=%d", adopted, other.AttributeCurrent(3))
	}
}

func tickBuffsInTransaction(t *testing.T, bridge *StatusBridge, target *CombatComponent, tick int64) []combat.BuffInstance {
	t.Helper()
	value, err := nest.RunDetachedTransaction(context.Background(), bridge.Committer, "combat_test_tick", func() (any, error) {
		return target.TickBuffs(tick), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return value.([]combat.BuffInstance)
}
