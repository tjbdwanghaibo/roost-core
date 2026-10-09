package skill

import (
	"errors"
	"testing"
)

// U-0134 · C2（空洞测试）· nightly gap map core `skill` 9/20。
//
// 执行器：分支条件求出非布尔值是 ErrRuntimeTypeMismatch 而不是当 false；重复体
// 报错或提前 finish 必须终止循环并把结果原样交回（同步循环与按间隔调度的单次
// 迭代各一份）；查询操作指向表外选择器、调度回来的迭代任务局部槽越界 / 迭代数
// 越界都是 ErrProgramInvariant。内存宿主：位置 / 属性读取对不存在的实体报
// ErrEntityNotFound；伤害命令在目录声明了别的公式策略或未声明该伤害类型时拒绝。

func findOperation[T operation](t *testing.T, program *Program) (OperationIndex, T) {
	t.Helper()
	for index, op := range program.operations {
		if typed, ok := op.(T); ok {
			return OperationIndex(index), typed
		}
	}
	var zero T
	t.Fatalf("program has no %T operation", zero)
	return 0, zero
}

// nonBoolCondition rewrites the first branch's condition into an int literal.
func nonBoolCondition(t *testing.T, program *Program) {
	t.Helper()
	index, branch := findOperation[branchOperation](t, program)
	branch.condition = intProgramValue{value: 1}
	program.operations[index] = branch
}

func TestExecutorRefusesNonBoolConditionsAndStopsRepeatOnBodyOutcome(t *testing.T) {
	environment := abilityTestEnvironment()
	const branch = `{"flow":"sequence","steps":[{"flow":"if","condition":{"op":"eq","args":[1,1]},"then":{"flow":"finish"}},{"flow":"finish"}]}`
	program := compileExecutorTestSkill(t, environment, "branch_type", branch)
	nonBoolCondition(t, program)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1}); !errors.Is(err, ErrRuntimeTypeMismatch) {
		t.Fatalf("Start with an int branch condition = %v, want ErrRuntimeTypeMismatch", err)
	}

	// 重复体出错：错误必须穿出循环。
	const repeatingBranch = `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"if","condition":{"op":"eq","args":[1,1]},"then":{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}}}},{"flow":"finish"}]}`
	program = compileExecutorTestSkill(t, environment, "repeat_error", repeatingBranch)
	nonBoolCondition(t, program)
	runtime = NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1}); !errors.Is(err, ErrRuntimeTypeMismatch) {
		t.Fatalf("Start with a failing repeat body = %v, want ErrRuntimeTypeMismatch", err)
	}

	// 重复体提前 finish：循环停在第一轮，counter 是 1 不是 3。
	const finishingRepeat = `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"add_memory","name":"counter","value":1}},{"flow":"finish"}]}},{"flow":"finish"}]}`
	program = compileExecutorTestSkill(t, environment, "repeat_finish", finishingRepeat)
	runtime = NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	castID, err := runtime.Start(program, CastInput{Caster: 1})
	if err != nil {
		t.Fatal(err)
	}
	cast, ok := runtime.casts[castID]
	if !ok {
		t.Fatalf("cast %d not retained", castID)
	}
	if counter, ok := cast.memory[0].Int(); !ok || counter != 1 {
		t.Fatalf("repeat kept running after the body finished: counter=%v ok=%v status=%v", counter, ok, cast.status)
	}
}

