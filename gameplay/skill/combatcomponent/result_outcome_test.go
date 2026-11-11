package combatcomponent

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

// Runtime 将空字符串视为缺失的结果，成功（包括免疫/无变化）必须显式携带 none。
func TestAdapterSuccessOutcomesSatisfyRuntimeContract(t *testing.T) {
	adapter, _, _, _ := newAdapterFixture()
	for _, command := range []skill.EffectCommand{
		{Payload: skill.DamageCommand{Source: 1, Target: 2, Amount: 10}},
		{Payload: skill.HealCommand{Target: 2, Amount: 100}},
		{Payload: skill.HealCommand{Target: 2, Amount: 0}},
		{Payload: skill.ShieldCommand{Target: 2, Amount: 10}},
		{Payload: skill.ShieldCommand{Target: 2, Amount: 0}},
	} {
		result, handled, err := adapter.Apply(command)
		if err != nil || !handled {
			t.Fatalf("%T handled=%v: %v", command.Payload, handled, err)
		}
		assertSuccessfulOutcome(t, result.Payload)
	}
	bridge, _, _, _ := newBridgeFixture()
	for _, command := range []skill.EffectCommand{
		{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20, Stacks: 2}},
		{Payload: skill.RemoveStatusCommand{SourceOwner: 1, Target: 2, Status: 10}},
		{Payload: skill.RemoveStatusCommand{SourceOwner: 1, Target: 2, Status: 10}},
		{Payload: skill.StatusCommand{SourceOwner: 1, Target: 2, Status: 10, DurationTicks: 20}},
		{Payload: skill.DispelStatusCommand{Target: 2, Category: "magic"}},
		{Payload: skill.DispelStatusCommand{Target: 2, Category: "magic"}},
		{Payload: skill.AttributeModifierCommand{SourceOwner: 1, Target: 2, Attribute: 3, Operation: "add", Value: 2, DurationTicks: 10}},
	} {
		result, handled, err := bridge.Apply(command)
		if err != nil || !handled {
			t.Fatalf("%T handled=%v: %v", command.Payload, handled, err)
		}
		assertSuccessfulOutcome(t, result.Payload)
	}
	bridge.HasGameplayTag = func(skill.EntityID, skill.GameplayTagHandle) bool { return true }
	result, handled, err := bridge.Apply(skill.EffectCommand{Payload: skill.StatusCommand{Target: 2, Status: 11, DurationTicks: 10}})
	if err != nil || !handled {
		t.Fatalf("immune status: %v", err)
	}
	assertSuccessfulOutcome(t, result.Payload)
}

func assertSuccessfulOutcome(t *testing.T, payload skill.EffectResultPayload) {
	t.Helper()
	var outcome skill.ResultOutcome
	switch p := payload.(type) {
	case skill.DamageEffectResult:
		outcome = p.ResultOutcome
	case skill.HealEffectResult:
		outcome = p.ResultOutcome
	case skill.ShieldEffectResult:
		outcome = p.ResultOutcome
	case skill.StatusEffectResult:
		outcome = p.ResultOutcome
	case skill.AttributeModifierEffectResult:
		outcome = p.ResultOutcome
	default:
		t.Fatalf("unexpected effect payload %T", payload)
	}
	if !outcome.Succeeded || outcome.FailureReason != skill.ExpectedFailureNone {
		t.Errorf("%T inconsistent outcome: %+v", payload, outcome)
	}
}
