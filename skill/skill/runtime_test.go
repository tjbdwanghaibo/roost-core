package skill

import (
	"errors"
	"testing"
)

func TestRuntimeActivateExecutesTypedDamage(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(7)})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, ok := runtime.InspectCast(castID)
	if !ok || snapshot.Status != CastFinished {
		t.Fatalf("unexpected cast: %#v", snapshot)
	}
	if got := host.HealthForTest(2); got != 90 {
		t.Fatalf("health = %d", got)
	}
}

func TestRuntimeExecutesSequenceRepeatIfAndMemory(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"set_memory","name":"counter","value":1}},{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}}},{"flow":"if","condition":{"op":"eq","args":["$memory.counter",4]},"then":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":5,"damage_type":"physical"}},"else":{"flow":"finish"}},{"flow":"finish"}]}`
	// Use a compact skill to keep replacement independent of fixture indentation.
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.memory","name":"Runtime","description":"Runtime flow.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{"counter":{"type":"int","default":0}},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	definition := mustParseJSON(t, json)
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(definition, environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(9)})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if got := host.HealthForTest(2); got != 95 {
		t.Fatalf("health = %d", got)
	}
}

func TestRuntimeRejectsAuthorityMismatchWithoutSideEffects(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	host.authority = AuthorityIdentity{Revision: "different", Digest: "different"}
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(1)})
	revision, events := host.CurrentRevision(), len(host.Events(0))
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrAuthorityMismatch) {
		t.Fatalf("got %v", err)
	}
	if runtime.CastCount() != 0 || host.CurrentRevision() != revision || len(host.Events(0)) != events || host.HealthForTest(2) != 100 {
		t.Fatal("authority mismatch caused side effects")
	}
}

func TestRuntimeDoesNotRetainOrInterpretDefinition(t *testing.T) {
	definition := mustParseFixture(t, "simple_damage.json")
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(definition, environment)
	requireNoErrors(t, diagnostics)
	definition.Phases = nil
	definition.InputSchema = NoneInputSchemaDefinition{}
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(3)})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 90 {
		t.Fatal("runtime behavior changed after definition mutation")
	}
}

func TestRuntimeRejectsCompilerSemanticsAndInputBeforeSideEffects(t *testing.T) {
	program, environment := compileRuntimeFixture(t, "simple_damage.json")
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{SupportedCompilerSemanticsRevision: "unsupported"})
	revision := host.CurrentRevision()
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrProgramSemanticsMismatch) {
		t.Fatalf("semantics mismatch = %v", err)
	}
	if runtime.CastCount() != 0 || host.CurrentRevision() != revision {
		t.Fatal("semantics mismatch caused side effects")
	}

	runtime = NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); !errors.Is(err, ErrCastInputInvalid) {
		t.Fatalf("input mismatch = %v", err)
	}
	if runtime.CastCount() != 0 || host.CurrentRevision() != revision {
		t.Fatal("input mismatch caused side effects")
	}
}

func TestRuntimeSelectEachUsesScopedLocals(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":100},"filters":[{"type":"alive"},{"type":"not_caster"}],"order":{"by":"distance","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"enemy","do":{"flow":"effect","effect":{"type":"damage","target":"$local.enemy","amount":1,"damage_type":"physical"}}},"on_empty":{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}}},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.select","name":"Runtime","description":"Runtime select.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100, Position: Position{X: 1}})
	runtime := NewRuntime(host, RuntimeOptions{MatchSeed: fixedTestSeed(4)})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if host.HealthForTest(2) != 99 || host.HealthForTest(3) != 99 {
		t.Fatalf("select each did not hit stable candidates: h2=%d h3=%d", host.HealthForTest(2), host.HealthForTest(3))
	}
}

func TestRuntimeKeepsEarlierCommitWhenLaterCommandFails(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"add","amount":10}},{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":1,"damage_type":"physical"}},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.partial","name":"Runtime","description":"Runtime partial commit.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(&effectResultHost{MemoryHost: host, err: ErrHostContractViolation}, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 999})
	if !errors.Is(err, ErrHostContractViolation) {
		t.Fatalf("later failure = %v", err)
	}
	if host.ResourceForTest(1, "mana") != 110 {
		t.Fatal("later failure rolled back earlier resource commit")
	}
	if snapshot, ok := runtime.InspectCast(castID); !ok || snapshot.Status != CastFailed {
		t.Fatalf("failed cast snapshot = %#v, %v", snapshot, ok)
	}
}

func TestRuntimePaysCostsBeforeCreatingCast(t *testing.T) {
	json := stringsReplaceOnce(t, string(mustReadFixture(t, "simple_damage.json")), `"costs": []`, `"costs":[{"resource":"mana","amount":5}]`)
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if got := host.ResourceForTest(1, "mana"); got != 95 {
		t.Fatalf("mana = %d", got)
	}

	host = runtimeTestHost(environment)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 4}})
	runtime = NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); !errors.Is(err, ErrInsufficientResource) {
		t.Fatalf("insufficient cost = %v", err)
	}
	if runtime.CastCount() != 0 || host.ResourceForTest(1, "mana") != 4 || host.HealthForTest(2) != 100 {
		t.Fatal("failed cost caused cast or world side effects")
	}
}

func TestRuntimeParallelBranchesCommitInDeclarationOrder(t *testing.T) {
	flow := `{"flow":"sequence","steps":[{"flow":"parallel","branches":[{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"set","amount":1}},{"flow":"effect","effect":{"type":"resource","target":"$caster","resource":"mana","operation":"set","amount":2}}]},{"flow":"finish"}]}`
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.parallel","name":"Runtime","description":"Runtime parallel order.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"none"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast","phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":` + flow + `}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if got := host.ResourceForTest(1, "mana"); got != 2 {
		t.Fatalf("parallel branch order produced mana=%d", got)
	}
}

func TestRuntimeGotoEntersTargetPhase(t *testing.T) {
	json := `{"schema":"roost.skill/v2","id":"skill.test.runtime.goto","name":"Runtime","description":"Runtime goto.","activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"first","phases":[{"id":"first","timeout_ticks":0,"on":{"enter":{"flow":"goto","phase":"second"}}},{"id":"second","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}},{"flow":"finish"}]}}}]}`
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseJSON(t, json), environment)
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(environment)
	runtime := NewRuntime(host, RuntimeOptions{})
	castID, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := runtime.InspectCast(castID)
	if snapshot.CurrentPhase != 1 || host.HealthForTest(2) != 98 {
		t.Fatalf("goto result: cast=%#v health=%d", snapshot, host.HealthForTest(2))
	}
}

func compileRuntimeFixture(t *testing.T, name string) (*Program, CompileEnvironment) {
	t.Helper()
	environment := DefaultCompileEnvironment()
	program, diagnostics := Compile(mustParseFixture(t, name), environment)
	requireNoErrors(t, diagnostics)
	return program, environment
}

func runtimeTestHost(environment CompileEnvironment) *MemoryHost {
	host := NewMemoryHost(AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
	return host
}

func fixedTestSeed(value byte) [32]byte {
	var seed [32]byte
	seed[0] = value
	return seed
}

func stringsReplaceOnce(t *testing.T, input, old, replacement string) string {
	t.Helper()
	index := -1
	for offset := 0; offset+len(old) <= len(input); offset++ {
		if input[offset:offset+len(old)] == old {
			index = offset
			break
		}
	}
	if index < 0 {
		t.Fatalf("substring %q not found", old)
	}
	return input[:index] + replacement + input[index+len(old):]
}
