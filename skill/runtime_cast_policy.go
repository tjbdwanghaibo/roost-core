package skill

import "errors"

func (runtime *Runtime) beginPolicyWait(cast *castInstance) error {
	if !policyAllowsEnterFallthrough(cast.program.cast.mode) {
		return ErrProgramInvariant
	}
	if cast.policyActive {
		cast.status = CastSuspended
		return nil
	}
	cast.policyActive = true
	cast.status = CastSuspended
	key := skillStateKey{Caster: cast.caster, Skill: cast.program.id}
	runtime.activePolicies[key] = cast.id
	runtime.touchActivePolicyLocked(key)
	switch cast.program.cast.mode {
	case castModeCharge:
		reason := "cancelled"
		if cast.program.cast.autoRelease {
			reason = "charge_full"
		}
		frame := runtime.retainLocalFrame(cast.locals)
		return runtime.schedule(cast, cast.startTick+cast.program.cast.maxChargeTicks, &castAutoReleaseTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame, Reason: reason})
	case castModeToggle, castModeHold:
		pulseFrame := runtime.retainLocalFrame(cast.locals)
		if err := runtime.schedule(cast, runtime.currentTick+cast.program.cast.pulseIntervalTicks, &castPulseTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: pulseFrame, PulseIndex: 1}); err != nil {
			return err
		}
		releaseFrame := runtime.retainLocalFrame(cast.locals)
		return runtime.schedule(cast, cast.startTick+cast.program.cast.maxDurationTicks, &castAutoReleaseTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: releaseFrame, Reason: "max_duration"})
	default:
		return ErrProgramInvariant
	}
}

func (runtime *Runtime) executeCastPulse(cast *castInstance, pulseIndex int64) error {
	if !cast.policyActive {
		return nil
	}
	if err := runtime.payCostList(cast, cast.program.cast.sustainCosts); err != nil {
		if errors.Is(err, ErrInsufficientResource) {
			return runtime.releaseCast(cast, "resource_depleted")
		}
		return err
	}
	cast.pulseIndex = pulseIndex
	runtime.emitCastLifecycleEvent(cast, "cast_pulse")
	if operation, found := phaseRootOperation(cast.program, cast.program.phases[cast.currentPhase], "pulse"); found {
		control, err := runtime.executeOperation(cast, operation)
		if err != nil {
			return err
		}
		if control.kind != flowContinue {
			return runtime.resolveControl(cast, control)
		}
	}
	nextDue := runtime.currentTick + cast.program.cast.pulseIntervalTicks
	endDue := cast.startTick + cast.program.cast.maxDurationTicks
	if nextDue < endDue && cast.policyActive {
		frame := runtime.retainLocalFrame(cast.locals)
		if err := runtime.schedule(cast, nextDue, &castPulseTask{CastID: cast.id, PhaseToken: cast.phaseToken, Frame: frame, PulseIndex: pulseIndex + 1}); err != nil {
			return err
		}
	}
	cast.status = CastSuspended
	return nil
}

func (runtime *Runtime) executeAutoRelease(cast *castInstance, reason string) error {
	if !cast.policyActive {
		return nil
	}
	if cast.program.cast.mode == castModeCharge && reason == "cancelled" {
		cast.releaseReason = "cancelled"
		cast.policyActive = false
		cast.windowStage = CastWindowCancelled
		cast.status = CastFinished
		runtime.markAbilityCastFinished(cast)
		delete(runtime.activePolicies, skillStateKey{Caster: cast.caster, Skill: cast.program.id})
		runtime.touchActivePolicyLocked(skillStateKey{Caster: cast.caster, Skill: cast.program.id})
		runtime.emitCastLifecycleEvent(cast, "cast_cancelled")
		return nil
	}
	return runtime.releaseCast(cast, reason)
}
