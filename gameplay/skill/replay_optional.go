package skill

// 调试包装器必须记录实际可选调用，不能只转发能力表。底层未提供可选能力时，
// 保持 Runtime 未发现接口时的默认行为；召唤事务缺失则明确拒绝。
func (host *RecordingHost) PreviewOwnedSummon(command SummonCommand) (OwnedSummonPreview, error) {
	before := host.host.CurrentRevision()
	var result OwnedSummonPreview
	err := ErrHostContractViolation
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		result, err = owned.PreviewOwnedSummon(command)
	}
	host.append("preview_owned_summon", command, result, err, before)
	return result, err
}

type ownedEntityResult struct {
	Metadata OwnedEntityMetadata
	Found    bool
}

func (host *RecordingHost) OwnedEntity(entity EntityID) (OwnedEntityMetadata, bool) {
	before := host.host.CurrentRevision()
	var result ownedEntityResult
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		result.Metadata, result.Found = owned.OwnedEntity(entity)
	}
	host.append("owned_entity", entity, result, nil, before)
	return result.Metadata, result.Found
}

func (host *RecordingHost) CommitOwnedSummon(id OwnedSummonTransactionID) error {
	before := host.host.CurrentRevision()
	err := ErrHostContractViolation
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		err = owned.CommitOwnedSummon(id)
	}
	host.append("commit_owned_summon", id, nil, err, before)
	return err
}

func (host *RecordingHost) RollbackOwnedSummon(id OwnedSummonTransactionID) error {
	before := host.host.CurrentRevision()
	err := ErrHostContractViolation
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		err = owned.RollbackOwnedSummon(id)
	}
	host.append("rollback_owned_summon", id, nil, err, before)
	return err
}

func (host *RecordingHost) RemoveOwnedEntitiesByProgram(programID string) error {
	before := host.host.CurrentRevision()
	var err error
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		err = owned.RemoveOwnedEntitiesByProgram(programID)
	}
	host.append("remove_owned_program", programID, nil, err, before)
	return err
}

func (host *RecordingHost) RemoveOwnedEntitiesForMatchEnd() error {
	before := host.host.CurrentRevision()
	var err error
	if owned, ok := host.host.(OwnedEntityRuntimeHost); ok {
		err = owned.RemoveOwnedEntitiesForMatchEnd()
	}
	host.append("remove_owned_match", nil, nil, err, before)
	return err
}

func (host *RecordingHost) CompactEventsThrough(cursor EventCursor) {
	before := host.host.CurrentRevision()
	if compactor, ok := host.host.(HostEventCompactor); ok {
		compactor.CompactEventsThrough(cursor)
	}
	host.append("compact_events", cursor, nil, nil, before)
}

type relationRequest struct{ Viewer, Owner EntityID }
type relationResult struct {
	Relation string
	Found    bool
}

func (host *RecordingHost) AbilityOwnerRelation(viewer, owner EntityID) (string, bool) {
	before := host.host.CurrentRevision()
	result := relationResult{Found: true}
	if provider, ok := host.host.(AbilityRelationProvider); ok {
		result.Relation, result.Found = provider.AbilityOwnerRelation(viewer, owner)
	}
	host.append("ability_relation", relationRequest{viewer, owner}, result, nil, before)
	return result.Relation, result.Found
}

func (host *RecordingHost) SkillPersistentStateSnapshot() []PersistentStateSnapshot {
	before := host.host.CurrentRevision()
	var result []PersistentStateSnapshot
	if provider, ok := host.host.(RuntimeStateExtensionProvider); ok {
		result = provider.SkillPersistentStateSnapshot()
	}
	host.append("persistent_state_snapshot", nil, result, nil, before)
	return result
}

type inputPositionResult struct {
	Position Position
	Allowed  bool
}

func (host *RecordingHost) ResolveInputPosition(request InputPositionRequest) (Position, bool) {
	before := host.host.CurrentRevision()
	result := inputPositionResult{Position: request.Position, Allowed: true}
	if resolver, ok := host.host.(InputPositionResolver); ok {
		result.Position, result.Allowed = resolver.ResolveInputPosition(request)
	}
	host.append("resolve_input_position", request, result, nil, before)
	return result.Position, result.Allowed
}

func (host *ReplayHost) PreviewOwnedSummon(command SummonCommand) (OwnedSummonPreview, error) {
	record := host.nextRecord("preview_owned_summon", command)
	return record.Result.(OwnedSummonPreview), record.Err
}
func (host *ReplayHost) OwnedEntity(entity EntityID) (OwnedEntityMetadata, bool) {
	result := host.next("owned_entity", entity).(ownedEntityResult)
	return result.Metadata, result.Found
}
func (host *ReplayHost) CommitOwnedSummon(id OwnedSummonTransactionID) error {
	return host.nextRecord("commit_owned_summon", id).Err
}
func (host *ReplayHost) RollbackOwnedSummon(id OwnedSummonTransactionID) error {
	return host.nextRecord("rollback_owned_summon", id).Err
}
func (host *ReplayHost) RemoveOwnedEntitiesByProgram(programID string) error {
	return host.nextRecord("remove_owned_program", programID).Err
}
func (host *ReplayHost) RemoveOwnedEntitiesForMatchEnd() error {
	return host.nextRecord("remove_owned_match", nil).Err
}
func (host *ReplayHost) CompactEventsThrough(cursor EventCursor) {
	host.nextRecord("compact_events", cursor)
}
func (host *ReplayHost) AbilityOwnerRelation(viewer, owner EntityID) (string, bool) {
	result := host.next("ability_relation", relationRequest{viewer, owner}).(relationResult)
	return result.Relation, result.Found
}
func (host *ReplayHost) SkillPersistentStateSnapshot() []PersistentStateSnapshot {
	return host.next("persistent_state_snapshot", nil).([]PersistentStateSnapshot)
}
func (host *ReplayHost) ResolveInputPosition(request InputPositionRequest) (Position, bool) {
	result := host.next("resolve_input_position", request).(inputPositionResult)
	return result.Position, result.Allowed
}
