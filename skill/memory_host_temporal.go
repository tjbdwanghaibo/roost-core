package skill

import "sort"

type temporalSnapshotRecord struct {
	authorityDigest string
	profile         TemporalSnapshotProfile
	owner           EntityID
	programID       string
	target          EntityID
	capturedTick    Tick
	expiresTick     Tick
	entity          MemoryEntity
	statuses        []statusInstance
}

func (host *MemoryHost) captureTemporalLocked(meta CommandMeta, command TemporalCaptureCommand) (EffectResult, error) {
	if err := host.requireRevisionLocked(meta.RequiredRevision); err != nil {
		return EffectResult{}, err
	}
	profile, found := host.temporalProfileLocked(command.Profile)
	if !found || command.Owner == 0 || command.Target == 0 || command.ProgramID == "" || command.GameplayDigest == "" || command.GameplayDigest != host.authority.Digest {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SnapshotCaptureEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePolicyRejected)}}, nil
	}
	entity, found := host.entities[command.Target]
	if !found {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SnapshotCaptureEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	if host.temporalSnapshotCountLocked(command.Owner, command.ProgramID, profile.Handle) >= profile.MaximumPerOwner {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SnapshotCaptureEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureCapacityReached)}}, nil
	}
	host.nextTemporalToken++
	token, err := NewSnapshotToken(host.nextTemporalToken)
	if err != nil {
		return EffectResult{}, err
	}
	host.temporalSnapshots[token.OpaqueID()] = temporalSnapshotRecord{authorityDigest: command.GameplayDigest, profile: cloneTemporalProfile(profile), owner: command.Owner, programID: command.ProgramID, target: command.Target, capturedTick: host.tick, expiresTick: host.tick + profile.MaximumAgeTicks, entity: captureTemporalEntity(entity, profile.Fields), statuses: host.cloneStatusesForTemporalLocked(command.Target, containsTemporalField(profile.Fields, "statuses"))}
	receipt := host.commitTemporalLocked("temporal_snapshot_captured", command.Target, command.Context)
	return EffectResult{Commit: receipt, Payload: SnapshotCaptureEffectResult{ResultOutcome: successfulResultOutcome(), Token: token}}, nil
}

func (host *MemoryHost) restoreTemporalLocked(meta CommandMeta, command TemporalRestoreCommand) (EffectResult, error) {
	if err := host.requireRevisionLocked(meta.RequiredRevision); err != nil {
		return EffectResult{}, err
	}
	record, found := host.temporalSnapshots[command.Token.OpaqueID()]
	if !found || host.tick > record.expiresTick {
		return host.temporalRestoreRejectedLocked(command, ExpectedFailureReferenceExpired), nil
	}
	if command.Owner != record.owner || command.ProgramID != record.programID || command.GameplayDigest != record.authorityDigest || command.Target != record.target {
		return host.temporalRestoreRejectedLocked(command, ExpectedFailurePermissionDenied), nil
	}
	current, found := host.entities[command.Target]
	if !found {
		return host.temporalRestoreRejectedLocked(command, ExpectedFailureInvalidTarget), nil
	}
	profile := cloneTemporalProfile(record.profile)
	policy := command.OnBlocked
	if policy == "" {
		policy = profile.BlockedPositionPolicy
	}
	if policy != profile.BlockedPositionPolicy {
		return host.temporalRestoreRejectedLocked(command, ExpectedFailurePolicyRejected), nil
	}
	if !current.Alive && !profile.AllowRevive {
		return host.temporalRestoreRejectedLocked(command, ExpectedFailurePolicyRejected), nil
	}
	restored := cloneTemporalRestoreEntity(current)
	applied := make([]string, 0, len(profile.Fields))
	skipped := make([]string, 0, len(profile.Fields))
	for _, field := range profile.Fields {
		switch field {
		case "position":
			position, blocked := host.temporalBlocked[record.entity.Position]
			if blocked {
				switch policy {
				case "fail":
					return host.temporalRestoreRejectedLocked(command, ExpectedFailureDestinationBlocked), nil
				case "nearest":
					restored.Position = position
				case "stay":
					skipped = append(skipped, field)
					continue
				}
			} else {
				restored.Position = record.entity.Position
			}
		case "facing":
			restored.Facing = record.entity.Facing
		case "health":
			restored.Health = record.entity.Health
			restored.Alive = record.entity.Alive
		case "resources":
			restored.Resources = cloneStringIntMap(record.entity.Resources)
		case "statuses":
			restored.Statuses = cloneStatusMap(record.entity.Statuses)
		case "ability_state":
			restored.AbilityState = cloneRuntimeValueMap(record.entity.AbilityState)
		case "cooldowns":
			restored.Cooldowns = cloneStringTickMap(record.entity.Cooldowns)
		default:
			continue
		}
		applied = append(applied, field)
	}
	host.entities[command.Target] = restored
	if containsTemporalField(profile.Fields, "statuses") {
		host.replaceTemporalStatusesLocked(command.Target, record.statuses)
	}
	sort.Strings(applied)
	sort.Strings(skipped)
	beforeHealth := current.Health
	receipt := host.commitTemporalLocked("temporal_restored", command.Target, command.Context)
	if profile.EventPolicy == "derived_combat" && containsTemporalField(profile.Fields, "health") {
		derivedContext := command.Context
		derivedContext.Target = command.Target
		if restored.Health > beforeHealth {
			derivedContext.Result = "healed"
			host.appendContextEventLocked("heal_resolved", command.Target, 0, derivedContext)
		} else if restored.Health < beforeHealth {
			derivedContext.Result = "damaged"
			host.appendContextEventLocked("damage_resolved", command.Target, 0, derivedContext)
		}
	}
	return EffectResult{Commit: receipt, Payload: SnapshotRestoreEffectResult{ResultOutcome: successfulResultOutcome(), Applied: len(applied) > 0, AppliedFields: applied, SkippedFields: skipped}}, nil
}

