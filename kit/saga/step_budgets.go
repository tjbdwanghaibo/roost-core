package saga

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

// stepBudgetFields 是 saga.step_defaults 与 saga.steps.<type>.<step> 下允许的键。
var stepBudgetFields = map[string]struct{}{"timeout": {}, "max_attempts": {}, "backoff_min": {}, "backoff_max": {}}

// StepBudgetsFromConfig 读取步骤的超时与重试预算（U-0280：重试次数是配置，一次操作可以有多次尝试）：
//
//	saga:
//	  step_defaults:            # 定义里没写的字段取这里；不写取框架默认 5s / 5 次 / 100ms..5s
//	    timeout: 5s
//	    max_attempts: 5
//	    backoff_min: 100ms
//	    backoff_max: 5s
//	  steps:                    # 按步骤覆盖，优先于定义与 step_defaults
//	    gift_item:              # Definition.Type
//	      debit:                # Step.Name
//	        max_attempts: 15
//
// definitions 非空时，saga.steps 下每个类型与步骤必须对应其中某个定义的步骤：写错名字的覆盖不会
// 静默失效，而是让 Init 失败。未知字段同样拒绝。生成工程的测试可以用它在单元测试里得到与运行时相同的预算。
// 定义里只差大小写的类型名或步骤名无法用配置键区分，直接报歧义错误（RR-20261006-06）。
// definitions 为空时无法核对名字，覆盖以 viper 给出的小写键保存；Engine.Register 的 StepBudgets.Resolve
// 原样查不到时按小写回退，大小写混写的类型 / 步骤名照样生效（RR-20261005-NC-194）。
func StepBudgetsFromConfig(cfg *viper.Viper, definitions ...coresaga.Definition) (coresaga.StepBudgets, error) {
	budgets := coresaga.StepBudgets{Defaults: coresaga.DefaultStepBudget()}
	if cfg == nil {
		return budgets, nil
	}
	defaults, err := readStepBudget(cfg, "saga.step_defaults")
	if err != nil {
		return coresaga.StepBudgets{}, err
	}
	budgets.Defaults = mergeStepBudget(budgets.Defaults, defaults)
	known, err := knownSagaSteps(definitions)
	if err != nil {
		return coresaga.StepBudgets{}, err
	}
	sagaTypes := cfg.GetStringMap("saga.steps")
	typeNames := make([]string, 0, len(sagaTypes))
	for name := range sagaTypes {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, typeName := range typeNames {
		steps := cfg.GetStringMap("saga.steps." + typeName)
		if len(steps) == 0 {
			return coresaga.StepBudgets{}, fmt.Errorf("saga.steps.%s: expected a map of step names", typeName)
		}
		stepNames := make([]string, 0, len(steps))
		for name := range steps {
			stepNames = append(stepNames, name)
		}
		sort.Strings(stepNames)
		for _, stepName := range stepNames {
			key := coresaga.StepKey{Type: typeName, Step: stepName}
			if len(definitions) > 0 {
				resolved, ok := known[typeName][stepName]
				if !ok {
					return coresaga.StepBudgets{}, fmt.Errorf("saga.steps.%s.%s: no registered saga %q has a step %q", typeName, stepName, typeName, stepName)
				}
				key = resolved
			}
			override, err := readStepBudget(cfg, "saga.steps."+typeName+"."+stepName)
			if err != nil {
				return coresaga.StepBudgets{}, err
			}
			if budgets.Overrides == nil {
				budgets.Overrides = map[coresaga.StepKey]coresaga.StepBudget{}
			}
			budgets.Overrides[key] = override
		}
	}
	if err := budgets.Validate(); err != nil {
		return coresaga.StepBudgets{}, err
	}
	return budgets, nil
}

// knownSagaSteps 按 viper 的小写键建已知步骤表。viper 读出的配置键一律小写，只差大小写的两个类型名或同一类型下
// 两个步骤名在 saga.steps 里无法区分：之前后注册的静默覆盖先注册的，覆盖落到谁身上取决于定义顺序
// （RR-20261006-06）。这里直接报歧义错误，与 configdata 大小写敏感、不猜的方向一致；同一个名字重复出现不算冲突。
func knownSagaSteps(definitions []coresaga.Definition) (map[string]map[string]coresaga.StepKey, error) {
	known := map[string]map[string]coresaga.StepKey{}
	typeNames := map[string]string{}
	for _, definition := range definitions {
		typeKey := strings.ToLower(definition.Type)
		if previous, ok := typeNames[typeKey]; ok && previous != definition.Type {
			return nil, fmt.Errorf("saga definitions: types %q and %q differ only in case; saga.steps keys are case-insensitive and cannot tell them apart", previous, definition.Type)
		}
		typeNames[typeKey] = definition.Type
		if known[typeKey] == nil {
			known[typeKey] = map[string]coresaga.StepKey{}
		}
		for _, step := range definition.Steps {
			stepKey := strings.ToLower(step.Name)
			if previous, ok := known[typeKey][stepKey]; ok && previous.Step != step.Name {
				return nil, fmt.Errorf("saga definition %q: steps %q and %q differ only in case; saga.steps keys are case-insensitive and cannot tell them apart", definition.Type, previous.Step, step.Name)
			}
			known[typeKey][stepKey] = coresaga.StepKey{Type: definition.Type, Step: step.Name}
		}
	}
	return known, nil
}

func readStepBudget(cfg *viper.Viper, prefix string) (coresaga.StepBudget, error) {
	if !cfg.IsSet(prefix) {
		return coresaga.StepBudget{}, nil
	}
	for field := range cfg.GetStringMap(prefix) {
		if _, ok := stepBudgetFields[field]; !ok {
			return coresaga.StepBudget{}, fmt.Errorf("%s.%s: unknown step budget field (want timeout, max_attempts, backoff_min, backoff_max)", prefix, field)
		}
	}
	var budget coresaga.StepBudget
	durations := []struct {
		field  string
		target *time.Duration
	}{{"timeout", &budget.Timeout}, {"backoff_min", &budget.BackoffMin}, {"backoff_max", &budget.BackoffMax}}
	for _, item := range durations {
		key := prefix + "." + item.field
		if !cfg.IsSet(key) {
			continue
		}
		// 不带单位的数字以前读成纳秒、照样为正（RR-20261005-NC-190）。
		value, err := app.ConfigDuration(cfg, key)
		if err != nil {
			return coresaga.StepBudget{}, err
		}
		if value <= 0 {
			return coresaga.StepBudget{}, fmt.Errorf("%s: want a positive duration, got %q", key, cfg.GetString(key))
		}
		*item.target = value
	}
	if key := prefix + ".max_attempts"; cfg.IsSet(key) {
		attempts, err := app.ConfigInt(cfg, key)
		if err != nil {
			return coresaga.StepBudget{}, err
		}
		if attempts <= 0 || attempts > 1000 {
			return coresaga.StepBudget{}, fmt.Errorf("%s: want 1..1000, got %q", key, cfg.GetString(key))
		}
		budget.MaxAttempts = uint32(attempts)
	}
	return budget, nil
}

func mergeStepBudget(base, override coresaga.StepBudget) coresaga.StepBudget {
	if override.Timeout > 0 {
		base.Timeout = override.Timeout
	}
	if override.MaxAttempts > 0 {
		base.MaxAttempts = override.MaxAttempts
	}
	if override.BackoffMin > 0 {
		base.BackoffMin = override.BackoffMin
	}
	if override.BackoffMax > 0 {
		base.BackoffMax = override.BackoffMax
	}
	return base
}
