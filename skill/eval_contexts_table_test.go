package skill

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// 求值上下文表的守卫（维护者第五轮决定）：表的每个格子都有用例。
//   - 可用的格子：把这一行的引用放进该上下文的一个位点，定义编译无 error，施法推进
//     10 个 tick 不出错，位点之后的“见证”效果确实执行（证明引用被求值过）；
//   - 不可用的格子：编译被拒绝，诊断点名该上下文与表项；Runtime 在该上下文里求这一行返回
//     ErrReferenceOutOfContext（不是 ErrProgramInvariant）。
//   - 该上下文没有这一行类型的位点时（采样点只求实体、进程字段没有字符串位点……），用例表
//     写明理由，守卫照样要求这一格有记录。
// 给表加一行或一个上下文而不补这里，TestEvalContextTableEveryCellHasACase 失败。快照点表
// （evalSnapshotTable）由 TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime 逐格守。
// 第五批 O33 的“漂移”格子（维护者第七轮决定改为编译期拒绝）另由
// TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative 钉住，诊断里的替代写法由
// TestO33AlternativesCompileAndRun 证明确实可用。

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

func evalCellHost() *MemoryHost {
	host := runtimeTestHost(DefaultCompileEnvironment())
	host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{2: 1}})
	return host
}

func runEvalCell(program *Program, fixture evalRowFixture, host *MemoryHost) (*MemoryHost, error) {
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
					if diagnosticMentionsAll(diagnostics, code, context.String(), row.name) {
						return
					}
					t.Fatalf("no %s diagnostic naming %s and row %s: %#v", code, context, row.name, diagnostics)
				}
				requireNoErrors(t, diagnostics)
				host, err := runEvalCell(program, fixture, evalCellHost())
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

