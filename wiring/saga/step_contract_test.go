package saga

import (
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	"strings"
	"testing"
	"time"
)

func TestStepTimeoutMustBeShorterThanTheStepConsumersAckWait(t *testing.T) {
	ack := coresaga.DefaultStepAckWait
	for name, tc := range map[string]struct {
		yaml       string
		definition coresaga.Definition
		want       string
	}{
		"step_defaults.timeout equal to ack wait": {yaml: "saga:\n  step_defaults:\n    timeout: " + ack.String() + "\n", definition: giftDefinition(), want: "saga.step_defaults.timeout"},
		"per-step override longer than ack wait":  {yaml: "saga:\n  steps:\n    gift_item:\n      debit:\n        timeout: 45s\n", definition: giftDefinition(), want: "saga.steps.gift_item.debit.timeout"},
		"timeout written in the definition": {yaml: "saga: {}\n", definition: func() coresaga.Definition {
			d := giftDefinition()
			d.Steps[1].Timeout = 45 * time.Second
			return d
		}(), want: `saga "gift_item" step "deliver"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := yamlConfig(t, tc.yaml)
			err := NewMod(tc.definition).Init(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Init = %v, want a refusal naming %s: a step that may run %s or longer outlives the step consumer's ack wait (%s) and is redelivered while it still runs", err, tc.want, ack, ack)
			}
		})
	}
	// 配置部分在任何 Mod Init 之前就被 CheckConfig 拒绝（A4 ① 的跨键规则）。
	if err := app.CheckConfig(yamlConfig(t, "server_type: game\nsid: 1\nsaga:\n  step_defaults:\n    timeout: 45s\n"), NewMod(giftDefinition())); err == nil || !strings.Contains(err.Error(), "saga.step_defaults.timeout") {
		t.Fatalf("CheckConfig = %v, want the step timeout refused before Init", err)
	}
	// 守卫：短于 AckWait 照常启动。
	if err := NewMod(giftDefinition()).Init(yamlConfig(t, "saga:\n  step_defaults:\n    timeout: "+(ack-time.Second).String()+"\n")); err != nil {
		t.Fatalf("a step timeout shorter than the ack wait was refused: %v", err)
	}
}

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
