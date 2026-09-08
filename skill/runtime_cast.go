package skill

func (runtime *Runtime) executeDamage(cast *castInstance, operation damageOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	amount, err := runtime.evalInt(cast, operation.amount)
	if err != nil {
		return EffectResult{}, err
	}
	context := runtime.effectEventContext(cast, operation.effectIndex)
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: DamageCommand{Source: cast.caster, Owner: cast.caster, Target: target, Amount: amount, DamageType: operation.damageType, Element: operation.element, Tags: append([]GameplayTagHandle(nil), operation.combatTags...), CanCritical: operation.canCritical, Meta: CommandMeta{EffectIndex: operation.effectIndex}, Event: context}})
}

func (runtime *Runtime) executeHeal(cast *castInstance, operation healOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	amount, err := runtime.evalInt(cast, operation.amount)
	if err != nil {
		return EffectResult{}, err
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: HealCommand{Source: cast.caster, Target: target, Amount: amount, Meta: CommandMeta{EffectIndex: operation.effectIndex}, Event: runtime.effectEventContext(cast, operation.effectIndex)}})
}

func (runtime *Runtime) executeShield(cast *castInstance, operation shieldOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	amount, err := runtime.evalInt(cast, operation.amount)
	if err != nil {
		return EffectResult{}, err
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: ShieldCommand{Source: cast.caster, SourceSkill: cast.program.id, SourceCast: cast.id, Target: target, Amount: amount, DurationTicks: operation.durationTicks, Meta: CommandMeta{EffectIndex: operation.effectIndex}, Event: runtime.effectEventContext(cast, operation.effectIndex)}})
}

func (runtime *Runtime) executeStatus(cast *castInstance, operation statusOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	meta := CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}
	var payload EffectCommandPayload
	if operation.remove {
		payload = RemoveStatusCommand{SourceOwner: cast.caster, Target: target, Status: operation.status, Meta: meta, Event: runtime.effectEventContext(cast, operation.effectIndex)}
	} else {
		payload = StatusCommand{SourceOwner: cast.caster, SourceSkill: cast.program.id, SourceCast: cast.id, Target: target, Status: operation.status, DurationTicks: operation.durationTicks, Stacks: operation.stacks, MaxStacks: operation.maxStacks, Meta: meta, Event: runtime.effectEventContext(cast, operation.effectIndex)}
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: meta, Payload: payload})
}

func (runtime *Runtime) executeStatusInstance(cast *castInstance, operation modifyStatusInstanceOperation) (EffectResult, error) {
	value, err := runtime.evalValue(cast, operation.status)
	if err != nil {
		return EffectResult{}, err
	}
	status, ok := value.StatusInstance()
	if !ok {
		return EffectResult{}, ErrRuntimeTypeMismatch
	}
	command := ModifyStatusInstanceCommand{Owner: cast.caster, SourceSkillID: cast.program.id, Status: status, Operation: operation.operation, OwnershipPolicy: operation.ownershipPolicy, Event: runtime.effectEventContext(cast, operation.effectIndex), Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}}
	if operation.hasValue {
		command.Value, err = runtime.evalInt(cast, operation.value)
		if err != nil {
			return EffectResult{}, err
		}
	}
	if operation.hasTarget {
		command.Target, err = runtime.evalEntity(cast, operation.target)
		if err != nil {
			return EffectResult{}, err
		}
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: command.Meta, Payload: command})
}

func (runtime *Runtime) executeAttributeModifier(cast *castInstance, operation attributeModifierOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	value, err := runtime.evalInt(cast, operation.value)
	if err != nil {
		return EffectResult{}, err
	}
	meta := CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: meta, Payload: AttributeModifierCommand{SourceOwner: cast.caster, SourceSkill: cast.program.id, SourceCast: cast.id, Target: target, Attribute: operation.attribute, Operation: operation.operation, Value: value, DurationTicks: operation.durationTicks, Meta: meta, Event: runtime.effectEventContext(cast, operation.effectIndex)}})
}

func (runtime *Runtime) executeResource(cast *castInstance, operation resourceOperation) error {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return err
	}
	amount, err := runtime.evalInt(cast, operation.amount)
	if err != nil {
		return err
	}
	_, err = runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: ResourceCommand{Target: target, Resource: operation.resource, Operation: operation.operation, Amount: amount}})
	return err
}

func (runtime *Runtime) executeMemory(cast *castInstance, operation memoryOperation) error {
	if int(operation.memory) >= len(cast.memory) {
		return ErrProgramInvariant
	}
	switch operation.operation {
	case "clear":
		cast.memory[operation.memory] = MissingRuntimeValue(cast.program.memory[operation.memory].typ)
	case "set":
		value, err := runtime.evalValue(cast, operation.value)
		if err != nil {
			return err
		}
		cast.memory[operation.memory] = value
	case "add":
		value, err := runtime.evalValue(cast, operation.value)
		if err != nil {
			return err
		}
		result, err := CheckedAddRuntimeValues(cast.memory[operation.memory], value)
		if err != nil {
			return err
		}
		cast.memory[operation.memory] = result
	default:
		return ErrProgramInvariant
	}
	return nil
}

