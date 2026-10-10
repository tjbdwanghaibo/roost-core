package skill

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCompileRejectsPhaseEventsTheRuntimeNeverDispatches(t *testing.T) {
	finish := `{"flow":"finish","reason":"done"}`
	for _, event := range []string{"recast", "timeout"} {
		t.Run(event, func(t *testing.T) {
			input := strings.Replace(minimalSkillJSON, `"on":{"enter":`+finish+`}`, `"on":{"enter":`+finish+`,"`+event+`":`+finish+`}`, 1)
			if input == minimalSkillJSON {
				t.Fatal("fixture replacement failed")
			}
			program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			if program != nil {
				t.Fatalf("on.%s compiled into a Program the Runtime never runs it from; diagnostics=%#v", event, diagnostics)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticCapabilityUnknown, "$.phases[0].on."+event)
		})
	}
}

func TestPhaseTimeoutTicksDoNotExcuseEnterFallthrough(t *testing.T) {
	enter := `{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}}`
	timeout := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":5,"damage_type":"physical"}},{"flow":"finish","reason":"timeout"}]}`
	input := `{"schema":"roost.skill/v2","id":"skill.test.phase.timeout","name":"Timeout","description":"Phase timeout.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":3,"on":{"enter":` + enter + `,"timeout":` + timeout + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, input), environment)
	if program != nil {
		host := runtimeTestHost(environment)
		_, err := NewRuntime(host, RuntimeOptions{}).Activate(program, CastInput{Caster: 1, Target: 2})
		t.Fatalf("fallthrough enter with timeout_ticks compiled; Activate = %v (invariant=%v)", err, errors.Is(err, ErrProgramInvariant))
	}
	requireDiagnosticAt(t, diagnostics, DiagnosticLifecycleFallthrough, "$.phases[0].on.enter")

	// 只去掉 timeout 分支、enter 仍不结束：fallthrough 本身也不能再靠 timeout_ticks 通过。
	withoutHandler := strings.Replace(input, `,"timeout":`+timeout, ``, 1)
	if _, diagnostics := Compile(mustParseJSON(t, withoutHandler), environment); !diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("fallthrough enter excused by timeout_ticks: %#v", diagnostics)
	}
}

// 不依赖 fallthrough 的非零 timeout_ticks 仍可编译，但给出 warning：Runtime
// 没有 phase 计时，这个值不起作用。
func TestNonzeroPhaseTimeoutTicksWarnsThatItIsNotEnforced(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"timeout_ticks":0`, `"timeout_ticks":5`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	if program == nil || diagnosticsHaveErrors(diagnostics) {
		t.Fatalf("finishing phase with timeout_ticks rejected: %#v", diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == DiagnosticWarning && diagnostic.Path == "$.phases[0].timeout_ticks" {
			return
		}
	}
	t.Fatalf("missing timeout_ticks warning: %#v", diagnostics)
}

func requireDiagnosticAt(t *testing.T, diagnostics []Diagnostic, code DiagnosticCode, path string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Path == path && diagnostic.Severity == DiagnosticError {
			return
		}
	}
	t.Fatalf("missing error %s at %s in %#v", code, path, diagnostics)
}

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