func TestExecutorRefusesSelectorsAndIterationTasksOutsideTheProgram(t *testing.T) {
	environment := abilityTestEnvironment()
	const selecting = `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"ability","shape":{"type":"ability_set"},"filters":[{"type":"ability_tag","tag":"spell"}],"order":{"by":"ability_slot","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"ability","then":{"flow":"finish"}}},{"flow":"finish"}]}`
	program := compileExecutorTestSkill(t, environment, "selector_index", selecting)
	index, query := findOperation[queryOperation](t, program)
	query.selector = SelectorIndex(len(program.selectors))
	program.operations[index] = query
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	if _, err := runtime.Start(program, CastInput{Caster: 1}); !errors.Is(err, ErrProgramInvariant) {
		t.Fatalf("Start with a selector beyond the table = %v, want ErrProgramInvariant", err)
	}

	// 调度回来的单次迭代：局部槽越界 / 迭代数越界拒绝；体 finish 或出错时不再排下一轮。
	const repeating = `{"flow":"sequence","steps":[{"flow":"repeat","times":3,"index_as":"i","do":{"flow":"if","condition":{"op":"eq","args":[1,1]},"then":{"flow":"finish"}}},{"flow":"finish"}]}`
	program = compileExecutorTestSkill(t, environment, "iteration_task", repeating)
	_, repeat := findOperation[repeatOperation](t, program)
	finishIndex, _ := findOperation[finishOperation](t, program)
	runtime = NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	cast := &castInstance{id: 1, program: program, caster: 1, locals: make([]RuntimeValue, len(program.locals)), memory: make([]RuntimeValue, len(program.memory))}
	if _, err := runtime.executeRepeatIteration(cast, &repeatIterationTask{CastID: 1, Body: finishIndex, IndexLocal: LocalIndex(len(cast.locals)), Iteration: 0, Times: 3}); !errors.Is(err, ErrProgramInvariant) {
		t.Fatalf("iteration task with a local slot beyond the frame = %v", err)
	}
	if _, err := runtime.executeRepeatIteration(cast, &repeatIterationTask{CastID: 1, Body: finishIndex, IndexLocal: repeat.indexLocal, Iteration: 3, Times: 3}); !errors.Is(err, ErrProgramInvariant) {
		t.Fatalf("iteration task past its count = %v", err)
	}
	control, err := runtime.executeRepeatIteration(cast, &repeatIterationTask{CastID: 1, Body: finishIndex, IndexLocal: repeat.indexLocal, Iteration: 0, Times: 3})
	if err != nil || control.kind != flowFinish {
		t.Fatalf("iteration whose body finished = (%v, %v), want flowFinish and no further scheduling", control.kind, err)
	}
	nonBoolCondition(t, program)
	if _, err := runtime.executeRepeatIteration(cast, &repeatIterationTask{CastID: 1, Body: repeat.body, IndexLocal: repeat.indexLocal, Iteration: 0, Times: 3}); !errors.Is(err, ErrRuntimeTypeMismatch) {
		t.Fatalf("iteration whose body failed = %v, want the body's error", err)
	}
}

func TestMemoryHostReadsAndDamageRefuseUnknownEntitiesAndForeignCatalogs(t *testing.T) {
	host := NewMemoryHost(AuthorityIdentity{Revision: "test", Digest: "test"})
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
	if _, err := host.Read(ReadRequest{Payload: ResourceRead{Entity: 99, Resource: "mana"}}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("ResourceRead of an unknown entity = %v", err)
	}
	if _, err := host.Read(ReadRequest{Payload: PositionRead{Entity: 99}}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("PositionRead of an unknown entity = %v", err)
	}
	if _, err := host.Read(ReadRequest{Payload: AttributeRead{Entity: 99, Attribute: 1}}); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("AttributeRead of an unknown entity = %v", err)
	}
	if _, err := host.Read(ReadRequest{Payload: PositionRead{Entity: 1}}); err != nil {
		t.Fatalf("PositionRead of a known entity = %v", err)
	}

	host.ConfigureGameplayCatalog(GameplayCatalog{Combat: CombatPolicyCatalog{FormulaPolicy: "legacy_v0"}})
	if _, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Target: 2, Amount: 10}}); !errors.Is(err, ErrCombatPolicyUnsupported) {
		t.Fatalf("damage under a foreign formula policy = %v", err)
	}
	host.ConfigureGameplayCatalog(GameplayCatalog{Combat: CombatPolicyCatalog{FormulaPolicy: "twelve_stage_v1", DamageTypes: []DamageTypeHandle{3}}})
	if _, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 9}}); !errors.Is(err, ErrCombatHandleInvalid) {
		t.Fatalf("damage with an undeclared damage type = %v", err)
	}
	if host.HealthForTest(2) != 100 {
		t.Fatalf("a refused damage command changed health: %d", host.HealthForTest(2))
	}
	if _, err := host.Apply(EffectCommand{Payload: DamageCommand{Source: 1, Target: 2, Amount: 10, DamageType: 3}}); err != nil {
		t.Fatalf("damage with a declared damage type = %v", err)
	}
}
