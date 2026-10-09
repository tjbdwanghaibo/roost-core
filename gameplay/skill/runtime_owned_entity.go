package skill

import "errors"

func (runtime *Runtime) executeOwnedSummon(cast *castInstance, operation summonOperation) (EffectResult, error) {
	ownedHost, ok := runtime.host.(OwnedEntityRuntimeHost)
	if !ok {
		return EffectResult{}, ErrHostContractViolation
	}
	positionValue, err := runtime.evalValue(cast, operation.position)
	if err != nil {
		return EffectResult{}, err
	}
	position, ok := positionValue.Position()
	if !ok {
		return EffectResult{}, ErrRuntimeTypeMismatch
	}
	overrides := make([]SummonAttributeOverride, len(operation.attributeOverrides))
	for index, override := range operation.attributeOverrides {
		value, evalErr := runtime.evalInt(cast, override.value)
		if evalErr != nil {
			return EffectResult{}, evalErr
		}
		overrides[index] = SummonAttributeOverride{Attribute: override.attribute, Value: value}
	}
	parameters := make([]SummonParameterBinding, len(operation.parameterBindings))
	for index, binding := range operation.parameterBindings {
		value, evalErr := runtime.evalValue(cast, binding.value)
		if evalErr != nil {
			return EffectResult{}, evalErr
		}
		parameters[index] = SummonParameterBinding{Name: binding.name, Value: value}
	}
	command := SummonCommand{Owner: cast.caster, GameplayDigest: cast.program.identity.gameplayDigest, SourceSkillID: cast.program.id, SourceCastID: cast.id, SourceEffectIndex: operation.effectIndex, Template: operation.template, Position: position, Count: operation.count, DurationTicks: operation.durationTicks, AttributeOverrides: overrides, ParameterBindings: parameters}
	if operation.hasSpawn {
		command.Transactional = true
		failure, previewErr := runtime.previewOwnedSpawnCapacity(ownedHost, command)
		if previewErr != nil {
			return EffectResult{}, previewErr
		}
		if failure != ExpectedFailureNone {
			return EffectResult{Commit: CommitReceipt{Revision: cast.visibleRevision}, Payload: SummonEffectResult{ResultOutcome: failedResultOutcome(failure)}}, nil
		}
	}
	result, err := runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: command})
	if err != nil || !operation.hasSpawn {
		return result, err
	}
	payload, ok := result.Payload.(SummonEffectResult)
	if !ok {
		return EffectResult{}, ErrHostContractViolation
	}
	if !payload.Succeeded {
		return result, nil
	}
	if payload.TransactionID == 0 {
		return EffectResult{}, ErrHostContractViolation
	}
	started := make([]SpawnID, 0, len(payload.Entities))
	for _, entity := range payload.Entities {
		if err := runtime.startEntitySpawn(cast, operation.spawnTemplate, operation.template, entity, operation.durationTicks, position); err != nil {
			cleanupErrors := []error{err}
			for _, spawnID := range started {
				cleanupErrors = append(cleanupErrors, runtime.requestSpawnStop(cast, runtime.spawns.get(spawnID), StopCauseFailure, ""))
			}
			cleanupErrors = append(cleanupErrors, ownedHost.RollbackOwnedSummon(payload.TransactionID))
			return EffectResult{}, errors.Join(cleanupErrors...)
		}
		started = append(started, runtime.nextSpawnID)
	}
	if err := ownedHost.CommitOwnedSummon(payload.TransactionID); err != nil {
		cleanupErrors := []error{err}
		for _, spawnID := range started {
			cleanupErrors = append(cleanupErrors, runtime.requestSpawnStop(cast, runtime.spawns.get(spawnID), StopCauseFailure, ""))
		}
		cleanupErrors = append(cleanupErrors, ownedHost.RollbackOwnedSummon(payload.TransactionID))
		return EffectResult{}, errors.Join(cleanupErrors...)
	}
	if err := runtime.reapUnhandedEntitySpawns(); err != nil {
		return EffectResult{}, err
	}
	if err := runtime.reapOwnedSpawns(); err != nil {
		return EffectResult{}, err
	}
	return result, nil
}

func (runtime *Runtime) executeOwnedCommand(cast *castInstance, operation entityCommandOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	command := OwnedEntityCommand{Owner: cast.caster, GameplayDigest: cast.program.identity.gameplayDigest, Target: target, Command: operation.command, Behavior: operation.behavior}
	if operation.hasPosition {
		value, evalErr := runtime.evalValue(cast, operation.position)
		if evalErr != nil {
			return EffectResult{}, evalErr
		}
		position, ok := value.Position()
		if !ok {
			return EffectResult{}, ErrRuntimeTypeMismatch
		}
		command.Position = position
	}
	if operation.hasTargetEntity {
		command.TargetEntity, err = runtime.evalEntity(cast, operation.targetEntity)
		if err != nil {
			return EffectResult{}, err
		}
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: command})
}
