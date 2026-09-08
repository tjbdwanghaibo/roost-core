package skill

func cloneEventContext(context EventContext) EventContext {
	context.gameplayTags = append([]GameplayTagHandle(nil), context.gameplayTags...)
	return context
}

func cloneRuntimeEvents(events []RuntimeEvent) []RuntimeEvent {
	result := append([]RuntimeEvent(nil), events...)
	for index := range result {
		result[index].Context = cloneEventContext(result[index].Context)
		if result[index].State != nil {
			state := *result[index].State
			state.Before = cloneStateRuntimeValue(state.Before)
			state.After = cloneStateRuntimeValue(state.After)
			result[index].State = &state
		}
		if result[index].Ability != nil {
			ability := *result[index].Ability
			ability.Before = cloneStateRuntimeValue(ability.Before)
			ability.After = cloneStateRuntimeValue(ability.After)
			result[index].Ability = &ability
		}
		if result[index].Result != nil {
			typed := *result[index].Result
			result[index].Result = &typed
		}
	}
	return result
}

func cloneStateRuntimeValue(value RuntimeValue) RuntimeValue {
	return cloneRuntimeValue(value)
}
