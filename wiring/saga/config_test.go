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
	if err := mod.Init(sagaTestConfig()); err != nil {
		t.Fatal(err)
	}
	got, want := mod.config.Engine, coresaga.DefaultOptions()
	if got.Layout.Prefix != want.Layout.Prefix || got.Layout.Stream != want.Layout.Stream || got.Layout.Partitions != want.Layout.Partitions || got.Layout.Groups != want.Layout.Groups ||
		got.StreamMaxBytes != want.StreamMaxBytes || got.DuplicateWindow != want.DuplicateWindow || got.MaxBatch != want.MaxBatch || got.Tick != want.Tick ||
		got.SnapshotEveryEvents != want.SnapshotEveryEvents || got.SnapshotInterval != want.SnapshotInterval || got.TerminalRetention != want.TerminalRetention ||
		got.ReceiptRetention != want.ReceiptRetention || got.PublishTimeout != want.PublishTimeout || got.PublishUnknownBudget != want.PublishUnknownBudget ||
		got.QueryTimeout != want.QueryTimeout || got.StartVerdictTimeout != want.StartVerdictTimeout || got.PurgeStallAlarm != want.PurgeStallAlarm ||
		got.MaxPayloadBytes != want.MaxPayloadBytes || got.Replicas != want.Replicas || got.Admission.Enabled != want.Admission.Enabled {
		t.Fatalf("declared defaults %+v differ from coresaga.DefaultOptions %+v", got, want)
	}
	admission := want.Admission
	admission.HighRecords, admission.LowRecords, admission.StaleAfter = 5000, 2500, 2*time.Second
	if got.Admission.HighRecords != admission.HighRecords || got.Admission.LowRecords != admission.LowRecords || got.Admission.StaleAfter != admission.StaleAfter {
		t.Fatalf("declared admission %+v differs from the core defaults %+v", got.Admission, admission)
	}
	if got.Writer != "" {
		t.Fatalf("Init wrote a writer identity %q before Provide (it comes from the singleton incarnation)", got.Writer)
	}
}

// saga.max_payload_bytes 由协调器（本 Mod）与发起方（wiring/nest）共用一份声明：同一服务里两个 Mod 都在时声明必须完全相同
// （A4 ① Merge），超过线上硬上限 4 MiB 在配置检查时就拒绝。
func TestSagaPayloadLimitIsOneDeclarationSharedWithNest(t *testing.T) {
	cfg := sagaTestConfig()
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
	if !cfg.IsSet("saga.coordinators") {
		cfg.Set("saga.coordinators", []string{"1"})
	}
	return cfg
}

// sagaTestConfig 是只写了必填键（saga.coordinators）的配置。
func sagaTestConfig() *viper.Viper {
	cfg := viper.New()
	cfg.Set("saga.coordinators", []string{"1"})
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
	if err := empty.Init(sagaTestConfig()); err != nil {
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

// saga.receipt_retention 必须盖住每个步骤解析后的重试窗口（RV-20261030-02）：参与方剪掉过了保留期的回执后，同一操作的重试会再
// 执行一次业务。规则用到定义（预算可写在定义里、step_defaults 或 saga.steps 覆盖），配置检查（--check-config / doctor）与
// Init 都按 coresaga.CheckReceiptRetention 拒绝，不在运行期间才暴露。
func TestReceiptRetentionMustCoverEveryStepsRetryWindow(t *testing.T) {
	const identity = "server_type: game\nsid: 1\n"
	for name, tc := range map[string]struct {
		yaml string
		want string
	}{
		// 评审的复现：保留 60s，10s × 10 次 + 10s × 9 次退避 = 190s。
		"step_defaults": {yaml: "saga:\n  terminal_retention: 1m\n  receipt_retention: 1m\n  step_defaults:\n    timeout: 10s\n    max_attempts: 10\n    backoff_min: 10s\n    backoff_max: 10s\n",
			want: `step "debit": receipt retention 1m0s must be at least 2 × the step's retry window 3m10s`},
		// 只有一个步骤的覆盖超出：deliver 改为 20 次，窗口 5s × 20 + 5s × 19 = 195s，留 2m 不够；debit 用缺省预算（窗口 45s，2 倍 90s）够。
		"per-step override": {yaml: "saga:\n  terminal_retention: 1m\n  receipt_retention: 2m\n  steps:\n    gift_item:\n      deliver:\n        max_attempts: 20\n",
			want: `step "deliver": receipt retention 2m0s must be at least 2 × the step's retry window`},
	} {
		t.Run(name, func(t *testing.T) {
			if err := app.CheckConfig(yamlConfig(t, identity+tc.yaml), NewMod(giftDefinition())); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "saga.receipt_retention") {
				t.Fatalf("CheckConfig = %v, want the receipt retention refused naming %s", err, tc.want)
			}
			if err := NewMod(giftDefinition()).Init(yamlConfig(t, tc.yaml)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Init = %v, want the same refusal as the config check", err)
			}
		})
	}
	// 缺省（168h，缺省预算窗口 45s）与恰好 2 倍（窗口 45s，留 90s）都照常通过。
	for _, text := range []string{"saga: {}\n", "saga:\n  terminal_retention: 1m\n  receipt_retention: 90s\n"} {
		if err := app.CheckConfig(yamlConfig(t, identity+text), NewMod(giftDefinition())); err != nil {
			t.Fatalf("CheckConfig(%q) = %v", text, err)
		}
		if err := NewMod(giftDefinition()).Init(yamlConfig(t, text)); err != nil {
			t.Fatalf("Init(%q) = %v", text, err)
		}
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
