package skill

type resultType string

const (
	resultTypeDamage            resultType = "damage_result"
	resultTypeHeal              resultType = "heal_result"
	resultTypeShield            resultType = "shield_result"
	resultTypeTeleport          resultType = "teleport_result"
	resultTypeStateChange       resultType = "state_change_result"
	resultTypeAbilityChange     resultType = "ability_change_result"
	resultTypeStatusOperation   resultType = "status_operation_result"
	resultTypeAttributeModifier resultType = "attribute_modifier_result"
	resultTypeSpawn             resultType = "spawn_result"
	resultTypeEntityCommand     resultType = "entity_command_result"
	resultTypeSnapshotCapture   resultType = "snapshot_capture_result"
	resultTypeSnapshotRestore   resultType = "snapshot_restore_result"
)

type ResultFieldHandle uint16

type resultFieldVisibility uint8

const (
	resultFieldBoth resultFieldVisibility = iota + 1
	resultFieldSuccess
	resultFieldFailure
)

type resultOutcomeScope uint8

const (
	resultOutcomeAny resultOutcomeScope = iota
	resultOutcomeSuccess
	resultOutcomeFailure
)

type resultFieldProgram struct {
	handle     ResultFieldHandle
	name       string
	typ        valueType
	visibility resultFieldVisibility
}

type resultLayoutProgram struct {
	typ             resultType
	fields          []resultFieldProgram
	allowedFailures []ExpectedFailureReason
}

func cloneResultLayout(layout resultLayoutProgram) resultLayoutProgram {
	result := layout
	result.fields = append([]resultFieldProgram(nil), layout.fields...)
	result.allowedFailures = append([]ExpectedFailureReason(nil), layout.allowedFailures...)
	return result
}

func (layout resultLayoutProgram) field(name string, outcome resultOutcomeScope) (resultFieldProgram, bool) {
	for _, field := range layout.fields {
		if field.name != name {
			continue
		}
		if field.visibility == resultFieldSuccess && outcome == resultOutcomeFailure || field.visibility == resultFieldFailure && outcome == resultOutcomeSuccess {
			return resultFieldProgram{}, false
		}
		return field, true
	}
	return resultFieldProgram{}, false
}

func (layout resultLayoutProgram) fieldByHandle(handle ResultFieldHandle) (resultFieldProgram, bool) {
	for _, field := range layout.fields {
		if field.handle == handle {
			return field, true
		}
	}
	return resultFieldProgram{}, false
}

func (layout resultLayoutProgram) allows(reason ExpectedFailureReason) bool {
	for _, allowed := range layout.allowedFailures {
		if allowed == reason {
			return true
		}
	}
	return false
}

func resultLayoutByType(typ resultType, dynamic valueType) resultLayoutProgram {
	combat := valueType{Base: valueKindInt, Quantity: quantityCombatAmount}
	count := valueType{Base: valueKindInt, Quantity: quantityCount}
	ticks := valueType{Base: valueKindInt, Quantity: quantityTicks}
	boolean := valueType{Base: valueKindBool}
	invalidTarget := []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailurePermissionDenied, ExpectedFailurePolicyRejected}
	fields := func(extra ...resultFieldProgram) []resultFieldProgram {
		result := []resultFieldProgram{{name: "succeeded", typ: boolean, visibility: resultFieldBoth}, {name: "failure_reason", typ: valueType{Base: valueKindString}, visibility: resultFieldBoth}}
		result = append(result, extra...)
		for index := range result {
			result[index].handle = ResultFieldHandle(index + 1)
		}
		return result
	}
	success := func(name string, value valueType) resultFieldProgram {
		return resultFieldProgram{name: name, typ: value, visibility: resultFieldSuccess}
	}
	both := func(name string, value valueType) resultFieldProgram {
		return resultFieldProgram{name: name, typ: value, visibility: resultFieldBoth}
	}
	layout := resultLayoutProgram{typ: typ}
	switch typ {
	case resultTypeDamage:
		layout.allowedFailures = invalidTarget
		layout.fields = fields(success("attempted", combat), success("mitigated", combat), success("absorbed", combat), success("health_damage", combat), success("critical", boolean), success("blocked", boolean), success("dodged", boolean), success("immune", boolean), success("parried", boolean), success("killed", boolean))
	case resultTypeHeal:
		layout.allowedFailures, layout.fields = invalidTarget, fields(success("attempted", combat), success("effective", combat))
	case resultTypeShield:
		layout.allowedFailures, layout.fields = invalidTarget, fields(success("added", combat))
	case resultTypeTeleport:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailureInvalidPosition, ExpectedFailureDestinationBlocked, ExpectedFailurePolicyRejected}
		layout.fields = fields(success("position", valueType{Base: valueKindPosition}))
	case resultTypeStatusOperation:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailurePolicyRejected, ExpectedFailurePermissionDenied, ExpectedFailureReferenceExpired}
		layout.fields = fields(success("applied", boolean), success("removed", boolean), success("immune", boolean), success("previous_stacks", count), success("current_stacks", count), success("removed_stacks", count), success("due_tick", ticks))
	case resultTypeAttributeModifier:
		layout.allowedFailures, layout.fields = invalidTarget, fields(success("applied", boolean), success("due_tick", ticks))
	case resultTypeStateChange, resultTypeAbilityChange:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailurePolicyRejected, ExpectedFailurePermissionDenied, ExpectedFailureReferenceExpired}
		layout.fields = fields(success("before", dynamic), success("after", dynamic), both("applied", boolean))
	case resultTypeSpawn:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidPosition, ExpectedFailureCapacityReached, ExpectedFailurePermissionDenied, ExpectedFailurePolicyRejected}
		layout.fields = fields(success("entities", valueType{Base: valueKindEntityList}), success("first_entity", valueType{Base: valueKindEntity}))
	case resultTypeEntityCommand:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailurePermissionDenied, ExpectedFailurePolicyRejected, ExpectedFailureReferenceExpired}
		layout.fields = fields(both("applied", boolean))
	case resultTypeSnapshotCapture:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailureCapacityReached, ExpectedFailurePermissionDenied, ExpectedFailurePolicyRejected}
		layout.fields = fields(success("token", valueType{Base: valueKindSnapshotToken}))
	case resultTypeSnapshotRestore:
		layout.allowedFailures = []ExpectedFailureReason{ExpectedFailureInvalidTarget, ExpectedFailureDestinationBlocked, ExpectedFailurePermissionDenied, ExpectedFailurePolicyRejected, ExpectedFailureReferenceExpired}
		layout.fields = fields(both("applied", boolean), success("applied_fields", valueType{Base: valueKindStringList}), success("skipped_fields", valueType{Base: valueKindStringList}))
	}
	return layout
}