func (host *MemoryHost) temporalRestoreRejectedLocked(command TemporalRestoreCommand, reason ExpectedFailureReason) EffectResult {
	context := command.Context
	context.Target = command.Target
	host.appendContextEventLocked("temporal_restore_rejected", command.Target, 0, context)
	return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SnapshotRestoreEffectResult{ResultOutcome: failedResultOutcome(reason)}}
}

func (host *MemoryHost) temporalProfileLocked(handle TemporalProfileHandle) (TemporalSnapshotProfile, bool) {
	for _, profile := range host.gameplay.Temporal.Entries {
		if profile.Handle == handle {
			profile.Fields = append([]string(nil), profile.Fields...)
			return profile, true
		}
	}
	return TemporalSnapshotProfile{}, false
}

func (host *MemoryHost) temporalSnapshotCountLocked(owner EntityID, programID string, profile TemporalProfileHandle) int {
	count := 0
	for _, record := range host.temporalSnapshots {
		if record.owner == owner && record.programID == programID && record.profile.Handle == profile && host.tick <= record.expiresTick {
			count++
		}
	}
	return count
}

func (host *MemoryHost) expireTemporalSnapshotsLocked() {
	keys := make([]uint64, 0)
	for token, record := range host.temporalSnapshots {
		if host.tick > record.expiresTick {
			keys = append(keys, token)
		}
	}
	sort.Slice(keys, func(left, right int) bool { return keys[left] < keys[right] })
	for _, token := range keys {
		delete(host.temporalSnapshots, token)
	}
}

func (host *MemoryHost) commitTemporalLocked(kind string, entity EntityID, context EventContext) CommitReceipt {
	host.revision++
	context.Target = entity
	host.appendContextEventLocked(kind, entity, 0, context)
	return CommitReceipt{Revision: host.revision, Changed: true}
}

func cloneTemporalProfile(profile TemporalSnapshotProfile) TemporalSnapshotProfile {
	profile.Fields = append([]string(nil), profile.Fields...)
	return profile
}

func cloneTemporalRestoreEntity(entity MemoryEntity) MemoryEntity {
	entity = cloneMemoryEntity(entity)
	entity.AbilityState = cloneRuntimeValueMap(entity.AbilityState)
	entity.Cooldowns = cloneStringTickMap(entity.Cooldowns)
	return entity
}

func captureTemporalEntity(entity MemoryEntity, fields []string) MemoryEntity {
	result := MemoryEntity{ID: entity.ID}
	for _, field := range fields {
		switch field {
		case "position":
			result.Position = entity.Position
		case "facing":
			result.Facing = entity.Facing
		case "health":
			result.Health, result.Alive = entity.Health, entity.Alive
		case "resources":
			result.Resources = cloneStringIntMap(entity.Resources)
		case "statuses":
			result.Statuses = cloneStatusMap(entity.Statuses)
		case "ability_state":
			result.AbilityState = cloneRuntimeValueMap(entity.AbilityState)
		case "cooldowns":
			result.Cooldowns = cloneStringTickMap(entity.Cooldowns)
		}
	}
	return result
}

func (host *MemoryHost) cloneStatusesForTemporalLocked(target EntityID, include bool) []statusInstance {
	if !include {
		return nil
	}
	result := make([]statusInstance, 0)
	for _, status := range host.statuses {
		if status.target == target {
			result = append(result, status)
		}
	}
	return result
}

func (host *MemoryHost) replaceTemporalStatusesLocked(target EntityID, snapshot []statusInstance) {
	result := make([]statusInstance, 0, len(host.statuses)+len(snapshot))
	for _, status := range host.statuses {
		if status.target != target {
			result = append(result, status)
		}
	}
	result = append(result, snapshot...)
	host.statuses = result
}

func containsTemporalField(fields []string, wanted string) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}
