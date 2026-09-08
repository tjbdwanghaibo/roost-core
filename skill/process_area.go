package skill

import "sort"

type AreaMemberState struct {
	MembershipTicks int64
	EnterCount      int64
}

func advanceAreaMembership(process *ProcessInstance, current []EntityID) []ProcessSignal {
	if process.AreaMembers == nil {
		process.AreaMembers = make(map[EntityID]AreaMemberState)
	}
	members := sortedUniqueEntityIDs(current)
	present := make(map[EntityID]bool, len(members))
	for _, member := range members {
		present[member] = true
	}

	left := make([]EntityID, 0)
	for member, state := range process.AreaMembers {
		if state.MembershipTicks > 0 && !present[member] {
			left = append(left, member)
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	signals := make([]ProcessSignal, 0, len(left)+len(members)*2)
	for _, member := range left {
		state := process.AreaMembers[member]
		signals = append(signals, areaMembershipSignal(ProcessSignalLeave, member, state))
		delete(process.AreaMembers, member)
	}
	for _, member := range members {
		state := process.AreaMembers[member]
		if state.MembershipTicks == 0 {
			state.EnterCount++
			state.MembershipTicks = 1
			process.AreaMembers[member] = state
			signals = append(signals, areaMembershipSignal(ProcessSignalEnter, member, state))
		} else {
			state.MembershipTicks++
			process.AreaMembers[member] = state
		}
	}
	for _, member := range members {
		signals = append(signals, areaMembershipSignal(ProcessSignalTick, member, process.AreaMembers[member]))
	}
	return signals
}

func stopAreaMembership(process *ProcessInstance, emitLeave bool) []ProcessSignal {
	if process == nil || len(process.AreaMembers) == 0 {
		return nil
	}
	leaves := make([]ProcessSignal, 0, len(process.AreaMembers))
	if emitLeave {
		members := make([]EntityID, 0, len(process.AreaMembers))
		for member, state := range process.AreaMembers {
			if state.MembershipTicks > 0 {
				members = append(members, member)
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		for _, member := range members {
			leaves = append(leaves, areaMembershipSignal(ProcessSignalLeave, member, process.AreaMembers[member]))
		}
	}
	clear(process.AreaMembers)
	return leaves
}

func sortedUniqueEntityIDs(values []EntityID) []EntityID {
	result := append([]EntityID(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	write := 0
	for _, value := range result {
		if value == 0 || write > 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func areaMembershipSignal(kind ProcessSignalKind, target EntityID, state AreaMemberState) ProcessSignal {
	return ProcessSignal{Kind: kind, Target: target, MembershipTicks: state.MembershipTicks, EnterCount: state.EnterCount}
}

func (runtime *Runtime) stepAreaMembership(cast *castInstance, process *ProcessInstance) ([]ProcessSignal, error) {
	if cast == nil || process == nil || process.Program == nil || int(process.TemplateIndex) >= len(process.Program.processTemplates) {
		return nil, ErrProgramInvariant
	}
	template := process.Program.processTemplates[process.TemplateIndex]
	if template.area == nil {
		return nil, nil
	}
	request, err := runtime.buildSelectRequest(cast, *template.area)
	if err != nil {
		return nil, err
	}
	result, err := runtime.host.Select(request)
	if err != nil {
		return nil, err
	}
	if result.Meta.Revision < request.Meta.RequiredRevision || result.Selection.elementType != selectionEntity || len(result.Selection.elements) > template.area.limit {
		return nil, ErrHostContractViolation
	}
	cast.visibleRevision = maxRevision(cast.visibleRevision, result.Meta.Revision)
	process.visibleRevision = cast.visibleRevision
	members := make([]EntityID, len(result.Selection.elements))
	for index, element := range result.Selection.elements {
		members[index] = element.entity
	}
	return advanceAreaMembership(process, members), nil
}

func areaProcessSignals(signals []ProcessSignal) []ProcessSignal {
	result := signals[:0]
	for _, signal := range signals {
		if signal.Kind != ProcessSignalLeave && signal.Kind != ProcessSignalEnter && signal.Kind != ProcessSignalTick {
			result = append(result, signal)
		}
	}
	return result
}
