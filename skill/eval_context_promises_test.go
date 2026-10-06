package skill

import (
	"errors"
	"strings"
	"testing"
)

// 本文件钉住 N09 第六批按求值上下文表（eval_contexts.go）找出的四处“编译器认可、Runtime
// 在另一个上下文里求不出”（RR-20261005-NC-280～283）。四处都是表做出来之后，逐格对照
// Runtime 的求值点才看见的：之前编译期只有施法作用域与回调作用域两套名字，memory 默认值、
// 状态默认值、采样点、投射引用各自按“施法作用域”检查。

func evalContextStateJSON(t *testing.T, input, stateType, stateDefault, steps string) string {
	t.Helper()
	text := agreementSkillJSON(input, "{}", "[]", steps)
	state := `"persistent_state":{"who":{"type":"` + stateType + `","scope":"owner","default":` + stateDefault + `,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}},"initial_phase"`
	return stringsReplaceOnce(t, text, `"initial_phase"`, state)
}

// evalContextAreaSpawn 是一个 area 衍生物：启动时与之后每个 interval 都跑 tick 回调。
func evalContextAreaSpawn(tick string) string {
	return `{"flow":"effect","effect":{"type":"summon","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"spawn":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"tick":` + tick + `}}`
}

// runEvalContextCast 编译、施法并推进 6 个 tick，返回第一个错误。
func runEvalContextCast(t *testing.T, program *Program, host *MemoryHost, input CastInput) error {
	t.Helper()
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, input); err != nil {
		return err
	}
	for tick := Tick(1); tick <= 6; tick++ {
		if err := runtime.Advance(tick); err != nil {
			return err
		}
	}
	return nil
}

// RR-20261005-NC-280：memory 默认值在 Activate 里按槽位顺序（名字排序）求值。旧的类型检查
// 在填 memory 类型表的同一个 map 循环里按施法作用域检查默认值：另一个 memory 的名字在不在
// 作用域里取决于 map 遍历顺序——同一份定义有时编译通过、有时报 REFERENCE_UNKNOWN；
// 编译通过且被读的槽位排在后面时，每次 Activate 都 ErrRuntimeTypeMismatch（读到未初始化的
// 槽位）。表的 memory_default 列没有 `$memory.*`：两种顺序每次都拒绝，诊断指向表项。
func TestCompileRejectsMemoryDefaultsReadingMemoryDeterministically(t *testing.T) {
	heal := agreementSteps(agreementEffect(`{"type":"heal","target":"$memory.a","amount":1}`), agreementEffect(`{"type":"heal","target":"$memory.b","amount":1}`))
	for name, memory := range map[string]string{
		"earlier slot reads a later one":  `{"a":{"type":"entity","default":"$memory.b"},"b":{"type":"entity","default":"$caster"}}`,
		"later slot reads an earlier one": `{"a":{"type":"entity","default":"$caster"},"b":{"type":"entity","default":"$memory.a"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := "$.memory.a.default"
			if strings.Contains(memory, `"b":{"type":"entity","default":"$memory.a"}`) {
				path = "$.memory.b.default"
			}
			input := agreementSkillJSON("none", memory, "[]", heal)
			for attempt := 0; attempt < 40; attempt++ {
				program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
				if program != nil {
					err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1})
					t.Fatalf("attempt %d compiled (cast error %v); the compile result must not depend on map order", attempt, err)
				}
				requireDiagnosticAt(t, diagnostics, DiagnosticInputUnavailable, path)
				requireDiagnosticMentions(t, diagnostics, path, "memory_default")
			}
		})
	}
}

// RR-20261005-NC-280 的控制：memory 默认值读施法者、施法输入、施法状态照常编译并执行。
func TestMemoryDefaultsReadingTheCastStillRun(t *testing.T) {
	memory := `{"who":{"type":"entity","default":"$input.target"},"self":{"type":"entity","default":"$caster"},"tap":{"type":"bool","default":{"op":"eq","args":["$cast.mode","tap"]}}}`
	steps := agreementSteps(`{"flow":"if","condition":"$memory.tap","then":`+captureDamage("$memory.who", "3")+`}`, agreementEffect(`{"type":"heal","target":"$memory.self","amount":1}`))
	program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("entity", memory, "[]", steps)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(DefaultCompileEnvironment())
	if err := runEvalContextCast(t, program, host, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if health := host.HealthForTest(2); health != 97 {
		t.Fatalf("target health = %d, want 97", health)
	}
}

// RR-20261005-NC-281：持久状态的默认值在读 / 写这条状态的位置求值（evalStateRead /
// executeStateMutation 用当时的 cast）。旧的类型检查按施法作用域检查默认值：`$input.target`
// 这样的默认值能编译，状态在 spawn 衍生物回调里被读时，回调的 cast 没有施法输入，
// Activate（衍生物启动那一步就跑回调）返回 ErrProgramInvariant。
func TestCompileRejectsStateDefaultsThatCannotBeEvaluatedEverywhere(t *testing.T) {
	readInCallback := agreementSteps(evalContextAreaSpawn(`{"flow":"if","condition":{"op":"exists","args":[{"read_state":{"state":"who","owner":"$owner"}}]},"then":` + captureDamage("$event.target", "1") + `}`))
	cases := []struct{ name, input, memory string }{
		{"cast input", "entity", "{}"},
		{"cast memory", "none", `{"m":{"type":"entity","default":"$caster"}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			stateDefault := `"$input.target"`
			if test.memory != "{}" {
				stateDefault = `"$memory.m"`
			}
			text := strings.Replace(evalContextStateJSON(t, test.input, "entity", stateDefault, readInCallback), `"memory":{}`, `"memory":`+test.memory, 1)
			program, diagnostics := Compile(mustParseJSON(t, text), DefaultCompileEnvironment())
			if program != nil {
				err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1, Target: 2})
				t.Fatalf("state default %s compiled; casting it: %v", stateDefault, err)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticInputUnavailable, "$.persistent_state.who.default")
			requireDiagnosticMentions(t, diagnostics, "$.persistent_state.who.default", "state_default")
		})
	}
}

