package skill

import "fmt"

func runShapePass(context *compileContext) {
	if context.artifacts.ir == nil {
		context.addDiagnostic(DiagnosticShapeInvalid, "$", "normalized IR is required")
		return
	}
	if context.artifacts.ir.globalCooldownTicks < 0 {
		context.addDiagnostic(DiagnosticShapeInvalid, "$.global_cooldown_ticks", "global cooldown ticks must be non-negative")
	}
	requireNonNegativeAuthoredTicks(context)
	rejectValuesTheRuntimeDoesNotExecute(context)
	activation := context.artifacts.ir.activation
	if activation.kind == "active" {
		window := activation.castWindow
		if window.windupTicks < 0 || window.commitTick < 0 || window.recoveryTicks < 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window", "cast window ticks must be non-negative")
		}
		if window.hasWindupExpression {
			if window.windupTicks != 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.windup_ticks", "windup_ticks must be omitted when windup_ticks_expression is set")
			}
			if window.windupTicksMin < 0 || window.windupTicksMax < window.windupTicksMin {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.windup_ticks_expression", "windup ticks bounds require 0 <= windup_ticks_min <= windup_ticks_max")
			}
			if window.commitTick > window.windupTicksMin {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.commit_tick", "commit_tick must not exceed windup_ticks_min")
			}
		} else {
			if window.windupTicksMin != 0 || window.windupTicksMax != 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.windup_ticks_min", "windup ticks bounds require windup_ticks_expression")
			}
			if window.commitTick > window.windupTicks {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window", "commit_tick must not exceed windup_ticks")
			}
		}
		if window.hasRecoveryExpression {
			if window.recoveryTicks != 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.recovery_ticks", "recovery_ticks must be omitted when recovery_ticks_expression is set")
			}
			if window.recoveryTicksMin < 0 || window.recoveryTicksMax < window.recoveryTicksMin {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.recovery_ticks_expression", "recovery ticks bounds require 0 <= recovery_ticks_min <= recovery_ticks_max")
			}
		} else if window.recoveryTicksMin != 0 || window.recoveryTicksMax != 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.recovery_ticks_min", "recovery ticks bounds require recovery_ticks_expression")
		}
		if window.movement != "allowed" && window.movement != "slow" && window.movement != "locked" {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.movement", "movement must be allowed, slow, or locked")
		}
		if window.turning != "allowed" && window.turning != "locked" {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cast_window.turning", "turning must be allowed or locked")
		}
		policy := activation.policy
		switch policy.mode {
		case castModeToggle, castModeHold:
			if policy.pulseIntervalTicks <= 0 || policy.maxDurationTicks <= 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.policy", "toggle and hold require positive pulse_interval_ticks and max_duration_ticks")
			}
		case castModeCharge:
			if policy.maxChargeTicks <= 0 || policy.minChargeBP < 0 || policy.minChargeBP > 10000 {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.policy", "charge requires positive max_charge_ticks and min_charge_bp in 0..10000")
			}
		case castModeAmmo:
			if policy.maxStock <= 0 || policy.rechargeTicks <= 0 || policy.initialStock < 0 || policy.initialStock > policy.maxStock {
				context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.policy", "ammo stock and recharge configuration is invalid")
			}
		}
	} else {
		if activation.cooldownScope != "caster" && activation.cooldownScope != "target" {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.cooldown_scope", "passive cooldown_scope must be caster or target")
		}
		if activation.procPolicy.maxDepth < 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, "$.activation.proc_policy.max_depth", "proc max depth must be non-negative")
		}
	}
	context.artifacts.ir.walkFlows(func(flow flowIR) {
		switch typed := flow.(type) {
		case *sequenceFlowIR:
			if len(typed.steps) == 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path, "sequence steps must not be empty")
			}
		case *parallelFlowIR:
			if len(typed.branches) == 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path, "parallel branches must not be empty")
			}
		case *selectFlowIR:
			if typed.selectPlan.limit <= 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".select.limit", "select limit must be positive")
			}
			if _, ok := typed.consume.(*selectOneConsumeIR); ok && typed.selectPlan.limit != 1 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".select.limit", "consume.one requires limit 1")
			}
			if typed.selectPlan.elementType == selectionAbility && typed.selectPlan.limit > context.environment.Limits.MaxAbilitySelections {
				context.addDiagnostic(DiagnosticBudgetExceeded, typed.source.Path+".select.limit", "ability select limit exceeds the environment maximum")
			}
		}
	})
	context.artifacts.shape.checked = !context.hasErrors()
}

