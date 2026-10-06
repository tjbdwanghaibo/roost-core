package skill

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// 求值上下文表的守卫（维护者第五轮决定）：表的每个格子都有用例。
//   - 可用 / 漂移的格子：把这一行的引用放进该上下文的一个位点，定义编译无 error，施法推进
//     10 个 tick 不出错，位点之后的“见证”效果确实执行（证明引用被求值过）；
//   - 不可用的格子：编译被拒绝，诊断点名该上下文；Runtime 在该上下文里求这一行返回
//     ErrReferenceOutOfContext（不是 ErrProgramInvariant）。
//   - 该上下文没有这一行类型的位点时（采样点只求实体、进程字段没有字符串位点……），用例表
//     写明理由，守卫照样要求这一格有记录。
// 给表加一行或一个上下文而不补这里，TestEvalContextTableEveryCellHasACase 失败。

type evalRowFixture struct {
	reference string
	input     string // 输入布局（none / entity）
	memory    string // memory 声明
	policy    string // 替换 {"mode":"tap"} 的 policy
	// kind 决定放进位点的方式：entity、optional（可缺省实体）、position、comparable
	// （能 eq 的 int / string / ability）、process。
	kind string
	// bp：值是 basis points，可以放进 scale_bp。
	bp bool
}

var evalRowFixtures = map[evalReferenceRowIndex]evalRowFixture{
	evalRowInput:                {reference: "$input.target", input: "entity", kind: "entity"},
	evalRowMemory:               {reference: "$memory.who", memory: `{"who":{"type":"entity","default":"$caster"}}`, kind: "entity"},
	evalRowLocal:                {reference: "$local.t", kind: "entity"},
	evalRowCaster:               {reference: "$caster", kind: "entity"},
	evalRowCasterPosition:       {reference: "$caster.position", kind: "position"},
	evalRowPrimaryTarget:        {reference: "$primary_target", input: "entity", kind: "optional"},
	evalRowAbilitySelf:          {reference: "$ability.self", kind: "comparable"},
	evalRowCastMode:             {reference: "$cast.mode", kind: "comparable"},
	evalRowCastElapsedTicks:     {reference: "$cast.elapsed_ticks", kind: "comparable"},
	evalRowCastChargeBP:         {reference: "$cast.charge_bp", policy: `{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":true}`, kind: "comparable", bp: true},
	evalRowCastReleaseReason:    {reference: "$cast.release_reason", policy: `{"mode":"charge","max_charge_ticks":10,"min_charge_bp":0,"auto_release":true}`, kind: "comparable"},
	evalRowCastPulseIndex:       {reference: "$cast.pulse_index", policy: `{"mode":"hold","pulse_interval_ticks":2,"max_duration_ticks":8,"sustain_costs":[]}`, kind: "comparable"},
	evalRowCastStock:            {reference: "$cast.stock", policy: `{"mode":"ammo","max_stock":2,"recharge_ticks":3,"initial_stock":2}`, kind: "comparable"},
	evalRowCastMaxStock:         {reference: "$cast.max_stock", policy: `{"mode":"ammo","max_stock":2,"recharge_ticks":3,"initial_stock":2}`, kind: "comparable"},
	evalRowOwner:                {reference: "$owner", kind: "entity"},
	evalRowOwnerPosition:        {reference: "$owner.position", kind: "position"},
	evalRowLifecycleEntity:      {reference: "$lifecycle_entity", kind: "entity"},
	evalRowProcess:              {reference: "$process", kind: "process"},
	evalRowEventSource:          {reference: "$event.source", kind: "entity"},
	evalRowEventOwner:           {reference: "$event.owner", kind: "entity"},
	evalRowEventTarget:          {reference: "$event.target", kind: "entity"},
	evalRowEventTick:            {reference: "$event.tick", kind: "comparable"},
	evalRowEventMembershipTicks: {reference: "$event.membership_ticks", kind: "comparable"},
	evalRowEventEnterCount:      {reference: "$event.enter_count", kind: "comparable"},
}

const evalWitness = `{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":1,"damage_type":"physical"}}`

// evalCallbackWitness 在回调里伤害施法者（$owner）：施法者一定活着，不依赖 area 成员。
const evalCallbackWitness = `{"flow":"effect","effect":{"type":"damage","target":"$owner","amount":1,"damage_type":"physical"}}`

// evalUse 把引用放进一段以 witness 结尾的流程：可比较的值进 if 条件，位置进 knockback 的起点，
// 进程进 modify_process。返回 ""：这种值在流程里没有位点。
func evalUse(fixture evalRowFixture, reference, self, witness string) string {
	switch fixture.kind {
	case "entity", "comparable":
		return `{"flow":"if","condition":{"op":"eq","args":["` + reference + `","` + reference + `"]},"then":` + witness + `}`
	case "optional":
		return `{"flow":"if","condition":{"op":"exists","args":["` + reference + `"]},"then":` + witness + `}`
	case "position":
		return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"knockback","target":"` + self + `","from":"` + reference + `","distance":0}},` + witness + `]}`
	case "process":
		return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"modify_process","process":"` + reference + `","property":"speed","operation":"mul_bp","value":9000,"over_ticks":2}},` + witness + `]}`
	}
	return ""
}

