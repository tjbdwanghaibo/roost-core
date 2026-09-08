package skill

import "sort"

func (host *MemoryHost) PreviewOwnedSpawn(command SpawnCommand) (OwnedSpawnPreview, error) {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	template, found := host.unitTemplate(command.Template)
	if !found {
		return OwnedSpawnPreview{}, ErrCombatHandleInvalid
	}
	remove, capacity := host.ownedReplacementPlan(command, template)
	if !capacity {
		return OwnedSpawnPreview{FailureReason: ExpectedFailureCapacityReached}, nil
	}
	return OwnedSpawnPreview{ReplacedEntities: append([]EntityID(nil), remove...), FailureReason: ExpectedFailureNone}, nil
}

func (host *MemoryHost) spawnOwnedLocked(command SpawnCommand) (EffectResult, error) {
	if command.Owner == 0 || command.GameplayDigest == "" || command.SourceSkillID == "" || command.SourceCastID == 0 {
		return EffectResult{}, ErrHostContractViolation
	}
	owner, ownerFound := host.entities[command.Owner]
	if !ownerFound || !owner.Alive {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SpawnEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
	}
	template, found := host.unitTemplate(command.Template)
	if !found {
		return EffectResult{}, ErrCombatHandleInvalid
	}
	if command.Count <= 0 || command.Count > template.MaximumSpawnCount || command.DurationTicks <= 0 || command.DurationTicks > template.MaximumLifetimeTicks {
		return EffectResult{}, ErrHostContractViolation
	}
	attributes, parameters, failure, err := host.resolveSpawnBindings(template, command)
	if err != nil {
		return EffectResult{}, err
	}
	if failure != ExpectedFailureNone {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SpawnEffectResult{ResultOutcome: failedResultOutcome(failure)}}, nil
	}
	remove, capacity := host.ownedReplacementPlan(command, template)
	if !capacity {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: SpawnEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureCapacityReached)}}, nil
	}
	transactionID := OwnedSpawnTransactionID(0)
	transaction := ownedSpawnTransaction{}
	if command.Transactional {
		host.nextOwnedTransaction++
		transactionID = host.nextOwnedTransaction
		transaction.replaced = make(map[EntityID]OwnedEntityMetadata, len(remove))
		transaction.entities = make(map[EntityID]MemoryEntity, len(remove))
		for _, entity := range remove {
			transaction.replaced[entity] = cloneOwnedEntityMetadata(host.ownedEntities[entity])
			transaction.entities[entity] = cloneMemoryEntity(host.entities[entity])
		}
	}
	for _, entity := range remove {
		host.removeOwnedLocked(entity, "owned_entity_replaced")
	}
	entities := make([]EntityID, command.Count)
	for index := 0; index < command.Count; index++ {
		entity := host.nextEntity
		host.nextEntity++
		host.nextOwnedSequence++
		due := saturatingTickAdd(host.tick, command.DurationTicks)
		metadata := OwnedEntityMetadata{Entity: entity, Owner: command.Owner, GameplayDigest: command.GameplayDigest, SourceSkillID: command.SourceSkillID, SourceCastID: command.SourceCastID, SourceEffectIndex: command.SourceEffectIndex, Template: command.Template, GameplayTags: append([]GameplayTagHandle(nil), template.GameplayTags...), SpawnTick: host.tick, SpawnSequence: host.nextOwnedSequence, LifetimeTicks: command.DurationTicks, DueTick: due, ControlProfile: template.ControlProfile, ParameterBindings: cloneRuntimeValueMap(parameters)}
		host.ownedEntities[entity] = metadata
		tags := make(map[GameplayTagHandle]bool, len(metadata.GameplayTags))
		for _, tag := range metadata.GameplayTags {
			tags[tag] = true
		}
		host.entities[entity] = MemoryEntity{ID: entity, Owner: command.Owner, Alive: true, Position: command.Position, TeamID: owner.TeamID, Resources: map[string]int64{}, Statuses: map[StatusHandle]bool{}, Attributes: cloneAttributeMap(attributes), GameplayTags: tags}
		host.commitLocked("owned_entity_spawned", entity, 0)
		entities[index] = entity
	}
	if transactionID != 0 {
		transaction.created = append([]EntityID(nil), entities...)
		host.ownedTransactions[transactionID] = transaction
	}
	return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Value: EntityRuntimeValue(entities[0]), Payload: SpawnEffectResult{ResultOutcome: successfulResultOutcome(), Entities: entities, FirstEntity: entities[0], TransactionID: transactionID}}, nil
}

