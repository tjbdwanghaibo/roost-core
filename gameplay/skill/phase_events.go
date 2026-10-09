package skill

// phase 根事件的名字，以及 Runtime 会派发其中哪些——lower 导出 root、编译期拒绝没有派发点的
// 事件、Runtime 的派发点共用这一张表（维护者决定 B3 ②）。
//
// 之前三处各写一份：lower 的 phaseEventFlows 列 8 个事件，编译期在 requireDispatchedPhaseEvents
// 里逐个点名 recast / timeout（NC-151），Runtime 在 executeCast / Cancel / Release / 脉冲 /
// UpdateInput 里各写字符串字面量。给 Runtime 加或去一个派发点时，只改其中一处就会出现
// “编译通过却永远不执行”的分支（NC-150 / NC-151 的形状）。
const (
	phaseEventEnter            = "enter"
	phaseEventRecast           = "recast"
	phaseEventCancel           = "cancel"
	phaseEventDirectionChanged = "direction_changed"
	phaseEventTargetChanged    = "target_changed"
	phaseEventTimeout          = "timeout"
	phaseEventRelease          = "release"
	phaseEventPulse            = "pulse"
)

// phaseEventTable 列出定义里可以写的全部 phase 事件（on.* 的键），顺序就是 lower 导出 root
// 的顺序。dispatched 表示 Runtime 有派发点：enter（executeCast）、cancel（Cancel）、
// release（Release / 自动释放）、pulse（policy 脉冲）、direction_changed / target_changed
// （UpdateInput 的输入端口，端口名就是事件名）。dispatched 为 false 的事件编译期拒绝。
var phaseEventTable = []struct {
	name       string
	dispatched bool
	flow       func(phaseEventsIR) flowIR
}{
	{phaseEventEnter, true, func(events phaseEventsIR) flowIR { return events.enter }},
	{phaseEventRecast, false, func(events phaseEventsIR) flowIR { return events.recast }},
	{phaseEventCancel, true, func(events phaseEventsIR) flowIR { return events.cancel }},
	{phaseEventDirectionChanged, true, func(events phaseEventsIR) flowIR { return events.directionChanged }},
	{phaseEventTargetChanged, true, func(events phaseEventsIR) flowIR { return events.targetChanged }},
	{phaseEventTimeout, false, func(events phaseEventsIR) flowIR { return events.timeout }},
	{phaseEventRelease, true, func(events phaseEventsIR) flowIR { return events.release }},
	{phaseEventPulse, true, func(events phaseEventsIR) flowIR { return events.pulse }},
}

type namedFlow struct {
	name string
	flow flowIR
}

// dispatchedPhaseEventFlows 返回 Runtime 会派发的事件及其 flow（可能为 nil），按表的顺序。
func dispatchedPhaseEventFlows(events phaseEventsIR) []namedFlow {
	flows := make([]namedFlow, 0, len(phaseEventTable))
	for _, event := range phaseEventTable {
		if event.dispatched {
			flows = append(flows, namedFlow{event.name, event.flow(events)})
		}
	}
	return flows
}

// undispatchedPhaseEventFlows 返回定义里写了、但 Runtime 没有派发点的事件。
func undispatchedPhaseEventFlows(events phaseEventsIR) []namedFlow {
	var flows []namedFlow
	for _, event := range phaseEventTable {
		if flow := event.flow(events); !event.dispatched && flow != nil {
			flows = append(flows, namedFlow{event.name, flow})
		}
	}
	return flows
}