// O33（维护者第七轮决定）：`$primary_target` 作 area 选择的起点。此前编译通过，启动那一步以施法目标
// （实体 2，远处）为圆心，移交后漂移成以 lifecycle 实体（陷阱，施法者脚下）为圆心；现在编译期拒绝，
// 诊断点名 process_step 上下文与 `$primary_target` 表项，并给出替代写法。
func TestProcessStepPrimaryTargetIsRejectedAtCompileTime(t *testing.T) {
	enter := `{"flow":"effect","effect":{"type":"damage","target":"$event.target","amount":1,"damage_type":"physical"}}`
	area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":4},"process":{"kind":"area","duration_ticks":4,"interval_ticks":1,"area":{"from":"$primary_target","kind":"entity","shape":{"type":"circle","radius":2},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":4}},"on":{"enter":` + enter + `}}`
	program, diagnostics := Compile(mustParseJSON(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(area))), DefaultCompileEnvironment())
	if program != nil {
		t.Fatalf("$primary_target in a process field compiled; it drifts to the lifecycle entity after handoff (O33)")
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == DiagnosticInputUnavailable && strings.HasSuffix(diagnostic.Path, ".area.from") &&
			strings.Contains(diagnostic.Message, "process_step") && strings.Contains(diagnostic.Message, "row $primary_target") && strings.Contains(diagnostic.Message, "改用") {
			return
		}
	}
	t.Fatalf("no INPUT_UNAVAILABLE at area.from naming process_step, the $primary_target row and an alternative: %#v", diagnostics)
}

// evalO33ReferenceCells / evalO33SnapshotCells 是第五批 O33 记下的全部“漂移”格子（此前编译通过、
// Runtime 不失败，但值随进程移交或求值位置变化）。维护者第七轮决定全部改为编译期拒绝；memory
// 默认值里的 phase_start 按方案 §8 的判断一并拒绝（它只是 Activate 时的值、与 cast_start 相同）。
var evalO33ReferenceCells = func() map[evalReferenceRowIndex][]evalContext {
	cells := map[evalReferenceRowIndex][]evalContext{}
	for _, row := range []evalReferenceRowIndex{evalRowPrimaryTarget, evalRowAbilitySelf, evalRowCastElapsedTicks, evalRowCastChargeBP, evalRowCastReleaseReason, evalRowCastPulseIndex, evalRowCastStock, evalRowCastMaxStock} {
		cells[row] = []evalContext{evalProcessStep, evalStateDefault}
	}
	return cells
}()

var evalO33SnapshotCells = map[snapshotPoint][]evalContext{
	snapshotCastStart:  {evalProcessStep, evalStateDefault},
	snapshotPhaseStart: {evalMemoryDefault, evalProcessStep, evalStateDefault},
}

// O33 的每个格子：表里不可用，说明里有替代写法（“改用 …”），有位点的格子编译被拒绝且诊断点名
// 上下文、表项与替代写法。
func TestEvalContextTableRejectsTheO33DriftCellsWithAnAlternative(t *testing.T) {
	for row, contexts := range evalO33ReferenceCells {
		entry := evalReferenceTable[row]
		for _, context := range contexts {
			cell := entry.cells[context]
			if cell.usable() || !strings.Contains(cell.semantics, "改用") {
				t.Errorf("row %s context %s: usable=%v semantics %q; O33 cells are rejected and name an alternative", entry.name, context, cell.usable(), cell.semantics)
				continue
			}
			definition, noSite := evalCellCase(t, row, context)
			if definition == "" {
				t.Errorf("row %s context %s: rejected cell has no negative case (%s)", entry.name, context, noSite)
				continue
			}
			_, diagnostics := Compile(mustParseJSON(t, definition), DefaultCompileEnvironment())
			if !diagnosticMentionsAll(diagnostics, DiagnosticInputUnavailable, context.String(), "row "+entry.name, "改用") {
				t.Errorf("row %s context %s: no diagnostic naming the context, the row and an alternative: %#v", entry.name, context, diagnostics)
			}
		}
	}
	for point, contexts := range evalO33SnapshotCells {
		for _, context := range contexts {
			cell := evalSnapshotTable[point][context]
			if cell.usable() || !strings.Contains(cell.semantics, "改用") {
				t.Errorf("snapshot %s context %s: usable=%v semantics %q; O33 cells are rejected and name an alternative", point, context, cell.usable(), cell.semantics)
				continue
			}
			environment, definition := evalSnapshotCellCase(t, point, context)
			_, diagnostics := Compile(mustParseJSON(t, definition), environment)
			if !diagnosticMentionsAll(diagnostics, DiagnosticAttributeSnapshotInvalid, context.String(), "row "+string(point), "改用") {
				t.Errorf("snapshot %s context %s: no diagnostic naming the context, the row and an alternative: %#v", point, context, diagnostics)
			}
		}
	}
}

func diagnosticMentionsAll(diagnostics []Diagnostic, code DiagnosticCode, texts ...string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code != code {
			continue
		}
		found := true
		for _, text := range texts {
			found = found && strings.Contains(diagnostic.Message, text)
		}
		if found {
			return true
		}
	}
	return false
}

// 快照点表（evalSnapshotTable）的守卫：每个快照点在每个非采样上下文里都有一格、有说明，
// 并逐格编译 / 施法——可用格编译无 error、施法推进 10 tick 不出错、位点之后的见证效果执行；
// 不可用格编译被拒，ATTRIBUTE_SNAPSHOT_INVALID 点名上下文与表项。采样上下文只求实体，
// 实体里没有属性读取，所以那三列没有格子。
//
// 读取用一个测试属性 reach（距离量纲）：进程字段只有实体 / 位置 / 距离 / 角度类型的位点，
// 默认目录里的属性都不是距离量纲，放不进 area 半径。
const evalReachAttribute AttributeHandle = 41

func evalSnapshotEnvironment() CompileEnvironment {
	environment := DefaultCompileEnvironment()
	environment.Gameplay.Attributes.Entries = append(environment.Gameplay.Attributes.Entries, AttributeCatalogEntry{
		Handle: evalReachAttribute, Key: "reach", ValueType: valueKindInt, Quantity: quantityWorldDistance, Readable: true,
		Snapshots: []string{"cast_start", "phase_start", "process_start", "current"}, ModifierOperations: []string{"add"},
		Minimum: 0, Maximum: 100, Rounding: "toward_zero",
	})
	environment.Digest = AuthorityDigest(environment)
	return environment
}

func evalSnapshotCellCase(t *testing.T, point snapshotPoint, context evalContext) (CompileEnvironment, string) {
	t.Helper()
	read := func(entity string) string {
		return `{"read_attribute":{"entity":"` + entity + `","attribute":"reach","snapshot":"` + string(point) + `"}}`
	}
	knockback := func(entity, witness string) string {
		return `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"knockback","target":"` + entity + `","from":"` + entity + `.position","distance":` + read(entity) + `}},` + witness + `]}`
	}
	probe := `{"op":"eq","args":[` + read("$caster") + `,` + read("$caster") + `]}`
	fixture := evalRowFixture{}
	var definition string
	switch context {
	case evalCastFlow:
		definition = evalDefinition(t, fixture, "{}", agreementSteps(knockback("$caster", evalWitness)), "")
	case evalMemoryDefault:
		definition = evalDefinition(t, fixture, `{"probe":{"type":"bool","default":`+probe+`}}`, agreementSteps(`{"flow":"if","condition":"$memory.probe","then":`+evalWitness+`}`), "")
	case evalProcessStep:
		definition = evalDefinition(t, fixture, "{}", agreementSteps(evalArea(`"$caster"`, read("$caster"), evalCallbackWitness)), "")
	case evalProcessCallback:
		definition = evalDefinition(t, fixture, "{}", agreementSteps(evalArea(`"$caster"`, "4", knockback("$owner", evalCallbackWitness))), "")
	case evalStateDefault:
		readState := func(owner, witness string) string {
			return `{"flow":"if","condition":{"read_state":{"state":"who_state","owner":"` + owner + `"}},"then":` + witness + `}`
		}
		state := `{"type":"bool","scope":"owner","default":` + probe + `,"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}}`
		definition = evalDefinition(t, fixture, "{}", agreementSteps(readState("$caster", evalWitness), evalArea(`"$caster"`, "4", readState("$owner", evalCallbackWitness))), state)
	default:
		t.Fatalf("context %s has no snapshot case builder", context)
	}
	return evalSnapshotEnvironment(), definition
}

