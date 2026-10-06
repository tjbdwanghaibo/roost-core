package skill

func (runtime *Runtime) evalStateRead(cast *castInstance, read stateReadProgramValue) (RuntimeValue, error) {
	binding, err := runtime.evalStateBinding(cast, read.binding)
	if err != nil {
		return RuntimeValue{}, err
	}
	defaultValue, err := runtime.evalStateDefault(cast, read.state)
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
	defaultValue, err := runtime.evalStateDefault(cast, operation.state)
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

// evalStateDefault 在 state_default 上下文里求状态默认值：读 / 写处可能是施法流程、进程字段
// 或进程回调，默认值只能用表里在所有这些位置都求得出的引用（RR-20261005-NC-281）。
//
// 交给 Host 的默认值总是带 state 声明的类型：null 默认值（entity / position / snapshot_token 状态，
// 以及 shared 状态的缺省默认值）按字面量求值是 Base=valueKindNull 的缺省值，原样交出去时 Host 的 set
// 比较 before 与写入值的类型，第一次写入（状态还不存在、before 取默认值）每次都报类型不匹配
// （RR-20261006-02）。缺省时换成声明类型的缺省值，Host 与状态事件看到的都是同一个类型。
func (runtime *Runtime) evalStateDefault(cast *castInstance, state stateReferenceProgram) (RuntimeValue, error) {
	previous := cast.switchEvalContext(evalStateDefault)
	defer cast.switchEvalContext(previous)
	value, err := runtime.evalValue(cast, state.defaultValue)
	if err != nil {
		return RuntimeValue{}, err
	}
	if !value.Present() {
		return MissingRuntimeValue(state.typ), nil
	}
	return value, nil
}