func (runtime *Runtime) executeTeleport(cast *castInstance, operation teleportOperation) (EffectResult, error) {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return EffectResult{}, err
	}
	destination, err := runtime.evalPosition(cast, operation.destination)
	if err != nil {
		return EffectResult{}, err
	}
	return runtime.applyHostEffect(cast, EffectCommand{Meta: CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}, Payload: TeleportCommand{Target: target, Destination: destination, OnBlocked: operation.onBlocked}})
}

func (runtime *Runtime) executeMotionImpulse(cast *castInstance, operation motionImpulseOperation) error {
	target, err := runtime.evalEntity(cast, operation.target)
	if err != nil {
		return err
	}
	origin, err := runtime.evalPosition(cast, operation.origin)
	if err != nil {
		return err
	}
	distance, err := runtime.evalInt(cast, operation.distance)
	if err != nil {
		return err
	}
	meta := CommandMeta{RequiredRevision: cast.visibleRevision, EffectIndex: operation.effectIndex}
	var payload EffectCommandPayload = KnockbackCommand{Target: target, From: origin, Distance: distance}
	if operation.kind == "pull" {
		payload = PullCommand{Target: target, Toward: origin, Distance: distance}
	}
	_, err = runtime.applyHostEffect(cast, EffectCommand{Meta: meta, Payload: payload})
	return err
}

func (runtime *Runtime) applyHostEffect(cast *castInstance, command EffectCommand) (EffectResult, error) {
	result, err := runtime.host.Apply(command)
	if err != nil {
		return EffectResult{}, err
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Commit.Revision)
	if err := runtime.drainHostEvents(cast); err != nil {
		return EffectResult{}, err
	}
	if outcome, _, known := effectPayloadOutcome(result.Payload); !known || outcome.Succeeded {
		if continuations, found := presentationEffectMount(cast.program, command.Meta.EffectIndex); found {
			runtime.emitEffectPresentation(cast, continuations, command.Meta.EffectIndex, result.Commit.Revision, presentationAnchorFromCommand(cast, command.Payload))
		}
	}
	return result, nil
}

func presentationAnchorFromCommand(cast *castInstance, payload EffectCommandPayload) PresentationAnchor {
	anchor := PresentationAnchor{Source: cast.caster, Target: cast.primaryTarget}
	switch command := payload.(type) {
	case DamageCommand:
		anchor.Source, anchor.Target = command.Source, command.Target
	case HealCommand:
		anchor.Source, anchor.Target = command.Source, command.Target
	case ShieldCommand:
		anchor.Source, anchor.Target = command.Source, command.Target
	case StatusCommand:
		anchor.Source, anchor.Target = command.SourceOwner, command.Target
	case RemoveStatusCommand:
		anchor.Source, anchor.Target = command.SourceOwner, command.Target
	case DispelStatusCommand:
		anchor.Target = command.Target
	case AttributeModifierCommand:
		anchor.Source, anchor.Target = command.SourceOwner, command.Target
	case ModifyStatusInstanceCommand:
		anchor.Source, anchor.Target = command.Owner, command.Target
	case ResourceCommand:
		anchor.Target = command.Target
	case TeleportCommand:
		anchor.Target = command.Target
		position := command.Destination
		anchor.Position = &position
	case KnockbackCommand:
		anchor.Target = command.Target
		position := command.From
		anchor.Position = &position
	case PullCommand:
		anchor.Target = command.Target
		position := command.Toward
		anchor.Position = &position
	case SpawnCommand:
		anchor.Source = command.Owner
		position := command.Position
		anchor.Position = &position
	case OwnedEntityCommand:
		anchor.Source, anchor.Target = command.Owner, command.Target
		position := command.Position
		anchor.Position = &position
	}
	return anchor
}

func (runtime *Runtime) effectEventContext(cast *castInstance, effect EffectIndex) EventContext {
	id := EventID((uint64(cast.id) << 32) | uint64(effect+1))
	context := deriveEvent(cast.eventContext, id)
	context.EffectIndex = effect
	return context
}

func (runtime *Runtime) evalEntity(cast *castInstance, value programValue) (EntityID, error) {
	result, err := runtime.evalValue(cast, value)
	if err != nil {
		return 0, err
	}
	entity, ok := result.Entity()
	if !ok {
		return 0, ErrRuntimeTypeMismatch
	}
	return entity, nil
}

func (runtime *Runtime) evalInt(cast *castInstance, value programValue) (int64, error) {
	result, err := runtime.evalValue(cast, value)
	if err != nil {
		return 0, err
	}
	integer, ok := result.Int()
	if !ok {
		return 0, ErrRuntimeTypeMismatch
	}
	return integer, nil
}

func (runtime *Runtime) evalPosition(cast *castInstance, value programValue) (Position, error) {
	result, err := runtime.evalValue(cast, value)
	if err != nil {
		return Position{}, err
	}
	position, ok := result.Position()
	if !ok {
		return Position{}, ErrRuntimeTypeMismatch
	}
	return position, nil
}
