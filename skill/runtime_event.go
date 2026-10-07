package skill

// drainHostEvents 把 Host 新产生的事件派发给被动路由、记进 cast 与状态事件，并推进 eventCursor。它跟在已经提交的宿主
// 操作（付费、效果、衍生物步进、停止）之后，所以不返回错误：之前派发出错一路传成施法失败，付费路径上已付费、未提交的
// 启动被删、费用不退（RR-20261006-55）。被拒的候选由 dispatchEvent 告警后跳过；根事件表满时停在这个事件前，留给 tick 上的
// collectHostEvents 重试（它把容量错误报给 Advance 的调用方），之后的事件也等到那时一起派发，顺序不变。
func (runtime *Runtime) drainHostEvents(cast *castInstance) {
	events := runtime.host.Events(runtime.eventCursor)
	for _, event := range events {
		if err := runtime.dispatchEvent(event.Context); err != nil {
			break
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