func (host *MemoryHost) CommitOwnedSpawn(transactionID OwnedSpawnTransactionID) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if transactionID == 0 {
		return ErrHostContractViolation
	}
	if _, found := host.ownedTransactions[transactionID]; !found {
		return ErrHostContractViolation
	}
	delete(host.ownedTransactions, transactionID)
	return nil
}

func (host *MemoryHost) RollbackOwnedSpawn(transactionID OwnedSpawnTransactionID) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	transaction, found := host.ownedTransactions[transactionID]
	if transactionID == 0 || !found {
		return ErrHostContractViolation
	}
	for _, entity := range transaction.created {
		delete(host.ownedEntities, entity)
		delete(host.entities, entity)
	}
	for entity, record := range transaction.replaced {
		host.ownedEntities[entity] = cloneOwnedEntityMetadata(record)
		host.entities[entity] = cloneMemoryEntity(transaction.entities[entity])
	}
	delete(host.ownedTransactions, transactionID)
	host.commitLocked("owned_entity_spawn_rolled_back", 0, 0)
	return nil
}

func cloneMemoryEntity(entity MemoryEntity) MemoryEntity {
	entity.Resources = cloneStringIntMap(entity.Resources)
	entity.Statuses = cloneStatusMap(entity.Statuses)
	entity.Attributes = cloneAttributeMap(entity.Attributes)
	entity.ElementMultipliers = cloneElementMultiplierMap(entity.ElementMultipliers)
	entity.GameplayTags = cloneGameplayTagSet(entity.GameplayTags)
	return entity
}

func (host *MemoryHost) resolveSpawnBindings(template UnitTemplateCatalogEntry, command SpawnCommand) (map[AttributeHandle]int64, map[string]RuntimeValue, ExpectedFailureReason, error) {
	attributePolicies := make(map[AttributeHandle]UnitTemplateAttributeOverridePolicy, len(template.AllowedAttributeOverrides))
	for _, policy := range template.AllowedAttributeOverrides {
		attributePolicies[policy.Attribute] = policy
	}
	attributes := make(map[AttributeHandle]int64, len(command.AttributeOverrides))
	for _, override := range command.AttributeOverrides {
		policy, allowed := attributePolicies[override.Attribute]
		if !allowed {
			return nil, nil, ExpectedFailurePolicyRejected, nil
		}
		if _, duplicate := attributes[override.Attribute]; duplicate {
			return nil, nil, ExpectedFailureNone, ErrHostContractViolation
		}
		minimum, maximum := policy.Minimum, policy.Maximum
		for _, attribute := range host.gameplay.Attributes.Entries {
			if attribute.Handle == override.Attribute {
				minimum = max(minimum, attribute.Minimum)
				maximum = min(maximum, attribute.Maximum)
				break
			}
		}
		if minimum > maximum {
			return nil, nil, ExpectedFailureNone, ErrHostContractViolation
		}
		attributes[override.Attribute] = clampInt64(override.Value, minimum, maximum)
	}
	parameterPolicies := make(map[string]UnitTemplateParameterPolicy, len(template.Parameters))
	for _, policy := range template.Parameters {
		parameterPolicies[policy.Name] = policy
	}
	parameters := make(map[string]RuntimeValue, len(command.ParameterBindings))
	for _, binding := range command.ParameterBindings {
		policy, allowed := parameterPolicies[binding.Name]
		if !allowed {
			return nil, nil, ExpectedFailurePolicyRejected, nil
		}
		if _, duplicate := parameters[binding.Name]; duplicate || !binding.Value.Present() || binding.Value.typ.Base != policy.ValueType || policy.ValueType == valueKindInt && binding.Value.typ.Quantity != policy.Quantity {
			return nil, nil, ExpectedFailureNone, ErrHostContractViolation
		}
		value := cloneRuntimeValue(binding.Value)
		if integer, ok := value.Int(); ok {
			value = IntRuntimeValue(clampInt64(integer, policy.Minimum, policy.Maximum), policy.Quantity)
		}
		parameters[binding.Name] = value
	}
	_, hasStart := parameters["start_position"]
	_, hasEnd := parameters["end_position"]
	if hasStart != hasEnd || hasStart && !template.DynamicCollider {
		return nil, nil, ExpectedFailurePolicyRejected, nil
	}
	return attributes, parameters, ExpectedFailureNone, nil
}