func TestEvalSnapshotTableCellsAgreeWithCompilerAndRuntime(t *testing.T) {
	points := []snapshotPoint{snapshotCastStart, snapshotPhaseStart, snapshotProcessStart}
	if len(evalSnapshotTable) != len(points) {
		t.Fatalf("snapshot table has %d points, the guard knows %d", len(evalSnapshotTable), len(points))
	}
	for _, point := range points {
		row, found := evalSnapshotTable[point]
		if !found {
			t.Fatalf("snapshot table has no row %s", point)
		}
		for context := evalCastFlow; context < evalContextCount; context++ {
			if context.isCapture() {
				continue
			}
			cell := row[context]
			t.Run(fmt.Sprintf("%s/%s", point, context), func(t *testing.T) {
				if strings.TrimSpace(cell.semantics) == "" {
					t.Fatalf("no semantics")
				}
				environment, definition := evalSnapshotCellCase(t, point, context)
				program, diagnostics := Compile(mustParseJSON(t, definition), environment)
				if !cell.usable() {
					if program != nil {
						t.Fatalf("unavailable cell compiled: %s", definition)
					}
					if !diagnosticMentionsAll(diagnostics, DiagnosticAttributeSnapshotInvalid, context.String(), "row "+string(point)) {
						t.Fatalf("no %s diagnostic naming %s and row %s: %#v", DiagnosticAttributeSnapshotInvalid, context, point, diagnostics)
					}
					return
				}
				requireNoErrors(t, diagnostics)
				host := runtimeTestHost(environment)
				host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{evalReachAttribute: 2}})
				if _, err := runEvalCell(program, evalRowFixture{}, host); err != nil {
					t.Fatalf("cast failed: %v", err)
				}
				if health := host.HealthForTest(1); health >= 100 {
					t.Fatalf("the witness after the site never ran: caster health = %d", health)
				}
			})
		}
	}
}