// evalWithLocal 在需要局部变量的行外面包一层 select each（局部变量叫 t，选中施法者附近的实体）。
func evalWithLocal(row evalReferenceRowIndex, from, body string) string {
	if row != evalRowLocal {
		return body
	}
	return `{"flow":"select","select":{"from":"` + from + `","kind":"entity","shape":{"type":"circle","radius":10},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":1},"consume":{"mode":"each","as":"t","do":` + body + `}}`
}

func evalDefinition(t *testing.T, fixture evalRowFixture, memory, steps string, state string) string {
	t.Helper()
	input := fixture.input
	if input == "" {
		input = "none"
	}
	if fixture.memory != "" {
		memory = mergeJSONObjects(fixture.memory, memory)
	}
	text := agreementSkillJSON(input, memory, "[]", steps)
	if fixture.policy != "" {
		// charge / hold / toggle 要求 on.release。
		text = stringsReplaceOnce(t, text, `{"mode":"tap"}`, fixture.policy)
		text = stringsReplaceOnce(t, text, `"on":{"enter":`, `"on":{"release":{"flow":"finish"},"enter":`)
	}
	if state != "" {
		text = stringsReplaceOnce(t, text, `"initial_phase"`, `"persistent_state":{"who_state":`+state+`},"initial_phase"`)
	}
	return text
}

func mergeJSONObjects(left, right string) string {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if right == "{}" || right == "" {
		return left
	}
	if left == "{}" || left == "" {
		return right
	}
	return strings.TrimSuffix(left, "}") + "," + strings.TrimPrefix(right, "{")
}

// evalArea 是一个 area 进程：from 是 area 选择的起点（进程每一步重新求值），radius 是半径，
// tick 是 tick 回调。
func evalArea(from, radius, tick string) string {
	return `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":` + from + `,"kind":"entity","shape":{"type":"circle","radius":` + radius + `},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"tick":` + tick + `}}`
}

