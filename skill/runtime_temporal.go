package skill

func (runtime *Runtime) executeCaptureSnapshot(cast *castInstance, operation captureSnapshotOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: TemporalCaptureCommand{Owner: cast.caster, Target: target, ProgramID: cast.program.id, GameplayDigest: cast.program.authority.Digest, Profile: operation.profile, Context: runtime.effectEventContext(cast, operation.effectIndex)}})
}

func (runtime *Runtime) executeRestoreSnapshot(cast *castInstance, operation restoreSnapshotOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	value, err := runtime.evalValue(cast, operation.snapshot)
	if err != nil {
		return EffectResult{}, err
	}
	token, ok := value.SnapshotToken()
	if !ok {
		return EffectResult{}, ErrRuntimeTypeMismatch
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: TemporalRestoreCommand{Owner: cast.caster, Target: target, ProgramID: cast.program.id, GameplayDigest: cast.program.authority.Digest, Token: token, OnBlocked: operation.onBlocked, Context: runtime.effectEventContext(cast, operation.effectIndex)}})
}
