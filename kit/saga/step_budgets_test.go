package saga

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

func giftDefinition() coresaga.Definition {
	// 与 `add saga` 生成的定义一致：步骤只写名字与 topic，预算全部来自配置。
	return coresaga.Definition{Type: "gift_item", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "gift_item.debit", CompensateTopic: "gift_item.debit.compensate"},
		{Name: "deliver", ForwardTopic: "gift_item.deliver", CompensateTopic: "gift_item.deliver.compensate"},
	}}
}

func yamlConfig(t *testing.T, text string) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// U-0280：步骤的超时与重试次数由配置给默认值，可按步骤覆盖；Mod 把它交给 Engine，Register 时补齐。
func TestStepBudgetsComeFromConfigWithPerStepOverrides(t *testing.T) {
	cfg := yamlConfig(t, `
saga:
  step_defaults:
    timeout: 4s
    max_attempts: 6
  steps:
    gift_item:
      debit:
        max_attempts: 15
`)
	mod := NewMod(giftDefinition())
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	resolved := mod.config.Engine.StepBudgets.Resolve(giftDefinition())
	debit, deliver := resolved.Steps[0], resolved.Steps[1]
	if debit.MaxAttempts != 15 || debit.Timeout != 4*time.Second || debit.BackoffMin != 100*time.Millisecond || debit.BackoffMax != 5*time.Second {
		t.Fatalf("debit budget=%+v, want the per-step override over the configured defaults", debit)
	}
	if deliver.MaxAttempts != 6 || deliver.Timeout != 4*time.Second {
		t.Fatalf("deliver budget=%+v, want the configured defaults", deliver)
	}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("resolved definition is invalid: %v", err)
	}

	// 定义里写死的值优先于配置默认值，但不优先于按步骤覆盖。
	explicit := giftDefinition()
	explicit.Steps[0].MaxAttempts, explicit.Steps[1].MaxAttempts = 3, 3
	resolved = mod.config.Engine.StepBudgets.Resolve(explicit)
	if resolved.Steps[0].MaxAttempts != 15 || resolved.Steps[1].MaxAttempts != 3 {
		t.Fatalf("explicit definition values resolved to %d / %d, want override 15 and definition 3", resolved.Steps[0].MaxAttempts, resolved.Steps[1].MaxAttempts)
	}

	// 没有任何配置时是框架默认值。
	empty := NewMod(giftDefinition())
	if err := empty.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	if got := empty.config.Engine.StepBudgets.Resolve(giftDefinition()).Steps[0]; got.MaxAttempts != 5 || got.Timeout != 5*time.Second {
		t.Fatalf("unconfigured budget=%+v, want the framework default 5 x 5s", got)
	}
}

func TestStepBudgetConfigRejectsTyposAndImpossibleValues(t *testing.T) {
	for name, text := range map[string]string{
		"unknown saga":      "saga:\n  steps:\n    gift:\n      debit:\n        max_attempts: 15\n",
		"unknown step":      "saga:\n  steps:\n    gift_item:\n      debitt:\n        max_attempts: 15\n",
		"unknown field":     "saga:\n  steps:\n    gift_item:\n      debit:\n        attempts: 15\n",
		"zero attempts":     "saga:\n  step_defaults:\n    max_attempts: 0\n",
		"too many attempts": "saga:\n  steps:\n    gift_item:\n      debit:\n        max_attempts: 1001\n",
		"negative timeout":  "saga:\n  step_defaults:\n    timeout: -1s\n",
		"inverted backoff":  "saga:\n  step_defaults:\n    backoff_min: 2s\n    backoff_max: 1s\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := NewMod(giftDefinition()).Init(yamlConfig(t, text)); err == nil {
				t.Fatalf("Init accepted %q", text)
			}
		})
	}
}

// RR-20261006-06（A12）：viper 把配置键折成小写，saga.steps 分不清只差大小写的类型或步骤名。之前已知表也按小写建，
// 后注册的定义静默覆盖先注册的，覆盖落到哪一个取决于定义顺序。现在只差大小写的名字直接报歧义错误
// （与 configdata 大小写敏感、不猜的方向一致），不论配置里有没有写到它。
func TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase(t *testing.T) {
	cfg := yamlConfig(t, `
saga:
  steps:
    gift_item:
      debit:
        max_attempts: 15
`)
	upperType := giftDefinition()
	upperType.Type = "Gift_Item"
	upperStep := giftDefinition()
	upperStep.Steps = append(upperStep.Steps, coresaga.Step{Name: "Debit", ForwardTopic: "gift_item.Debit", CompensateTopic: "gift_item.Debit.compensate"})
	for name, definitions := range map[string][]coresaga.Definition{
		"types":         {giftDefinition(), upperType},
		"types_swapped": {upperType, giftDefinition()},
		"steps":         {upperStep},
	} {
		budgets, err := StepBudgetsFromConfig(cfg, definitions...)
		if err == nil || !strings.Contains(err.Error(), "differ only in case") {
			t.Fatalf("%s: err=%v overrides=%v, want an ambiguity error", name, err, budgets.Overrides)
		}
	}
	// 同一个定义出现两次不是歧义。
	if _, err := StepBudgetsFromConfig(cfg, giftDefinition(), giftDefinition()); err != nil {
		t.Fatalf("the same definition twice: %v", err)
	}
}