func clampInt64(value, minimum, maximum int64) int64 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func cloneRuntimeValueMap(values map[string]RuntimeValue) map[string]RuntimeValue {
	if values == nil {
		return nil
	}
	result := make(map[string]RuntimeValue, len(values))
	for key, value := range values {
		result[key] = cloneRuntimeValue(value)
	}
	return result
}

func (host *MemoryHost) ownedReplacementPlan(command SpawnCommand, template UnitTemplateCatalogEntry) ([]EntityID, bool) {
	ownerRecords := host.ownedRecordsLocked(func(record OwnedEntityMetadata) bool {
		return record.Owner == command.Owner && record.Template == command.Template
	})
	sourceRecords := host.ownedRecordsLocked(func(record OwnedEntityMetadata) bool {
		return record.Owner == command.Owner && record.Template == command.Template && record.SourceSkillID == command.SourceSkillID
	})
	teamRecords := host.ownedRecordsLocked(func(record OwnedEntityMetadata) bool {
		return record.Template == command.Template && host.sameOwnedTeam(command.Owner, record.Owner)
	})
	ownerExcess := len(ownerRecords) + command.Count - template.MaximumPerOwner
	sourceExcess := 0
	if template.MaximumPerSourceSkill > 0 {
		sourceExcess = len(sourceRecords) + command.Count - template.MaximumPerSourceSkill
	}
	teamExcess := 0
	if template.MaximumPerTeam > 0 {
		teamExcess = len(teamRecords) + command.Count - template.MaximumPerTeam
	}
	if ownerExcess <= 0 && sourceExcess <= 0 && teamExcess <= 0 {
		return nil, true
	}
	if template.ReplacementPolicy == "reject_new" {
		return nil, false
	}
	candidateByID := make(map[EntityID]OwnedEntityMetadata)
	for _, records := range [][]OwnedEntityMetadata{ownerRecords, sourceRecords, teamRecords} {
		for _, record := range records {
			candidateByID[record.Entity] = record
		}
	}
	candidates := make([]OwnedEntityMetadata, 0, len(candidateByID))
	for _, record := range candidateByID {
		candidates = append(candidates, record)
	}
	sortOwnedReplacementCandidates(candidates, template.ReplacementPolicy, command.Position, host.entities)
	result := make([]EntityID, 0)
	for _, record := range candidates {
		matchesOwner := ownerExcess > 0 && record.Owner == command.Owner
		matchesSource := sourceExcess > 0 && record.Owner == command.Owner && record.SourceSkillID == command.SourceSkillID
		matchesTeam := teamExcess > 0 && host.sameOwnedTeam(command.Owner, record.Owner)
		if !matchesOwner && !matchesSource && !matchesTeam {
			continue
		}
		result = append(result, record.Entity)
		if matchesOwner {
			ownerExcess--
		}
		if matchesSource {
			sourceExcess--
		}
		if matchesTeam {
			teamExcess--
		}
		if ownerExcess <= 0 && sourceExcess <= 0 && teamExcess <= 0 {
			return result, true
		}
	}
	return nil, false
}

