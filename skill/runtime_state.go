package skill

func (runtime *Runtime) evalStateRead(cast *castInstance, read stateReadProgramValue) (RuntimeValue, error) {
	binding, err := runtime.evalStateBinding(cast, read.binding)
	if err != nil {
		return RuntimeValue{}, err
	}
	defaultValue, err := runtime.evalValue(cast, read.state.defaultValue)
	if err != nil {
		return RuntimeValue{}, err
	}
	result, err := runtime.host.ReadState(StateReadRequest{Meta: QueryMeta{RequiredRevision: cast.visibleRevision}, Handle: runtime.stateHandle(cast.program, read.state), Binding: binding, Default: defaultValue})
	if err != nil {
		return RuntimeValue{}, err
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Meta.Revision)
	return result.Value, nil
}

func (runtime *Runtime) executeStateMutation(cast *castInstance, operation stateOperation) (StateMutationResult, error) {
	binding, err := runtime.evalStateBinding(cast, operation.binding)
	if err != nil {
		return StateMutationResult{}, err
	}
	defaultValue, err := runtime.evalValue(cast, operation.state.defaultValue)
	if err != nil {
		return StateMutationResult{}, err
	}
	value := RuntimeValue{}
	if operation.hasValue {
		value, err = runtime.evalValue(cast, operation.value)
		if err != nil {
			return StateMutationResult{}, err
		}
	}
	result, err := runtime.host.ModifyState(StateMutationCommand{
		Meta:   CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex},
		Handle: runtime.stateHandle(cast.program, operation.state), Binding: binding, Scope: operation.state.scope,
		Operation: operation.operation, Value: value, Default: defaultValue,
		Minimum: operation.state.minimum, Maximum: operation.state.maximum,
		DurationTicks: operation.durationTicks, MaximumDurationTicks: operation.state.maximumDurationTicks,
		ExpiryPolicy: operation.expiryPolicy, ClearOn: append([]string(nil), operation.state.clearOn...),
		Event: runtime.effectEventContext(cast, operation.effectIndex),
	})
	if err != nil {
		return StateMutationResult{}, err
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
	if err := runtime.drainHostEvents(cast); err != nil {
		return StateMutationResult{}, err
	}
	return result, nil
}

func (runtime *Runtime) stateHandle(program *Program, state stateReferenceProgram) StateHandle {
	if state.shared != 0 {
		return StateHandle{Shared: state.shared}
	}
	return StateHandle{GameplayDigest: program.identity.gameplayDigest, Slot: state.slot}
}

func (runtime *Runtime) evalStateBinding(cast *castInstance, binding stateBindingProgram) (StateScopeBinding, error) {
	result := StateScopeBinding{}
	if binding.hasOwner {
		value, err := runtime.evalEntity(cast, binding.owner)
		if err != nil {
			return StateScopeBinding{}, err
		}
		result.Owner = value
	}
	if binding.hasSubject {
		value, err := runtime.evalEntity(cast, binding.subject)
		if err != nil {
			return StateScopeBinding{}, err
		}
		result.Subject = value
	}
	if binding.hasTeamOf {
		value, err := runtime.evalEntity(cast, binding.teamOf)
		if err != nil {
			return StateScopeBinding{}, err
		}
		result.Team = uint64(value)
	}
	return result, nil
}