// RR-20261005-NC-281 的控制：默认值读 `$caster` 的状态在施法流程与衍生物回调里都能读（回调里
// `$caster` 求成衍生物的 owner，即同一个施法者），施法流程里写过之后读到的是写入值。
func TestStateDefaultOfTheCasterRunsInCallbacks(t *testing.T) {
	steps := agreementSteps(evalContextAreaSpawn(`{"flow":"if","condition":{"read_state":{"state":"who","owner":"$owner"}},"then":` + captureDamage("$event.target", "1") + `}`))
	program, diagnostics := Compile(mustParseJSON(t, evalContextStateJSON(t, "none", "bool", `{"op":"eq","args":["$caster","$caster"]}`, steps)), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(DefaultCompileEnvironment())
	if err := runEvalContextCast(t, program, host, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	if health := host.HealthForTest(2); health >= 100 {
		t.Fatalf("the callback must read the default (true): target health = %d", health)
	}
}

// RR-20261005-NC-282：cast_start / phase_start 的整个读取（包括实体）在采样点求值。读取处的
// exists 守卫在采样点不存在：实体是可缺省的引用（`$primary_target`、null 默认值的 memory）
// 时，旧实现编译通过，缺省时 Activate（cast_start）或进 phase（phase_start）就
// ErrRuntimeTypeMismatch——作者写了守卫，施法仍然整个失败。
func TestCompileRejectsOptionalEntitiesInCachedReads(t *testing.T) {
	guarded := func(reference, point string) string {
		return `{"flow":"if","condition":{"op":"exists","args":["` + reference + `"]},"then":` + captureDamage(reference, captureRead(reference, point)) + `}`
	}
	entityPath := agreementEnterPath + ".then.effect.amount.read_attribute.entity"
	for _, point := range []string{"cast_start", "phase_start"} {
		for name, input := range map[string]string{
			"primary target": agreementSkillJSON("none", "{}", "[]", agreementSteps(guarded("$primary_target", point))),
			"null memory":    agreementSkillJSON("none", `{"who":{"type":"entity","default":null}}`, "[]", agreementSteps(guarded("$memory.who", point))),
		} {
			t.Run(point+" "+name, func(t *testing.T) {
				program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
				if program != nil {
					err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1})
					t.Fatalf("%s read of an optional entity compiled; casting without it: %v", point, err)
				}
				requireDiagnosticAt(t, diagnostics, DiagnosticAttributeSnapshotInvalid, entityPath)
			})
		}
	}
}

