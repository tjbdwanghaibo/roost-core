package skill

import "sort"

type AreaMemberState struct {
	MembershipTicks int64
	EnterCount      int64
}

func advanceAreaMembership(spawn *SpawnInstance, current []EntityID) []SpawnSignal {
	if spawn.AreaMembers == nil {
		spawn.AreaMembers = make(map[EntityID]AreaMemberState)
	}
	members := sortedUniqueEntityIDs(current)
	present := make(map[EntityID]bool, len(members))
	for _, member := range members {
		present[member] = true
	}

	left := make([]EntityID, 0)
	for member, state := range spawn.AreaMembers {
		if state.MembershipTicks > 0 && !present[member] {
			left = append(left, member)
		}
	}
	sort.Slice(left, func(i, j int) bool { return left[i] < left[j] })
	signals := make([]SpawnSignal, 0, len(left)+len(members)*2)
	for _, member := range left {
		state := spawn.AreaMembers[member]
		signals = append(signals, areaMembershipSignal(SpawnSignalLeave, member, state))
		delete(spawn.AreaMembers, member)
	}
	for _, member := range members {
		state := spawn.AreaMembers[member]
		if state.MembershipTicks == 0 {
			state.EnterCount++
			state.MembershipTicks = 1
			spawn.AreaMembers[member] = state
			signals = append(signals, areaMembershipSignal(SpawnSignalEnter, member, state))
		} else {
			state.MembershipTicks++
			spawn.AreaMembers[member] = state
		}
	}
	for _, member := range members {
		signals = append(signals, areaMembershipSignal(SpawnSignalTick, member, spawn.AreaMembers[member]))
	}
	return signals
}

func stopAreaMembership(spawn *SpawnInstance, emitLeave bool) []SpawnSignal {
	if spawn == nil || len(spawn.AreaMembers) == 0 {
		return nil
	}
	leaves := make([]SpawnSignal, 0, len(spawn.AreaMembers))
	if emitLeave {
		members := make([]EntityID, 0, len(spawn.AreaMembers))
		for member, state := range spawn.AreaMembers {
			if state.MembershipTicks > 0 {
				members = append(members, member)
			}
		}
		sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
		for _, member := range members {
			leaves = append(leaves, areaMembershipSignal(SpawnSignalLeave, member, spawn.AreaMembers[member]))
		}
	}
	clear(spawn.AreaMembers)
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

func areaMembershipSignal(kind SpawnSignalKind, target EntityID, state AreaMemberState) SpawnSignal {
	return SpawnSignal{Kind: kind, Target: target, MembershipTicks: state.MembershipTicks, EnterCount: state.EnterCount}
}

func (runtime *Runtime) stepAreaMembership(cast *castInstance, spawn *SpawnInstance) ([]SpawnSignal, error) {
	if cast == nil || spawn == nil || spawn.Program == nil || int(spawn.TemplateIndex) >= len(spawn.Program.spawnTemplates) {
		return nil, ErrProgramInvariant
	}
	template := spawn.Program.spawnTemplates[spawn.TemplateIndex]
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
	spawn.visibleRevision = cast.visibleRevision
	members := make([]EntityID, len(result.Selection.elements))
	for index, element := range result.Selection.elements {
		members[index] = element.entity
	}
	return advanceAreaMembership(spawn, members), nil
}

func areaSpawnSignals(signals []SpawnSignal) []SpawnSignal {
	result := signals[:0]
	for _, signal := range signals {
		if signal.Kind != SpawnSignalLeave && signal.Kind != SpawnSignalEnter && signal.Kind != SpawnSignalTick {
			result = append(result, signal)
		}
	}
	return result
}