// evalCellCase 生成一个格子的定义；noSite 非空时表示没有位点。
func evalCellCase(t *testing.T, row evalReferenceRowIndex, context evalContext) (definition string, noSite string) {
	t.Helper()
	fixture := evalRowFixtures[row]
	reference := fixture.reference
	read := func(point string) string {
		return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$caster","amount":` + captureRead(reference, point) + `,"damage_type":"physical"}},` + evalWitness + `]}`
	}
	callbackRead := `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$owner","amount":` + captureRead(reference, "process_start") + `,"damage_type":"physical"}},` + evalCallbackWitness + `]}`
	switch context {
	case evalCastFlow:
		use := evalUse(fixture, reference, "$caster", evalWitness)
		if fixture.kind == "process" {
			use = `{"flow":"if","condition":{"op":"eq","args":["$process","$process"]},"then":` + evalWitness + `}`
		}
		return evalDefinition(t, fixture, "{}", agreementSteps(evalWithLocal(row, "$caster", use)), ""), ""
	case evalMemoryDefault:
		memory, flow := "", ""
		switch fixture.kind {
		case "position":
			memory = `{"probe":{"type":"position","default":"` + reference + `"}}`
			flow = `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"knockback","target":"$caster","from":"$memory.probe","distance":0}},` + evalWitness + `]}`
		case "optional":
			memory = `{"probe":{"type":"bool","default":{"op":"exists","args":["` + reference + `"]}}}`
			flow = `{"flow":"if","condition":"$memory.probe","then":` + evalWitness + `}`
		default:
			memory = `{"probe":{"type":"bool","default":{"op":"eq","args":["` + reference + `","` + reference + `"]}}}`
			flow = `{"flow":"if","condition":"$memory.probe","then":` + evalWitness + `}`
		}
		return evalDefinition(t, fixture, memory, agreementSteps(flow), ""), ""
	case evalCastStartCapture, evalPhaseStartCapture:
		if !evalReferenceTable[row].entity {
			return "", "采样点只求读取的实体"
		}
		point := "cast_start"
		if context == evalPhaseStartCapture {
			point = "phase_start"
		}
		body := read(point)
		if fixture.kind == "optional" {
			body = `{"flow":"if","condition":{"op":"exists","args":["` + reference + `"]},"then":` + body + `}`
		}
		return evalDefinition(t, fixture, "{}", agreementSteps(evalWithLocal(row, "$caster", body)), ""), ""
	case evalProcessStartCapture:
		if !evalReferenceTable[row].entity {
			return "", "采样点只求读取的实体"
		}
		body := callbackRead
		if fixture.kind == "optional" {
			body = `{"flow":"if","condition":{"op":"exists","args":["` + reference + `"]},"then":` + body + `}`
		}
		return evalDefinition(t, fixture, "{}", agreementSteps(evalArea(`"$caster"`, "4", evalWithLocal(row, "$owner", body))), ""), ""
	case evalProcessStep:
		from, radius := `"$caster"`, "4"
		switch {
		case fixture.kind == "entity" || fixture.kind == "optional" || fixture.kind == "position":
			from = `"` + reference + `"`
		case fixture.bp:
			radius = `{"op":"scale_bp","args":[4,"` + reference + `"]}`
		case evalReferenceTable[row].cells[context].usable():
			return "", "进程字段没有这种类型（字符串 / 技能 / 非 bp 计数与 tick）的位点"
		default:
			from = `"` + reference + `"`
		}
		steps := agreementSteps(evalWithLocal(row, "$caster", evalArea(from, radius, evalCallbackWitness)))
		return evalDefinition(t, fixture, "{}", steps, ""), ""
	case evalProcessCallback:
		use := evalUse(fixture, reference, "$owner", evalCallbackWitness)
		if fixture.kind == "process" {
			// modify_process 只认绑定了数值属性的 motion 进程。
			projectile := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{` + numericLinearProcess() + `},"on":{"tick":` + use + `}}`
			return evalDefinition(t, fixture, "{}", agreementSteps(projectile), ""), ""
		}
		return evalDefinition(t, fixture, "{}", agreementSteps(evalArea(`"$caster"`, "4", evalWithLocal(row, "$owner", use))), ""), ""
	case evalStateDefault:
		stateType, stateDefault, readCondition := "bool", `{"op":"eq","args":["`+reference+`","`+reference+`"]}`, func(owner string) string {
			return `{"read_state":{"state":"who_state","owner":"` + owner + `"}}`
		}
		switch fixture.kind {
		case "position":
			stateType, stateDefault = "position", `"`+reference+`"`
		case "optional", "entity":
			stateType, stateDefault = "entity", `"`+reference+`"`
		}
		use := func(owner, witness string) string {
			switch stateType {
			case "position":
				return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"knockback","target":"` + owner + `","from":` + readCondition(owner) + `,"distance":0}},` + witness + `]}`
			case "entity":
				return `{"flow":"if","condition":{"op":"exists","args":[` + readCondition(owner) + `]},"then":` + witness + `}`
			}
			return `{"flow":"if","condition":` + readCondition(owner) + `,"then":` + witness + `}`
		}
		state := `{"type":"` + stateType + `","scope":"owner","default":` + stateDefault + `,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}`
		// 同一条状态在施法流程与进程回调里各读一次：默认值在两个上下文里都要求得出。
		steps := agreementSteps(evalWithLocal(row, "$caster", use("$caster", evalWitness)), evalArea(`"$caster"`, "4", use("$owner", evalCallbackWitness)))
		return evalDefinition(t, fixture, "{}", steps, state), ""
	}
	t.Fatalf("context %s has no case builder", context)
	return "", ""
}

func runEvalCell(program *Program, fixture evalRowFixture) (*MemoryHost, error) {
	host := runtimeTestHost(DefaultCompileEnvironment())
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 1}})
	runtime := NewRuntime(host, RuntimeOptions{})
	input := CastInput{Caster: 1}
	if fixture.input == "entity" {
		input.Target = 2
	}
	castID, err := runtime.Activate(program, input)
	if err != nil {
		return host, err
	}
	for tick := Tick(1); tick <= 10; tick++ {
		if tick == 3 && program.cast.mode == castModeCharge {
			_ = runtime.Release(castID)
		}
		if err := runtime.Advance(tick); err != nil {
			return host, err
		}
	}
	return host, nil
}

