package saga

import (
	"testing"

	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

// RR-20261005-NC-194：saga.steps 下按步骤的覆盖对大小写混写的类型 / 步骤名同样生效。viper 把配置键
// 一律转成小写；Mod 没拿到定义（kitsaga.NewMod() 无参数，定义之后经 Engine.Register 登记）时，覆盖
// 以小写键保存，Engine.Register 的 StepBudgets.Resolve 按原样的 {Type, Step} 精确查找。
// 旧行为：`saga.steps.GiftItem.Debit.max_attempts: 15` 对 Type "GiftItem" / Step "Debit" 不生效，
// 步骤按默认 5 次尝试运行，没有任何报错（拿到定义时 StepBudgetsFromConfig 会把键还原成定义里的写法，不受影响）。
func TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions(t *testing.T) {
	cfg := yamlConfig(t, "saga:\n  steps:\n    GiftItem:\n      Debit:\n        max_attempts: 15\n")
	definition := coresaga.Definition{Type: "GiftItem", Version: 1, Steps: []coresaga.Step{{Name: "Debit", ForwardTopic: "gift.debit"}}}
	for _, withDefinitions := range []bool{false, true} {
		var definitions []coresaga.Definition
		if withDefinitions {
			definitions = append(definitions, definition)
		}
		budgets, err := StepBudgetsFromConfig(cfg, definitions...)
		if err != nil {
			t.Fatalf("withDefinitions=%v: %v", withDefinitions, err)
		}
		// Engine.Register 用的就是这一步。
		resolved := budgets.Resolve(definition)
		if got := resolved.Steps[0].MaxAttempts; got != 15 {
			t.Fatalf("withDefinitions=%v: GiftItem/Debit max_attempts = %d, want the configured 15 (overrides %v)", withDefinitions, got, budgets.Overrides)
		}
	}
}

// 原样写法的覆盖优先于小写回退（代码里同时给了两份覆盖时）。
func TestExactOverrideWinsOverTheLowercaseFallback(t *testing.T) {
	budgets := coresaga.StepBudgets{Overrides: map[coresaga.StepKey]coresaga.StepBudget{
		{Type: "GiftItem", Step: "Debit"}: {MaxAttempts: 7},
		{Type: "giftitem", Step: "debit"}: {MaxAttempts: 15},
	}}
	definition := coresaga.Definition{Type: "GiftItem", Version: 1, Steps: []coresaga.Step{{Name: "Debit", ForwardTopic: "gift.debit"}}}
	if got := budgets.Resolve(definition).Steps[0].MaxAttempts; got != 7 {
		t.Fatalf("max_attempts = %d, want the exact override 7", got)
	}
}
