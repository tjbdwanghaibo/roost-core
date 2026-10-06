package saga

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

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
// 字段的类型与范围、未知字段由 saga Mod 的声明检查（A4 ①）。definitions 非空时，saga.steps 下每个类型与步骤必须
// 对应其中某个定义的步骤：写错名字的覆盖不会静默失效，而是让 Init 失败。生成工程的测试可以用它在单元测试里得到
// 与运行时相同的预算。定义里只差大小写的类型名或步骤名无法用配置键区分，直接报歧义错误（RR-20261006-06）。
// definitions 为空时无法核对名字，覆盖以 viper 给出的小写键保存；Engine.Register 的 StepBudgets.Resolve
// 原样查不到时按小写回退，大小写混写的类型 / 步骤名照样生效（RR-20261005-NC-194）。
func StepBudgetsFromConfig(cfg *viper.Viper, definitions ...coresaga.Definition) (coresaga.StepBudgets, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return coresaga.StepBudgets{}, err
	}
	return stepBudgets(settings.Saga.StepDefaults, settings.Saga.Steps, definitions)
}

func stepBudgets(defaults stepBudgetConfig, steps map[string]map[string]stepBudgetConfig, definitions []coresaga.Definition) (coresaga.StepBudgets, error) {
	budgets := coresaga.StepBudgets{Defaults: mergeStepBudget(coresaga.DefaultStepBudget(), defaults.budget())}
	known, err := knownSagaSteps(definitions)
	if err != nil {
		return coresaga.StepBudgets{}, err
	}
	typeNames := make([]string, 0, len(steps))
	for name := range steps {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, typeName := range typeNames {
		stepNames := make([]string, 0, len(steps[typeName]))
		for name := range steps[typeName] {
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
			if budgets.Overrides == nil {
				budgets.Overrides = map[coresaga.StepKey]coresaga.StepBudget{}
			}
			budgets.Overrides[key] = steps[typeName][stepName].budget()
		}
	}
	if err := budgets.Validate(); err != nil {
		return coresaga.StepBudgets{}, err
	}
	return budgets, nil
}

// budget 把声明读出的字段转成 core 的预算；没写的字段是零值（不覆盖）。
func (c stepBudgetConfig) budget() coresaga.StepBudget {
	return coresaga.StepBudget{Timeout: c.Timeout, MaxAttempts: uint32(c.MaxAttempts), BackoffMin: c.BackoffMin, BackoffMax: c.BackoffMax}
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
