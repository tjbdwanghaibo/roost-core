package skill

import "fmt"

func (host *MemoryHost) Apply(command EffectCommand) (EffectResult, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return EffectResult{}, err
	}
	switch payload := command.Payload.(type) {
	case TemporalCaptureCommand:
		return host.captureTemporalLocked(command.Meta, payload)
	case TemporalRestoreCommand:
		return host.restoreTemporalLocked(command.Meta, payload)
	case DamageCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.applyDamageLocked(payload)
	case HealCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.applyHealLocked(payload)
	case ShieldCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.applyShieldLocked(payload)
	case StatusCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.applyStatusLocked(payload)
	case RemoveStatusCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.removeStatusLocked(payload)
	case DispelStatusCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.dispelStatusLocked(payload)
	case ModifyStatusInstanceCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		return host.modifyStatusInstanceLocked(payload)
	case AttributeModifierCommand:
		payload.Meta = mergeCommandMeta(payload.Meta, command.Meta)
		if err := host.requireRevisionLocked(payload.Meta.RequiredRevision); err != nil {
			return EffectResult{}, err
		}
		return host.applyAttributeModifierLocked(payload)
	case TeleportCommand:
		entity, ok := host.entities[payload.Target]
		if !ok {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: TeleportEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
		}
		if entity.Position == payload.Destination {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Value: PositionRuntimeValue(entity.Position), Payload: TeleportEffectResult{ResultOutcome: successfulResultOutcome(), Position: entity.Position}}, nil
		}
		entity.Position = payload.Destination
		host.entities[payload.Target] = entity
		return EffectResult{Commit: host.commitLocked("teleported", payload.Target, 0), Value: PositionRuntimeValue(entity.Position), Payload: TeleportEffectResult{ResultOutcome: successfulResultOutcome(), Position: entity.Position}}, nil
	case KnockbackCommand:
		entity, ok := host.entities[payload.Target]
		if !ok {
			return EffectResult{}, ErrEntityNotFound
		}
		entity.Position = moveByDominantAxis(entity.Position, payload.From, payload.Distance, false)
		host.entities[payload.Target] = entity
		return EffectResult{Commit: host.commitLocked("knockback", payload.Target, 0), Value: PositionRuntimeValue(entity.Position)}, nil
	case PullCommand:
		entity, ok := host.entities[payload.Target]
		if !ok {
			return EffectResult{}, ErrEntityNotFound
		}
		entity.Position = moveByDominantAxis(entity.Position, payload.Toward, payload.Distance, true)
		host.entities[payload.Target] = entity
		return EffectResult{Commit: host.commitLocked("pulled", payload.Target, 0), Value: PositionRuntimeValue(entity.Position)}, nil
	case ResourceCommand:
		entity, ok := host.entities[payload.Target]
		if !ok {
			return EffectResult{}, ErrEntityNotFound
		}
		resource := host.resourceKeyLocked(payload.Resource)
		if resource == "" {
			return EffectResult{}, ErrCombatHandleInvalid
		}
		before := entity.Resources[resource]
		after := before
		switch payload.Operation {
		case "set":
			after = payload.Amount
		case "add":
			after = saturatingInt64Add(before, payload.Amount)
		case "spend", "sub":
			if payload.Amount < 0 || before < payload.Amount {
				return EffectResult{}, ErrInsufficientResource
			}
			after = before - payload.Amount
		default:
			return EffectResult{}, fmt.Errorf("skill: unsupported resource operation %q", payload.Operation)
		}
		if after < 0 {
			return EffectResult{}, ErrInsufficientResource
		}
		if after == before {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Value: IntRuntimeValue(after, quantityResourceAmount)}, nil
		}
		entity.Resources[resource] = after
		host.entities[payload.Target] = entity
		return EffectResult{Commit: host.commitLocked("resource_changed", payload.Target, 0), Value: IntRuntimeValue(after, quantityResourceAmount)}, nil
	case SpawnCommand:
		return host.spawnOwnedLocked(payload)
	case OwnedEntityCommand:
		return host.commandOwnedLocked(payload)
	default:
		return EffectResult{}, fmt.Errorf("skill: unsupported effect command %T", command.Payload)
	}
}

func mergeCommandMeta(payload, envelope CommandMeta) CommandMeta {
	if payload.RequiredRevision == 0 {
		payload.RequiredRevision = envelope.RequiredRevision
	}
	if payload.EffectIndex == 0 {
		payload.EffectIndex = envelope.EffectIndex
	}
	return payload
}

func moveByDominantAxis(position, toward Position, distance int64, pull bool) Position {
	if distance < 0 {
		distance = absoluteDifference(distance, 0)
	}
	dx, dy := saturatingInt64Sub(toward.X, position.X), saturatingInt64Sub(toward.Y, position.Y)
	if !pull {
		dx, dy = saturatingInt64Sub(0, dx), saturatingInt64Sub(0, dy)
	}
	if absoluteDifference(dx, 0) >= absoluteDifference(dy, 0) {
		if dx < 0 {
			position.X = saturatingInt64Add(position.X, -distance)
		} else if dx > 0 {
			position.X = saturatingInt64Add(position.X, distance)
		}
	} else if dy < 0 {
		position.Y = saturatingInt64Add(position.Y, -distance)
	} else if dy > 0 {
		position.Y = saturatingInt64Add(position.Y, distance)
	}
	return position
}
