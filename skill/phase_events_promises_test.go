package skill

import (
	"reflect"
	"testing"
)

// B3 ②：phase 事件只在 phaseEventTable 里写一次。表必须覆盖定义能写的全部 on.* 事件（IR 与
// 线格式各一个字段），每一项的取值函数取的是同名字段；输入端口名就是对应的事件名。
// 有派发点的集合由表决定：lower 只导出它们，编译期拒绝其余的。
func TestPhaseEventTableIsTheSingleSource(t *testing.T) {
	if fields := reflect.TypeOf(phaseEventsIR{}).NumField(); fields != len(phaseEventTable) {
		t.Fatalf("phaseEventsIR has %d events, the table lists %d", fields, len(phaseEventTable))
	}
	if fields := reflect.TypeOf(PhaseEventsDefinition{}).NumField(); fields != len(phaseEventTable) {
		t.Fatalf("PhaseEventsDefinition has %d events, the table lists %d", fields, len(phaseEventTable))
	}
	sentinel := func(name string) flowIR { return &finishFlowIR{reason: name} }
	events := phaseEventsIR{
		enter: sentinel(phaseEventEnter), recast: sentinel(phaseEventRecast), cancel: sentinel(phaseEventCancel),
		directionChanged: sentinel(phaseEventDirectionChanged), targetChanged: sentinel(phaseEventTargetChanged),
		timeout: sentinel(phaseEventTimeout), release: sentinel(phaseEventRelease), pulse: sentinel(phaseEventPulse),
	}
	seen := map[string]bool{}
	for _, event := range phaseEventTable {
		if seen[event.name] {
			t.Fatalf("event %q listed twice", event.name)
		}
		seen[event.name] = true
		if got := event.flow(events).(*finishFlowIR).reason; got != event.name {
			t.Fatalf("table entry %q reads the %q field", event.name, got)
		}
	}
	dispatched := map[string]bool{}
	for _, flow := range dispatchedPhaseEventFlows(events) {
		dispatched[flow.name] = true
	}
	for _, flow := range undispatchedPhaseEventFlows(events) {
		if dispatched[flow.name] {
			t.Fatalf("event %q is both dispatched and refused", flow.name)
		}
		dispatched[flow.name] = true
	}
	if len(dispatched) != len(phaseEventTable) {
		t.Fatalf("dispatched + refused cover %d events, want %d", len(dispatched), len(phaseEventTable))
	}
	for _, port := range []InputPort{InputPortDirectionChanged, InputPortTargetChanged} {
		if !seen[string(port)] {
			t.Fatalf("input port %q is not a phase event: UpdateInput would look up a root lower never exported", port)
		}
	}
}