// O33 诊断里给出的替代写法确实能编译、执行（不能让作者照着改了还是编不过）：
//   - `$primary_target` 进程字段 → spawn position 在施法流程里用施法目标的位置，回调里以
//     `$lifecycle_entity` 为中心 select（远处的目标被打到、施法者没有）；
//   - `$cast.*` / cast_start 进程字段 → numeric track 的初值（启动时用施法求一次）；
//   - 状态默认值里的施法引用 → 字面量 / `$caster` 默认值，在施法流程里 modify_state 写入同一个
//     表达式（null 默认值的实体状态 set 在 MemoryHost 上类型不匹配，是另一处问题，这里不用它）；
//   - memory 默认值里的 phase_start → cast_start（快照守卫的正例覆盖）。
func TestO33AlternativesCompileAndRun(t *testing.T) {
	environment := evalSnapshotEnvironment()
	compile := func(t *testing.T, definition string) *Program {
		t.Helper()
		program, diagnostics := Compile(mustParseJSON(t, definition), environment)
		requireNoErrors(t, diagnostics)
		return program
	}
	run := func(t *testing.T, program *Program, input CastInput, host *MemoryHost) {
		t.Helper()
		runtime := NewRuntime(host, RuntimeOptions{})
		castID, err := runtime.Activate(program, input)
		if err != nil {
			t.Fatal(err)
		}
		for tick := Tick(1); tick <= 6; tick++ {
			if tick == 3 && program.cast.mode == castModeCharge {
				_ = runtime.Release(castID)
			}
			if err := runtime.Advance(tick); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("lifecycle entity instead of $primary_target", func(t *testing.T) {
		tick := `{"flow":"select","select":{"from":"$lifecycle_entity","kind":"entity","shape":{"type":"circle","radius":2},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":4},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"damage","target":"$local.t","amount":1,"damage_type":"physical"}}}}`
		spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$input.target.position","count":1,"duration_ticks":4},"on":{"tick":` + tick + `}}`
		program := compile(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(spawn)))
		host := runtimeTestHost(environment)
		host.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100, Position: Position{X: 50}})
		run(t, program, CastInput{Caster: 1, Target: 2}, host)
		if host.HealthForTest(2) >= 100 || host.HealthForTest(1) != 100 {
			t.Fatalf("the callbacks must center on the lifecycle entity at the target: target health %d, caster health %d", host.HealthForTest(2), host.HealthForTest(1))
		}
	})
	t.Run("numeric track instead of $cast state and cast_start in process fields", func(t *testing.T) {
		for _, value := range []string{
			`{"op":"scale_bp","args":[10,"$cast.charge_bp"]}`,
			`{"read_attribute":{"entity":"$caster","attribute":"reach","snapshot":"cast_start"}}`,
		} {
			spawn := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":10},"process":{` +
				numericLinearProcessWithTracks(`{"property":"speed","operation":"set","value":`+value+`,"over_ticks":0}`) + `}}`
			program := compile(t, evalDefinition(t, evalRowFixtures[evalRowCastChargeBP], "{}", agreementSteps(spawn), ""))
			host := runtimeTestHost(environment)
			host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{evalReachAttribute: 2}})
			run(t, program, CastInput{Caster: 1}, host)
		}
	})
	t.Run("modify_state in the cast flow instead of a state default", func(t *testing.T) {
		lifetime := `"lifetime":{"duration_ticks":20,"maximum_duration_ticks":40,"on_write":"refresh","clear_on":[]}`
		state := `"persistent_state":{"who":{"type":"entity","scope":"owner","default":"$caster",` + lifetime + `},"flag":{"type":"bool","scope":"owner","default":false,` + lifetime + `}},"initial_phase"`
		write := `{"flow":"if","condition":{"op":"exists","args":["$primary_target"]},"then":{"flow":"effect","effect":{"type":"modify_state","state":"who","owner":"$caster","operation":"set","value":"$primary_target","duration_ticks":20,"expiry_policy":"refresh"}}}`
		flag := `{"flow":"effect","effect":{"type":"modify_state","state":"flag","owner":"$caster","operation":"set","value":{"op":"gte","args":[{"read_attribute":{"entity":"$caster","attribute":"reach","snapshot":"cast_start"}},1]},"duration_ticks":20,"expiry_policy":"refresh"}}`
		read := `{"flow":"if","condition":{"op":"and","args":[{"read_state":{"state":"flag","owner":"$owner"}},{"op":"exists","args":[{"read_state":{"state":"who","owner":"$owner"}}]}]},"then":` + evalCallbackWitness + `}`
		definition := stringsReplaceOnce(t, agreementSkillJSON("entity", "{}", "[]", agreementSteps(write, flag, evalArea(`"$caster"`, "4", read))), `"initial_phase"`, state)
		program := compile(t, definition)
		host := runtimeTestHost(environment)
		host.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}, Attributes: map[AttributeHandle]int64{evalReachAttribute: 2}})
		run(t, program, CastInput{Caster: 1, Target: 2}, host)
		if host.HealthForTest(1) >= 100 {
			t.Fatalf("the process callback must see the states written in the cast flow: caster health %d", host.HealthForTest(1))
		}
	})
}
