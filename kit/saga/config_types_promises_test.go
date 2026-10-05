package saga

import (
	"strings"
	"testing"
)

// RR-20261005-NC-190：步骤预算的时长不带单位时拒绝。旧行为：`timeout: 5` 读成 5ns，正数，
// 校验通过，每次尝试几乎立刻超时。
func TestStepBudgetDurationsRefuseValuesWithoutAUnit(t *testing.T) {
	for _, text := range []string{"5", "\"5\"", "abc"} {
		cfg := yamlConfig(t, "saga:\n  step_defaults:\n    timeout: "+text+"\n")
		if budgets, err := StepBudgetsFromConfig(cfg, giftDefinition()); err == nil || !strings.Contains(err.Error(), "saga.step_defaults.timeout") {
			t.Fatalf("timeout: %s -> %+v, %v; want an error naming saga.step_defaults.timeout", text, budgets.Defaults, err)
		}
	}
	cfg := yamlConfig(t, "saga:\n  steps:\n    gift_item:\n      debit:\n        backoff_max: 30\n")
	if _, err := StepBudgetsFromConfig(cfg, giftDefinition()); err == nil || !strings.Contains(err.Error(), "saga.steps.gift_item.debit.backoff_max") {
		t.Fatalf("per-step backoff_max: 30 -> %v; want an error naming the key", err)
	}
}
