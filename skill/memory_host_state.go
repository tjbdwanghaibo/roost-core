package skill

import (
	"sort"
)

func (host *MemoryHost) SkillPersistentStateSnapshot() []PersistentStateSnapshot {
	host.mutex.RLock()
	defer host.mutex.RUnlock()
	result := make([]PersistentStateSnapshot, 0, len(host.states))
	for key, record := range host.states {
		result = append(result, PersistentStateSnapshot{Handle: key.handle, Binding: key.binding, Value: cloneStateRuntimeValue(record.value), DueTick: record.dueTick, Sequence: record.sequence, ClearOn: append([]string(nil), record.clearOn...)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result
}

func (host *MemoryHost) ReadState(request StateReadRequest) (StateReadResult, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(request.Meta.RequiredRevision); err != nil {
		return StateReadResult{}, err
	}
	key := memoryStateKey{handle: request.Handle, binding: request.Binding}
	record, found := host.states[key]
	if !found {
		return StateReadResult{Meta: QueryResultMeta{Revision: host.revision}, Value: cloneStateRuntimeValue(request.Default)}, nil
	}
	if entity, ok := record.value.Entity(); ok {
		entityState, exists := host.entities[entity]
		if !exists || !entityState.Alive {
			delete(host.states, key)
			host.commitStateEventLocked("state_cleared", key, record.value, MissingRuntimeValue(record.value.typ), "entity_invalid", record.event)
			return StateReadResult{Meta: QueryResultMeta{Revision: host.revision}, Value: cloneStateRuntimeValue(request.Default)}, nil
		}
	}
	return StateReadResult{Meta: QueryResultMeta{Revision: host.revision}, Value: cloneStateRuntimeValue(record.value), Present: true}, nil
}

func (host *MemoryHost) ModifyState(command StateMutationCommand) (StateMutationResult, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if err := host.requireRevisionLocked(command.Meta.RequiredRevision); err != nil {
		return StateMutationResult{}, err
	}
	if err := validateStateBinding(command.Scope, command.Binding); err != nil {
		return StateMutationResult{}, err
	}
	key := memoryStateKey{handle: command.Handle, binding: command.Binding}
	record, exists := host.states[key]
	before := cloneStateRuntimeValue(command.Default)
	if exists {
		before = cloneStateRuntimeValue(record.value)
	}
	if command.Operation == "clear" {
		if !exists {
			return StateMutationResult{ResultOutcome: successfulResultOutcome(), Commit: CommitReceipt{Revision: host.revision}, Before: before, After: before}, nil
		}
		delete(host.states, key)
		after := MissingRuntimeValue(before.typ)
		receipt := host.commitStateEventLocked("state_cleared", key, before, after, "explicit", command.Event)
		return StateMutationResult{ResultOutcome: successfulResultOutcome(), Commit: receipt, Before: before, After: after}, nil
	}
	after, err := applyStateOperation(before, command.Value, command.Operation, command.Minimum, command.Maximum)
	if err != nil {
		return StateMutationResult{}, err
	}
	duration := command.DurationTicks
	if command.MaximumDurationTicks > 0 && duration > command.MaximumDurationTicks {
		duration = command.MaximumDurationTicks
	}
	if duration <= 0 {
		return StateMutationResult{}, ErrProgramInvariant
	}
	due := host.tick + duration
	if exists {
		switch command.ExpiryPolicy {
		case "keep":
			due = record.dueTick
		case "extend":
			due = record.dueTick + duration
			maximumDue := host.tick + command.MaximumDurationTicks
			if command.MaximumDurationTicks > 0 && due > maximumDue {
				due = maximumDue
			}
		}
	}
	host.nextStateSequence++
	host.states[key] = memoryStateRecord{value: cloneStateRuntimeValue(after), dueTick: due, sequence: host.nextStateSequence, clearOn: append([]string(nil), command.ClearOn...), event: cloneEventContext(command.Event)}
	receipt := host.commitStateEventLocked("state_changed", key, before, after, command.Operation, command.Event)
	return StateMutationResult{ResultOutcome: successfulResultOutcome(), Commit: receipt, Before: before, After: after}, nil
}

func validateStateBinding(scope StateScope, binding StateScopeBinding) error {
	switch scope {
	case StateScopeOwner:
		if binding.Owner == 0 || binding.Subject != 0 || binding.Team != 0 {
			return ErrCastInputInvalid
		}
	case StateScopeOwnerTarget:
		if binding.Owner == 0 || binding.Subject == 0 || binding.Team != 0 {
			return ErrCastInputInvalid
		}
	case StateScopeTeam:
		if binding.Team == 0 || binding.Owner != 0 || binding.Subject != 0 {
			return ErrCastInputInvalid
		}
	case StateScopeMatch:
		if binding != (StateScopeBinding{}) {
			return ErrCastInputInvalid
		}
	default:
		return ErrCastInputInvalid
	}
	return nil
}

func applyStateOperation(before, operand RuntimeValue, operation string, minimum, maximum int64) (RuntimeValue, error) {
	var result RuntimeValue
	var err error
	switch operation {
	case "set":
		if before.typ.Base != operand.typ.Base || !operand.Present() {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		result = cloneStateRuntimeValue(operand)
	case "add":
		result, err = CheckedAddRuntimeValues(before, operand)
	case "mul_bp":
		result, err = CheckedScaleBPRuntimeValue(before, operand)
	case "min", "max":
		left, leftOK := before.Int()
		right, rightOK := operand.Int()
		if !leftOK || !rightOK {
			return RuntimeValue{}, ErrRuntimeTypeMismatch
		}
		if operation == "min" && right < left || operation == "max" && right > left {
			left = right
		}
		result = IntRuntimeValue(left, before.typ.Quantity)
	default:
		return RuntimeValue{}, ErrProgramInvariant
	}
	if err != nil {
		return RuntimeValue{}, err
	}
	if integer, ok := result.Int(); ok {
		if integer < minimum {
			integer = minimum
		}
		if maximum >= minimum && integer > maximum {
			integer = maximum
		}
		result = IntRuntimeValue(integer, result.typ.Quantity)
	}
	return result, nil
}

func (host *MemoryHost) expireStatesLocked() {
	type dueState struct {
		key    memoryStateKey
		record memoryStateRecord
	}
	due := make([]dueState, 0)
	for key, record := range host.states {
		if record.dueTick <= host.tick {
			due = append(due, dueState{key: key, record: record})
		}
	}
	sort.Slice(due, func(left, right int) bool {
		if due[left].record.dueTick != due[right].record.dueTick {
			return due[left].record.dueTick < due[right].record.dueTick
		}
		return due[left].record.sequence < due[right].record.sequence
	})
	for _, item := range due {
		current, found := host.states[item.key]
		if !found || current.sequence != item.record.sequence {
			continue
		}
		delete(host.states, item.key)
		host.commitStateEventLocked("state_expired", item.key, current.value, MissingRuntimeValue(current.value.typ), "ttl", current.event)
	}
}

func (host *MemoryHost) ClearStateLifecycle(reason string, owner, subject EntityID) int {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	type selected struct {
		key    memoryStateKey
		record memoryStateRecord
	}
	items := make([]selected, 0)
	for key, record := range host.states {
		if !containsString(record.clearOn, reason) {
			continue
		}
		if reason == "owner_death" && key.binding.Owner != owner || reason == "target_death" && key.binding.Subject != subject {
			continue
		}
		items = append(items, selected{key: key, record: record})
	}
	sort.Slice(items, func(left, right int) bool { return items[left].record.sequence < items[right].record.sequence })
	for _, item := range items {
		delete(host.states, item.key)
		host.commitStateEventLocked("state_cleared", item.key, item.record.value, MissingRuntimeValue(item.record.value.typ), reason, item.record.event)
	}
	return len(items)
}

func (host *MemoryHost) commitStateEventLocked(kind string, key memoryStateKey, before, after RuntimeValue, reason string, context EventContext) CommitReceipt {
	host.revision++
	host.nextCursor++
	context.Tick = host.tick
	context.WorldRevision = host.revision
	change := &StateChangeEvent{Handle: key.handle, Binding: key.binding, Before: cloneStateRuntimeValue(before), After: cloneStateRuntimeValue(after), Reason: reason}
	host.events = append(host.events, RuntimeEvent{Cursor: host.nextCursor, Revision: host.revision, Tick: host.tick, Kind: kind, Entity: key.binding.Owner, Context: cloneEventContext(context), State: change})
	return CommitReceipt{Revision: host.revision, Changed: true}
}