func (host *MemoryHost) sameOwnedTeam(left, right EntityID) bool {
	leftEntity, leftFound := host.entities[left]
	rightEntity, rightFound := host.entities[right]
	if !leftFound || !rightFound {
		return false
	}
	if leftEntity.TeamID == 0 || rightEntity.TeamID == 0 {
		return left == right
	}
	return leftEntity.TeamID == rightEntity.TeamID
}

func sortOwnedReplacementCandidates(records []OwnedEntityMetadata, policy string, position Position, entities map[EntityID]MemoryEntity) {
	sort.SliceStable(records, func(left, right int) bool {
		a, b := records[left], records[right]
		switch policy {
		case "replace_newest":
			if a.SpawnTick != b.SpawnTick {
				return a.SpawnTick > b.SpawnTick
			}
			if a.SpawnSequence != b.SpawnSequence {
				return a.SpawnSequence > b.SpawnSequence
			}
		case "replace_nearest", "replace_farthest":
			da := distanceSquared(entities[a.Entity].Position, position)
			db := distanceSquared(entities[b.Entity].Position, position)
			if da != db {
				if policy == "replace_farthest" {
					return da > db
				}
				return da < db
			}
			if a.SpawnSequence != b.SpawnSequence {
				return a.SpawnSequence < b.SpawnSequence
			}
		default:
			if a.SpawnTick != b.SpawnTick {
				return a.SpawnTick < b.SpawnTick
			}
			if a.SpawnSequence != b.SpawnSequence {
				return a.SpawnSequence < b.SpawnSequence
			}
		}
		return a.Entity < b.Entity
	})
}

func (host *MemoryHost) commandOwnedLocked(command OwnedEntityCommand) (EffectResult, error) {
	record, found := host.ownedEntities[command.Target]
	if !found {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: EntityCommandEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureReferenceExpired)}}, nil
	}
	if record.Owner != command.Owner || record.GameplayDigest != command.GameplayDigest {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: EntityCommandEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePermissionDenied)}}, nil
	}
	template, found := host.unitTemplate(record.Template)
	if !found {
		return EffectResult{}, ErrHostContractViolation
	}
	if !containsString(template.Commands, command.Command) || command.Command == "invoke_behavior" && !containsString(template.Behaviors, command.Behavior) {
		return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: EntityCommandEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailurePolicyRejected)}}, nil
	}
	if !validOwnedCommandPayload(command) {
		return EffectResult{}, ErrHostContractViolation
	}
	entity := host.entities[command.Target]
	switch command.Command {
	case "move_to":
		entity.Position = command.Position
		host.entities[command.Target] = entity
	case "follow", "attack_target":
		if target, ok := host.entities[command.TargetEntity]; !ok || !target.Alive {
			return EffectResult{Commit: CommitReceipt{Revision: host.revision}, Payload: EntityCommandEffectResult{ResultOutcome: failedResultOutcome(ExpectedFailureInvalidTarget)}}, nil
		}
	case "return_to_owner":
		entity.Position = host.entities[record.Owner].Position
		host.entities[command.Target] = entity
	case "despawn":
		host.removeOwnedLocked(command.Target, "owned_entity_despawned")
		return EffectResult{Commit: CommitReceipt{Revision: host.revision, Changed: true}, Payload: EntityCommandEffectResult{ResultOutcome: successfulResultOutcome(), Applied: true}}, nil
	}
	receipt := host.commitLocked("owned_entity_commanded", command.Target, 0)
	return EffectResult{Commit: receipt, Payload: EntityCommandEffectResult{ResultOutcome: successfulResultOutcome(), Applied: true}}, nil
}

