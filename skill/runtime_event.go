package skill

func (runtime *Runtime) drainHostEvents(cast *castInstance) error {
	events := runtime.host.Events(runtime.eventCursor)
	for _, event := range events {
		if err := runtime.dispatchEvent(event.Context); err != nil {
			return err
		}
		if event.Cursor > runtime.eventCursor {
			runtime.eventCursor = event.Cursor
		}
		runtime.appendCastEvent(cast, event)
		runtime.recordStateEvent(event)
	}
	if compactor, ok := runtime.host.(HostEventCompactor); ok && runtime.eventCursor != 0 {
		compactor.CompactEventsThrough(runtime.eventCursor)
	}
	return nil
}

func (runtime *Runtime) appendCastEvent(cast *castInstance, event RuntimeEvent) {
	if cast == nil {
		return
	}
	cast.events = append(cast.events, cloneRuntimeEvent(event))
	if overflow := len(cast.events) - runtime.options.CastEventLimit; overflow > 0 {
		copy(cast.events, cast.events[overflow:])
		cast.events = cast.events[:runtime.options.CastEventLimit]
		cast.eventsDropped += uint64(overflow)
	}
}

func cloneRuntimeEvent(event RuntimeEvent) RuntimeEvent {
	event.Context = cloneEventContext(event.Context)
	if event.State != nil {
		state := *event.State
		state.Before = cloneStateRuntimeValue(state.Before)
		state.After = cloneStateRuntimeValue(state.After)
		event.State = &state
	}
	if event.Ability != nil {
		ability := *event.Ability
		ability.Before = cloneStateRuntimeValue(ability.Before)
		ability.After = cloneStateRuntimeValue(ability.After)
		event.Ability = &ability
	}
	if event.Result != nil {
		result := *event.Result
		event.Result = &result
	}
	return event
}
