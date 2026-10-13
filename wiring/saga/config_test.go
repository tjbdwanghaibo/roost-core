package saga

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

func TestSagaDeclaredDefaultsMatchCoreDefaults(t *testing.T) {
	mod := NewMod()
	if err := mod.Init(viper.New()); err != nil {
		t.Fatal(err)
	}
	got, want := mod.config.Engine, coresaga.DefaultOptions()
	if got.CoordinatorWorkers != want.CoordinatorWorkers || got.PublisherWorkers != want.PublisherWorkers ||
		got.CoordinatorBatch != want.CoordinatorBatch || got.PublisherBatch != want.PublisherBatch ||
		got.LeaseDuration != want.LeaseDuration || got.StoreTimeout != want.StoreTimeout || got.PollInterval != want.PollInterval ||
		got.PublishTimeout != want.PublishTimeout || got.PublishBackoffMin != want.PublishBackoffMin ||
		got.PublishBackoffMax != want.PublishBackoffMax || got.MaxPayloadBytes != want.MaxPayloadBytes {
		t.Fatalf("declared defaults %+v differ from coresaga.DefaultOptions %+v", got, want)
	}
	for name, timeout := range map[string]any{
		"result":        mod.config.Completions.ProcessTimeout,
		"start_effect":  mod.config.Starts.ProcessTimeout,
		"result_effect": mod.config.NestResults.ProcessTimeout,
	} {
		if timeout != want.StoreTimeout {
			t.Errorf("%s process_timeout default = %v, want core StoreTimeout %v", name, timeout, want.StoreTimeout)
		}
	}
}

// saga.max_payload_bytes 由协调器（本 Mod）与发起方（wiring/nest）共用一份声明：同一服务里两个 Mod 都在时声明必须完全相同
// （A4 ① Merge），超过线上硬上限 4 MiB 在配置检查时就拒绝。
func TestSagaPayloadLimitIsOneDeclarationSharedWithNest(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sid", 1)
	cfg.Set("server_type", "game")
	if err := app.CheckConfig(cfg, NewMod(), kitnest.NewMod(nil)); err != nil {
		t.Fatalf("saga and nest Mods in one service: %v", err)
	}
	for _, schema := range []app.ConfigSchema{NewMod().ConfigSchema(), kitnest.NewMod(nil).ConfigSchema()} {
		if _, ok := schema.Lookup("saga.max_payload_bytes"); !ok {
			t.Fatalf("saga.max_payload_bytes missing from a declaration: %+v", schema.Keys)
		}
	}
	cfg.Set("saga.max_payload_bytes", 4<<20+1)
	if err := app.CheckConfig(cfg, NewMod()); err == nil {
		t.Fatal("saga.max_payload_bytes over the 4 MiB wire cap passed the config check")
	}
}

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

// 步骤的超时与重试次数由配置给默认值，可按步骤覆盖；Mod 把它交给 Engine，Register 时补齐。
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

// viper 把配置键折成小写，saga.steps 分不清只差大小写的类型或步骤名：只差大小写的名字直接报歧义错误，不论配置里有没有写到它
// （不能让后注册的定义静默覆盖先注册的）。
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