func validOwnedCommandPayload(command OwnedEntityCommand) bool {
	zeroPosition := command.Position == (Position{})
	switch command.Command {
	case "move_to":
		return command.TargetEntity == 0 && command.Behavior == ""
	case "follow", "attack_target":
		return zeroPosition && command.TargetEntity != 0 && command.Behavior == ""
	case "invoke_behavior":
		return zeroPosition && command.TargetEntity == 0 && command.Behavior != ""
	case "hold_position", "return_to_owner", "stop", "despawn":
		return zeroPosition && command.TargetEntity == 0 && command.Behavior == ""
	default:
		return false
	}
}

func (host *MemoryHost) removeOwnedLocked(entity EntityID, event string) {
	delete(host.ownedEntities, entity)
	delete(host.entities, entity)
	host.commitLocked(event, entity, 0)
}

func (host *MemoryHost) expireOwnedLocked() {
	ids := make([]EntityID, 0)
	for entity, record := range host.ownedEntities {
		template, _ := host.unitTemplate(record.Template)
		owner, ownerFound := host.entities[record.Owner]
		if record.DueTick <= host.tick || template.OwnerDeathPolicy == "despawn" && (!ownerFound || !owner.Alive) {
			ids = append(ids, entity)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, entity := range ids {
		host.removeOwnedLocked(entity, "owned_entity_expired")
	}
}

func (host *MemoryHost) RemoveOwnedEntitiesByProgram(programID string) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	ids := make([]EntityID, 0)
	for entity, record := range host.ownedEntities {
		template, _ := host.unitTemplate(record.Template)
		if record.SourceSkillID == programID && template.SkillRemovedPolicy == "despawn" {
			ids = append(ids, entity)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, entity := range ids {
		host.removeOwnedLocked(entity, "owned_entity_skill_removed")
	}
	return nil
}

func (host *MemoryHost) RemoveOwnedEntitiesForMatchEnd() error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	ids := make([]EntityID, 0)
	for entity, record := range host.ownedEntities {
		template, _ := host.unitTemplate(record.Template)
		if template.MatchEndPolicy == "despawn" {
			ids = append(ids, entity)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, entity := range ids {
		host.removeOwnedLocked(entity, "owned_entity_match_ended")
	}
	return nil
}

func (host *MemoryHost) ownedRecordsLocked(include func(OwnedEntityMetadata) bool) []OwnedEntityMetadata {
	result := make([]OwnedEntityMetadata, 0)
	for _, record := range host.ownedEntities {
		if include(record) {
			result = append(result, cloneOwnedEntityMetadata(record))
		}
	}
	return result
}

func (host *MemoryHost) unitTemplate(handle UnitTemplateHandle) (UnitTemplateCatalogEntry, bool) {
	for _, entry := range host.gameplay.UnitTemplates.Entries {
		if entry.Handle == handle {
			return entry, true
		}
	}
	return UnitTemplateCatalogEntry{}, false
}

func (host *MemoryHost) OwnedEntities(owner EntityID) []OwnedEntityMetadata {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	result := host.ownedRecordsLocked(func(record OwnedEntityMetadata) bool { return owner == 0 || record.Owner == owner })
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].SpawnSequence != result[j].SpawnSequence {
			return result[i].SpawnSequence < result[j].SpawnSequence
		}
		return result[i].Entity < result[j].Entity
	})
	return result
}

func (host *MemoryHost) OwnedEntity(entity EntityID) (OwnedEntityMetadata, bool) {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	record, found := host.ownedEntities[entity]
	return cloneOwnedEntityMetadata(record), found
}

func cloneOwnedEntityMetadata(record OwnedEntityMetadata) OwnedEntityMetadata {
	record.GameplayTags = append([]GameplayTagHandle(nil), record.GameplayTags...)
	record.ParameterBindings = cloneRuntimeValueMap(record.ParameterBindings)
	return record
}