// RR-20261005-NC-282 的控制：同样的守卫读取用 current 照常执行；非可缺省的 memory 照常用
// cast_start。
func TestGuardedOptionalReadsAtCurrentStillRun(t *testing.T) {
	guarded := `{"flow":"if","condition":{"op":"exists","args":["$primary_target"]},"then":` + captureDamage("$primary_target", captureRead("$primary_target", "current")) + `}`
	program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(guarded))), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
	memory := `{"who":{"type":"entity","default":"$caster"}}`
	program, diagnostics = Compile(mustParseJSON(t, agreementSkillJSON("none", memory, "[]", agreementSteps(captureDamage("$memory.who", captureRead("$memory.who", "cast_start"))))), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	if err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
}

// RR-20261005-NC-283：类型检查按根投射引用（`$primary_target.position` 从实体根投射出位置），
// lower 却把整串当 builtin 名字（输入槽位则按全名查）。Runtime 的 builtin 分支没有这些名字：
// `$primary_target.position`、`$lifecycle_entity.position`、`$event.*.position` 每次求值
// ErrProgramInvariant；`$input.target.position` 从 B3 起报 LOWER_UNRESOLVED（定位在 `$`）。
// 修复后 lower 按表拆成根 + field，由 evalReference 的 field 分支取位置。
func TestProjectedReferencesCompileAndRun(t *testing.T) {
	teleportTo := func(destination string) string {
		return agreementEffect(`{"type":"teleport","target":"$caster","destination":"` + destination + `","on_blocked":"fail"}`)
	}
	for name, steps := range map[string]string{
		"$input.target.position":   teleportTo("$input.target.position"),
		"$primary_target.position": `{"flow":"if","condition":{"op":"exists","args":["$primary_target"]},"then":` + teleportTo("$primary_target.position") + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(steps))), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
			host := runtimeTestHost(DefaultCompileEnvironment())
			host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Position: Position{X: 3, Y: 4}})
			if err := runEvalContextCast(t, program, host, CastInput{Caster: 1, Target: 2}); err != nil {
				t.Fatal(err)
			}
			read, err := host.Read(ReadRequest{Payload: PositionRead{Entity: 1}})
			if err != nil {
				t.Fatal(err)
			}
			if position, _ := read.Value.Position(); position != (Position{X: 3, Y: 4}) {
				t.Fatalf("caster position = %+v, want the target's position", position)
			}
		})
	}
	for _, reference := range []string{"$lifecycle_entity.position", "$event.target.position", "$event.source.position", "$event.owner.position"} {
		t.Run(reference, func(t *testing.T) {
			callback := `{"flow":"effect","effect":{"type":"knockback","target":"$event.target","from":"` + reference + `","distance":1}}`
			program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(evalContextAreaSpawn(callback)))), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
			if err := runEvalContextCast(t, program, runtimeTestHost(DefaultCompileEnvironment()), CastInput{Caster: 1}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// 表外引用在 Runtime 报 ErrReferenceOutOfContext 并点名上下文与表项，不再落到
// ErrProgramInvariant（维护者第五轮决定）。用移交后的衍生物上下文直接求一个施法输入引用。
func TestRuntimeReportsOutOfContextReferencesAgainstTheTable(t *testing.T) {
	program, _ := compileRuntimeJSON(t, agreementSkillJSON("none", "{}", "[]", agreementSteps(evalContextAreaSpawn(captureDamage("$event.target", "1")))))
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{})
	cast := &castInstance{program: program, caster: 1, evalContext: evalSpawnStep}
	_, err := runtime.evalReference(cast, referenceProgramValue{kind: referenceInput, index: 0, typ: evalEntityType})
	if !errors.Is(err, ErrReferenceOutOfContext) || errors.Is(err, ErrProgramInvariant) {
		t.Fatalf("err = %v, want ErrReferenceOutOfContext (not ErrProgramInvariant)", err)
	}
	for _, want := range []string{"spawn_step", "$input.*"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name %q", err, want)
		}
	}
}

func requireDiagnosticMentions(t *testing.T, diagnostics []Diagnostic, path, text string) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Path == path && strings.Contains(diagnostic.Message, text) {
			return
		}
	}
	t.Fatalf("no diagnostic at %s mentions %q: %#v", path, text, diagnostics)
}
