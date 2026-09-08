package skill

func runShapePass(context *compileContext) {
	if context.artifacts.ir == nil {
		context.addDiagnostic(DiagnosticShapeInvalid, "$", "normalized IR is required")
		return
	}
	if context.artifacts.ir.globalCooldownTicks < 0 {
		context.addDiagnostic(DiagnosticShapeInvalid, "$.global_cooldown_ticks", "global cooldown ticks must be non-negative")
	}
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
