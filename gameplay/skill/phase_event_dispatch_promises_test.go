package skill

import (
	"errors"
	"strings"
	"testing"
)

// RR-20261005-NC-151：编译器接受的 phase 事件必须是 Runtime 会派发的事件。
// Runtime 只派发 enter / cancel / release / pulse / direction_changed /
// target_changed，从不派发 recast 与 timeout，也从不为 timeout_ticks 排计时；
// 旧实现照样编译这些分支（运行时静默丢弃），并把 timeout_ticks > 0 当作
// enter fallthrough 的出口——tap 技能于是编译通过、每次施法 ErrProgramInvariant。
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