func TestEvalContextTableEveryCellHasACase(t *testing.T) {
	var missing []string
	for index, row := range evalReferenceTable {
		if row.name == "" {
			continue
		}
		if _, found := evalRowFixtures[evalReferenceRowIndex(index)]; !found {
			missing = append(missing, row.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("evaluation context table rows without a fixture (add one to evalRowFixtures): %v", missing)
	}
	for index, row := range evalReferenceTable {
		if row.name == "" {
			continue
		}
		for context := evalCastFlow; context < evalContextCount; context++ {
			if strings.TrimSpace(row.cells[context].semantics) == "" {
				t.Errorf("row %s context %s has no semantics", row.name, context)
			}
			definition, noSite := evalCellCase(t, evalReferenceRowIndex(index), context)
			if definition == "" && noSite == "" {
				t.Errorf("row %s context %s has neither a case nor a no-site reason", row.name, context)
			}
		}
	}
}

// 正例与反例：逐格编译、施法。
func TestEvalContextTableCellsAgreeWithCompilerAndRuntime(t *testing.T) {
	for index, row := range evalReferenceTable {
		if row.name == "" {
			continue
		}
		rowIndex := evalReferenceRowIndex(index)
		fixture := evalRowFixtures[rowIndex]
		for context := evalCastFlow; context < evalContextCount; context++ {
			cell := row.cells[context]
			t.Run(fmt.Sprintf("%s/%s", row.name, context), func(t *testing.T) {
				definition, noSite := evalCellCase(t, rowIndex, context)
				if definition == "" {
					t.Skipf("no site: %s", noSite)
				}
				program, diagnostics := Compile(mustParseJSON(t, definition), DefaultCompileEnvironment())
				if !cell.usable() {
					if program != nil {
						t.Fatalf("unavailable cell compiled: %s", definition)
					}
					code := DiagnosticInputUnavailable
					if context.isCapture() {
						code = DiagnosticAttributeSnapshotInvalid
					}
					for _, diagnostic := range diagnostics {
						if diagnostic.Code == code && strings.Contains(diagnostic.Message, context.String()) {
							return
						}
					}
					t.Fatalf("no %s diagnostic naming %s: %#v", code, context, diagnostics)
				}
				requireNoErrors(t, diagnostics)
				host, err := runEvalCell(program, fixture)
				if err != nil {
					t.Fatalf("cast failed: %v", err)
				}
				if health := host.HealthForTest(1); health >= 100 {
					t.Fatalf("the witness after the site never ran: caster health = %d", health)
				}
			})
		}
	}
}

// 反例的 Runtime 一半：表里不可用的格子，在该上下文里直接求这一行返回 ErrReferenceOutOfContext
// 并点名上下文与表项；可用的格子不会因为查表被拒绝。
func TestRuntimeEvaluatesReferencesOnlyWhereTheTableAllows(t *testing.T) {
	program, _ := compileRuntimeJSON(t, minimalSkillJSON)
	runtime := NewRuntime(runtimeTestHost(DefaultCompileEnvironment()), RuntimeOptions{})
	for index, row := range evalReferenceTable {
		if row.name == "" {
			continue
		}
		reference := referenceProgramValue{kind: referenceBuiltin, builtin: row.name, typ: row.typ, row: evalReferenceRowIndex(index)}
		switch evalReferenceRowIndex(index) {
		case evalRowInput:
			reference.kind = referenceInput
		case evalRowMemory:
			reference.kind = referenceMemory
		case evalRowLocal:
			reference.kind = referenceLocal
		}
		for context := evalCastFlow; context < evalContextCount; context++ {
			cast := &castInstance{program: program, caster: 1, evalContext: context, snapshots: map[int]RuntimeValue{}}
			_, err := runtime.evalReference(cast, reference)
			outOfContext := errors.Is(err, ErrReferenceOutOfContext)
			if row.cells[context].usable() == outOfContext {
				t.Errorf("row %s context %s: usable=%v but evalReference returned %v", row.name, context, row.cells[context].usable(), err)
			}
			if outOfContext && (errors.Is(err, ErrProgramInvariant) || !strings.Contains(err.Error(), context.String()) || !strings.Contains(err.Error(), row.name)) {
				t.Errorf("row %s context %s: error %q must name the context and the row and must not be ErrProgramInvariant", row.name, context, err)
			}
		}
	}
}

// O33：移交后进程字段里漂移的引用，值就是表里写的那样。`$primary_target` 作 area 选择的起点：
// 启动那一步以施法目标（实体 2，远处）为圆心，移交后以 lifecycle 实体（陷阱，施法者脚下）为圆心。
func TestProcessStepPrimaryTargetDriftsToTheLifecycleEntity(t *testing.T) {
	enter := `{"flow":"effect","effect":{"type":"damage","target":"$event.target","amount":1,"damage_type":"physical"}}`
	area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":"$primary_target","kind":"entity","shape":{"type":"circle","radius":2},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":4}},"on":{"enter":` + enter + `}}`
	program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(area))), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(DefaultCompileEnvironment())
	host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Position: Position{X: 50}})
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if health := host.HealthForTest(2); health != 99 {
		t.Fatalf("start step must center on the cast target: target health = %d, want 99", health)
	}
	casterBefore := host.HealthForTest(1)
	for tick := Tick(1); tick <= 3; tick++ {
		if err := runtime.Advance(tick); err != nil {
			t.Fatal(err)
		}
	}
	if host.HealthForTest(1) >= casterBefore {
		t.Fatalf("after handoff the area must center on the lifecycle entity at the caster's feet: caster health %d -> %d", casterBefore, host.HealthForTest(1))
	}
}