// requireNonNegativeAuthoredTicks 是“作者写的 tick 不能为负”在 shape pass 里的
// 集中检查（RR-20261005-NC-152）。Tick 是 int64，wire 不限制符号；此前只有
// global_cooldown、cast window、policy 等字段在这里检查，其余字段要么不检查
// （负 cooldown 让冷却立即结束、负 repeat 间隔在运行时排到过去而
// ErrProgramInvariant、负状态时长每次被 Host 拒绝），要么靠预算 pass 把负数
// 饱和成 MaxInt64 后在 `$` 报一个误导的生命期超限。motion、process、numeric
// track、状态生命期等字段仍由各自 pass 检查，这里只补没有检查的字段。
func requireNonNegativeAuthoredTicks(context *compileContext) {
	ir := context.artifacts.ir
	if ir.cooldownTicks < 0 {
		context.addDiagnostic(DiagnosticShapeInvalid, "$.cooldown_ticks", "cooldown_ticks must be non-negative")
	}
	for index, phase := range ir.phases {
		if phase.timeoutTicks < 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, fmt.Sprintf("$.phases[%d].timeout_ticks", index), "timeout_ticks must be non-negative")
		}
	}
	ir.walkFlows(func(flow flowIR) {
		switch typed := flow.(type) {
		case *waitFlowIR:
			if typed.ticks < 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".ticks", "wait ticks must be non-negative")
			}
		case *repeatFlowIR:
			if typed.intervalTicks < 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".interval_ticks", "repeat interval_ticks must be non-negative")
			}
		case *selectFlowIR:
			if chain, ok := typed.selectPlan.shape.(*chainShapeIR); ok && chain.hopIntervalTicks < 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".select.shape.hop_interval_ticks", "chain hop_interval_ticks must be non-negative")
			}
		case *effectFlowIR:
			if status, ok := typed.effect.(*addStatusEffectIR); ok && status.durationTicks < 0 {
				context.addDiagnostic(DiagnosticShapeInvalid, status.source.Path+".duration_ticks", "status duration_ticks must be non-negative")
			}
		}
	})
}

// rejectValuesTheRuntimeDoesNotExecute 拒绝能解码、却不会按作者意思执行的取值。
//
//   - 只编译不传 Host 的字段（RR-20261005-NC-213）：chain 的 allow_repeat /
//     hop_interval_ticks 不在 ChainSelectShape 里，Runtime 也不按间隔排 hop；
//     attribute_modifier 的 stack_policy / max_stacks 不在 AttributeModifierCommand
//     里。只接受 Runtime 实际执行的默认值，与 NC-151 的方向 B 一致；实现它们需要
//     维护者先定语义。
//   - 两个参考 Host（MemoryHost、combatcomponent）与 Runtime 都拒绝的取值
//     （RR-20261005-NC-215）：attribute_modifier 时长 <= 0、add_status 时长为 0
//     （负值由 requireNonNegativeAuthoredTicks 报）、resource 的 operation 不是
//     set / add / spend / sub、cost 字面量为负（runtime_turn.go 的 payCostList 拒绝）。
//     这些定义此前编译通过，施法时每次失败。attribute_modifier 的 operation 要对照
//     属性 catalog，在 authority pass 里检查。
func rejectValuesTheRuntimeDoesNotExecute(context *compileContext) {
	ir := context.artifacts.ir
	rejectNegativeCostLiterals(context, ir.costs)
	rejectNegativeCostLiterals(context, ir.activation.policy.sustainCosts)
	ir.walkFlows(func(flow flowIR) {
		switch typed := flow.(type) {
		case *selectFlowIR:
			if chain, ok := typed.selectPlan.shape.(*chainShapeIR); ok {
				if chain.allowRepeat {
					context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".select.shape.allow_repeat", "chain allow_repeat is not executed by the Runtime; only false is accepted")
				}
				if chain.hopIntervalTicks > 0 {
					context.addDiagnostic(DiagnosticShapeInvalid, typed.source.Path+".select.shape.hop_interval_ticks", "chain hop_interval_ticks is not executed by the Runtime; only 0 is accepted")
				}
			}
		case *effectFlowIR:
			switch effect := typed.effect.(type) {
			case *attributeModifierEffectIR:
				if effect.stackPolicy != "" {
					context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".stack_policy", "attribute_modifier stack_policy is not passed to the Host; modifiers always stack independently")
				}
				if effect.maxStacks != 0 {
					context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".max_stacks", "attribute_modifier max_stacks is not passed to the Host; modifiers always stack independently")
				}
				if effect.durationTicks <= 0 {
					context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".duration_ticks", "attribute_modifier duration_ticks must be positive")
				}
			case *addStatusEffectIR:
				if effect.durationTicks == 0 {
					context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".duration_ticks", "status duration_ticks must be positive")
				}
			case *resourceEffectIR:
				switch effect.operation {
				case "set", "add", "spend", "sub":
				default:
					context.addDiagnostic(DiagnosticShapeInvalid, effect.source.Path+".operation", "resource operation must be set, add, spend, or sub")
				}
			}
		}
	})
}

func rejectNegativeCostLiterals(context *compileContext, costs []costIR) {
	for _, cost := range costs {
		if literal, ok := cost.amount.(*intValueIR); ok && literal.value < 0 {
			context.addDiagnostic(DiagnosticShapeInvalid, cost.source.Path+".amount", "cost amount must be non-negative")
		}
	}
}
