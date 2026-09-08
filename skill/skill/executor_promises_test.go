package skill

import (
	"errors"
	"testing"
)

// U-0094 (C2): the executor's structural invariants guard against a Program
// whose tables no longer agree with each other (a corrupted or hand-built
// program). Each guard must surface as ErrProgramInvariant (or
// ErrAsyncFlowNotScheduled for a phase without an enter root) from Start
// instead of indexing out of range, and the failed cast must not stay active.
func TestStartRefusesEachCorruptedProgramShape(t *testing.T) {
	environment := abilityTestEnvironment()
	cases := []struct {
		name   string
		id     string
		flow   string
		tamper func(*Program)
		want   error
	}{
		{"initial phase beyond table", "phase_index", `{"flow":"finish"}`, func(p *Program) { p.initialPhase = PhaseIndex(len(p.phases)) }, ErrProgramInvariant},
		{"phase without enter root", "no_root", `{"flow":"finish"}`, func(p *Program) { p.phases[0].roots = nil }, ErrAsyncFlowNotScheduled},
		{"root operation missing", "nil_operation", `{"flow":"finish"}`, func(p *Program) {
			index, found := phaseRootOperation(p, p.phases[0], "enter")
			if !found {
				t.Fatal("fixture has no enter root")
			}
			p.operations[index] = nil
		}, ErrProgramInvariant},
		{"root operation beyond table", "operation_index", `{"flow":"finish"}`, func(p *Program) {
			index, _ := phaseRootOperation(p, p.phases[0], "enter")
			p.operations = p.operations[:index]
		}, ErrProgramInvariant},
		{"repeat count above computed limit", "repeat_limit", `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}}},{"flow":"finish"}]}`, func(p *Program) { p.limits.Repeat = 2 }, ErrProgramInvariant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			program := compileExecutorTestSkill(t, environment, tc.id, tc.flow)
			tc.tamper(program)
			runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
			castID, err := runtime.Start(program, CastInput{Caster: 1})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Start = %d, %v; want %v", castID, err, tc.want)
			}
			if runtime.ActiveCastCount() != 0 {
				t.Fatalf("a cast refused at start stayed active: %d", runtime.ActiveCastCount())
			}
			if castID != 0 {
				snapshot, ok := runtime.InspectCast(castID)
				if !ok || snapshot.Status != CastFailed {
					t.Fatalf("retained cast %d status = %+v, %v; want CastFailed", castID, snapshot.Status, ok)
				}
			}
		})
	}
}

func compileExecutorTestSkill(t *testing.T, environment CompileEnvironment, id, flow string) *Program {
	t.Helper()
	definition := `{"schema":"roost.skill/v2","id":"skill.test.executor.` + id + `","name":"Executor","description":"Executor invariants.","gameplay_tags":["spell"],"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{"counter":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	program, diagnostics := Compile(mustParseJSON(t, definition), environment)
	requireNoErrors(t, diagnostics)
	return program
}
