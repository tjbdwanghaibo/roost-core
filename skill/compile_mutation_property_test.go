package skill

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// 编译 ⇒ 可执行的性质测试（N09 第四批，第三批方向判断第 3 条）。
//
// 编译器的职责是证明 Program 可执行：Runtime 遇到 ErrProgramInvariant 就意味着编译器
// 放过了一个它本该拒绝（或 Runtime 本该支持）的形状。手写 acceptance 只覆盖 fixture
// 本身；这里以 testdata 的全部 fixture 加几个覆盖 fixture 没有用到的效果的种子为起点，
// 机械生成变异：删除对象键、改写数值、按同名键的词表替换字符串（外加几个不存在的
// 名字）、翻转布尔、删除 / 复制数组元素；设 SKILL_MUTATION_FULL=1 时再加跨种子的
// 子树移植。编译无 error 的变异在 MemoryHost 上用五种驱动方式施法（配置的输入、实体
// 输入、全输入、tick 2 取消、tick 1 打断并送输入端口）推进 40 tick 并做 checkpoint，
// 断言 Compile 与 Runtime 都不 panic、任何返回的错误都不是 ErrProgramInvariant。
//
// 第二个性质（只编译）：变异改变了 Program 的执行内容，gameplay digest 就必须改变——
// checkpoint 恢复按 (id, gameplay digest) 找 Program，摘要漏字段会让恢复拿到另一个
// 语义的 Program。
//
// 本测试在 RR-20261005-NC-210（未声明 memory 名字落到槽位 0）与 NC-211（移交后 area
// 回调 finish）修复前分别失败。新增效果 / 字段时，若这里出现 ErrProgramInvariant，先
// 看编译器缺了哪条规则，而不是在 Runtime 里补一个兜底分支。

type mutationSeed struct {
	name    string
	data    []byte
	fixture fixtureCase
}

type definitionMutation struct {
	desc string
	data []byte
}

func mutationSeeds(t *testing.T) []mutationSeed {
	t.Helper()
	var seeds []mutationSeed
	for _, fixture := range discoveredFixtureCases(t) {
		seeds = append(seeds, mutationSeed{name: fixture.name, data: mustReadFixture(t, fixture.name), fixture: fixture})
	}
	withEnter := func(input, memory, enter string) []byte {
		text := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, enter, 1)
		text = strings.Replace(text, `"input_schema":{"type":"none"}`, `"input_schema":{"type":"`+input+`"}`, 1)
		return []byte(strings.Replace(text, `"memory":{}`, `"memory":`+memory, 1))
	}
	effects := func(list ...string) string {
		steps := make([]string, 0, len(list)+1)
		for _, effect := range list {
			steps = append(steps, `{"flow":"effect","effect":`+effect+`}`)
		}
		return `{"flow":"sequence","steps":[` + strings.Join(append(steps, `{"flow":"finish"}`), ",") + `]}`
	}
	area := `{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":3},"process":{"kind":"area","duration_ticks":3,"interval_ticks":1,"emit_leave_on_stop":true,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"enter":{"flow":"effect","effect":{"type":"heal","target":"$event.target","amount":1}},"leave":{"flow":"finish"}}}`
	entity := fixtureCase{input: CastInput{Caster: 1, Target: 2}, expectedCast: CastFinished}
	none := fixtureCase{input: CastInput{Caster: 1}, expectedCast: CastFinished}
	return append(seeds,
		mutationSeed{name: "seed.shield", data: withEnter("entity", "{}", effects(`{"type":"shield","target":"$input.target","amount":10,"duration_ticks":5}`)), fixture: entity},
		mutationSeed{name: "seed.attribute_modifier", data: withEnter("entity", "{}", effects(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"add","value":5,"duration_ticks":10}`)), fixture: entity},
		mutationSeed{name: "seed.memory", data: withEnter("none", `{"count":{"type":"int","default":0},"flag":{"type":"bool","default":false}}`, effects(`{"type":"add_memory","name":"count","value":1}`, `{"type":"set_memory","name":"flag","value":true}`, `{"type":"clear_memory","name":"count"}`)), fixture: none},
		mutationSeed{name: "seed.movement", data: withEnter("entity", "{}", effects(`{"type":"knockback","target":"$input.target","from":"$caster.position","distance":2}`, `{"type":"pull","target":"$input.target","toward":"$caster.position","distance":1}`, `{"type":"stop_movement","target":"$input.target"}`)), fixture: entity},
		mutationSeed{name: "seed.modify_process", data: []byte(numericProcessSkillJSON(numericLinearProcess(), `{"type":"modify_process","process":"$process","property":"speed","operation":"mul_bp","value":8500,"over_ticks":6}`)), fixture: none},
		// RR-20261005-NC-211 的原触发：施法先结束，area 停止时 leave 回调 finish。
		mutationSeed{name: "seed.area_handoff", data: withEnter("none", "{}", `{"flow":"sequence","steps":[`+area+`,{"flow":"finish"}]}`), fixture: none},
		// N09 第五批：快照点的采样上下文（NC-220 / NC-223）与被动的事件输入（NC-221）。
		// 变异会把 current 换成 cast_start / process_start、把 `$owner` 换成 `$input.target` 等，
		// 覆盖“采样点求不出实体”的形状。
		mutationSeed{name: "seed.local_attribute_read", data: withEnter("none", "{}", `{"flow":"sequence","steps":[{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"damage","target":"$local.t","amount":{"read_attribute":{"entity":"$local.t","attribute":"ability_power","snapshot":"current"}},"damage_type":"physical"}}}},{"flow":"finish"}]}`), fixture: none},
		mutationSeed{name: "seed.process_start_read", data: withEnter("entity", "{}", `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":{"read_attribute":{"entity":"$caster","attribute":"ability_power","snapshot":"cast_start"}},"damage_type":"physical"}},{"flow":"effect","effect":{"type":"spawn","template":"deployable.trap","position":"$caster.position","count":1,"duration_ticks":3},"process":{"kind":"area","duration_ticks":3,"interval_ticks":1,"area":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":4},"filters":[{"type":"targetable"}],"order":{"by":"stable_id","direction":"asc"},"limit":2}},"on":{"tick":{"flow":"effect","effect":{"type":"damage","target":"$event.target","amount":{"read_attribute":{"entity":"$owner","attribute":"ability_power","snapshot":"process_start"}},"damage_type":"physical"}}}},{"flow":"finish"}]}`), fixture: entity},
		mutationSeed{name: "seed.passive_entity_input", data: []byte(strings.Replace(passiveSkillJSON(1, `[]`, `[]`), `"input_schema":{"type":"none"}`, `"input_schema":{"type":"entity"}`, 1)), fixture: fixtureCase{passive: true, expectedCast: CastFinished}},
	)
}

type mutationLibrary struct {
	leaves     map[string][]any    // 键名 → 种子里出现过的叶子值
	transplant map[string][]string // 键名（flow 对象统一为 "#flow"）→ 种子里出现过的子树
}

func buildMutationLibrary(t *testing.T, seeds []mutationSeed) mutationLibrary {
	t.Helper()
	leaves := map[string]map[string]any{}
	subtrees := map[string]map[string]bool{}
	for _, seed := range seeds {
		var root any
		if err := json.Unmarshal(seed.data, &root); err != nil {
			t.Fatalf("%s: %v", seed.name, err)
		}
		var walk func(key string, node any)
		walk = func(key string, node any) {
			switch typed := node.(type) {
			case map[string]any:
				category := mutationCategory(key, typed)
				if category != "" {
					encoded, _ := json.Marshal(typed)
					if subtrees[category] == nil {
						subtrees[category] = map[string]bool{}
					}
					subtrees[category][string(encoded)] = true
				}
				for childKey, child := range typed {
					walk(childKey, child)
				}
			case []any:
				for _, child := range typed {
					walk(key, child)
				}
			default:
				encoded, _ := json.Marshal(typed)
				if leaves[key] == nil {
					leaves[key] = map[string]any{}
				}
				leaves[key][string(encoded)] = typed
			}
		}
		walk("", root)
	}
	library := mutationLibrary{leaves: map[string][]any{}, transplant: map[string][]string{}}
	for key, values := range leaves {
		encoded := make([]string, 0, len(values))
		for value := range values {
			encoded = append(encoded, value)
		}
		sort.Strings(encoded)
		for _, value := range encoded {
			library.leaves[key] = append(library.leaves[key], values[value])
		}
	}
	for category, values := range subtrees {
		for value := range values {
			library.transplant[category] = append(library.transplant[category], value)
		}
		sort.Strings(library.transplant[category])
	}
	library.transplant["#flow"] = append(library.transplant["#flow"], `{"flow":"finish"}`, `{"flow":"wait","ticks":1,"then":{"flow":"finish"}}`)
	return library
}

func mutationCategory(key string, object map[string]any) string {
	if _, isFlow := object["flow"]; isFlow {
		return "#flow"
	}
	return key
}

func mutateDefinition(t *testing.T, data []byte, library mutationLibrary, transplant bool) []definitionMutation {
	t.Helper()
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	var result []definitionMutation
	emit := func(desc string) {
		encoded, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, definitionMutation{desc: desc, data: encoded})
	}
	var walk func(path, key string, node any, set func(any))
	walk = func(path, key string, node any, set func(any)) {
		if object, ok := node.(map[string]any); ok && transplant && key != "" {
			for _, candidate := range library.transplant[mutationCategory(key, object)] {
				var replacement any
				_ = json.Unmarshal([]byte(candidate), &replacement)
				set(replacement)
				emit(path + " <- " + candidate)
			}
			set(object)
		}
		switch typed := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for childKey := range typed {
				keys = append(keys, childKey)
			}
			sort.Strings(keys)
			for _, childKey := range keys {
				child := typed[childKey]
				walk(path+"."+childKey, childKey, child, func(value any) { typed[childKey] = value })
				delete(typed, childKey)
				emit("drop " + path + "." + childKey)
				typed[childKey] = child
			}
		case []any:
			for index := range typed {
				position := index
				walk(fmt.Sprintf("%s[%d]", path, position), key, typed[position], func(value any) { typed[position] = value })
			}
			if len(typed) > 0 {
				original := append([]any(nil), typed...)
				set(append([]any(nil), original[1:]...))
				emit("drop-first " + path)
				set(append(append([]any(nil), original...), original[len(original)-1]))
				emit("dup-last " + path)
				set(original)
			}
		case float64:
			for _, value := range []float64{0, 1, -1, 2, 7, 100, 100000, typed + 1, typed * 3} {
				if value != typed {
					set(value)
					emit(fmt.Sprintf("%s=%v", path, value))
				}
			}
			set(typed)
		case bool:
			set(!typed)
			emit(fmt.Sprintf("%s=%v", path, !typed))
			set(typed)
		case string:
			candidates := append(append([]any(nil), library.leaves[key]...), "zz_unknown", typed+"_zz", "$memory.zz", "$local.zz", "$input.zz")
			for _, value := range candidates {
				if text, ok := value.(string); ok && text == typed {
					continue
				}
				set(value)
				emit(fmt.Sprintf("%s=%v", path, value))
			}
			set(typed)
		}
	}
	walk("$", "", root, func(any) {})
	return result
}

// compileMutation 返回编译无 error 的 Program；Compile panic 记为失败。
func compileMutation(t *testing.T, label string, data []byte, environment CompileEnvironment) (program *Program) {
	t.Helper()
	definition, err := Parse(data)
	if err != nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("%s: Compile panicked: %v", label, recovered)
			program = nil
		}
	}()
	program, diagnostics := Compile(definition, environment)
	if diagnosticsHaveErrors(diagnostics) {
		return nil
	}
	return program
}

// runMutation 用五种驱动方式施法，只把 ErrProgramInvariant 与 panic 当作失败：
// 输入不匹配、Host 拒绝、资源不足等是变异后定义的正常结果。
func runMutation(program *Program, fixture fixtureCase, environment CompileEnvironment) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("runtime panicked: %v", recovered)
		}
	}()
	invariant := func(stage string, candidate error) error {
		if candidate != nil && errors.Is(candidate, ErrProgramInvariant) {
			return fmt.Errorf("%s: %w", stage, candidate)
		}
		return nil
	}
	if fixture.passive {
		runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{MatchSeed: fixedTestSeed(23)})
		_, activateErr := runtime.ActivatePassive(program, EventContext{EventID: 1, RootEventID: 1, Source: 2, Owner: 1, Target: 1, Result: fixture.passiveResult})
		if failure := invariant("passive", activateErr); failure != nil {
			return failure
		}
		for tick := Tick(0); tick <= 20; tick++ {
			if failure := invariant(fmt.Sprintf("advance %d", tick), runtime.Advance(tick)); failure != nil {
				return failure
			}
		}
		return nil
	}
	everyInput := CastInput{Caster: 1, Target: 2, StartPosition: positionPointer(Position{}), EndPosition: positionPointer(Position{X: 4}), Path: []Position{{}, {X: 4}}, Direction: &Direction{X: 1}}
	inputs := []CastInput{fixture.input, {Caster: 1, Target: 2}, everyInput, fixture.input, fixture.input}
	for variant, input := range inputs {
		runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{MatchSeed: fixedTestSeed(23)})
		castID, activateErr := runtime.Activate(program, input)
		if failure := invariant(fmt.Sprintf("activate[%d]", variant), activateErr); failure != nil {
			return failure
		}
		if activateErr != nil {
			continue
		}
		for tick := Tick(0); tick <= 40; tick++ {
			switch {
			case variant == 3 && tick == 2:
				if failure := invariant("cancel", runtime.Cancel(castID)); failure != nil {
					return failure
				}
			case variant == 4 && tick == 1:
				for _, tag := range program.cast.interruptTags {
					if failure := invariant("interrupt", runtime.Interrupt(castID, tag)); failure != nil {
						return failure
					}
				}
				for _, port := range []InputPort{InputPortDirectionChanged, InputPortTargetChanged} {
					if failure := invariant("input", runtime.Input(castID, port, InputPayload{Target: 2, Direction: &Direction{X: 1}})); failure != nil {
						return failure
					}
				}
			case tick == 5:
				if failure := invariant("release", runtime.Release(castID)); failure != nil {
					return failure
				}
			}
			if failure := invariant(fmt.Sprintf("advance[%d] %d", variant, tick), runtime.Advance(tick)); failure != nil {
				return failure
			}
		}
		if _, checkpointErr := runtime.Checkpoint(); checkpointErr != nil {
			if failure := invariant("checkpoint", checkpointErr); failure != nil {
				return failure
			}
		}
	}
	return nil
}

// mutationRaceStride 由 compile_mutation_race_test.go 在 -race 构建里改成 40：race 下
// 单个变异的施法慢一个数量级，全量会让包测试超过默认的 10 分钟。
var mutationRaceStride = 1

// mutationStride 是变异的抽样步长（按变异序号确定性抽样，编译与施法都只处理被抽中的
// 变异）：默认全量；-short 下每 5 个取 1 个；race 构建每 40 个取 1 个。
// SKILL_MUTATION_FULL=1 打开子树移植（约 3 万个变异），不受步长影响以外的限制。
func mutationStride() int {
	stride := mutationRaceStride
	if testing.Short() && stride < 5 {
		stride = 5
	}
	return stride
}

func TestCompiledMutationsNeverHitProgramInvariant(t *testing.T) {
	environment := DefaultCompileEnvironment()
	seeds := mutationSeeds(t)
	library := buildMutationLibrary(t, seeds)
	transplant := os.Getenv("SKILL_MUTATION_FULL") != ""
	stride := mutationStride()
	total, accepted := 0, 0
	reported := map[string]bool{}
	for _, seed := range seeds {
		fixture := seed.fixture
		baseline := compileMutation(t, seed.name, seed.data, environment)
		if baseline == nil {
			t.Fatalf("seed %s does not compile", seed.name)
		}
		if err := runMutation(baseline, fixture, environment); err != nil {
			t.Errorf("seed %s: %v", seed.name, err)
		}
		for _, mutation := range mutateDefinition(t, seed.data, library, transplant) {
			total++
			if total%stride != 0 {
				continue
			}
			program := compileMutation(t, seed.name+" "+mutation.desc, mutation.data, environment)
			if program == nil {
				continue
			}
			accepted++
			if err := runMutation(program, fixture, environment); err != nil {
				// 同一字段的同类失败只报一次，避免一个缺口刷屏。
				field := strings.SplitN(mutation.desc, "=", 2)[0]
				if key := seed.name + "|" + field; !reported[key] {
					reported[key] = true
					t.Errorf("%s %s: compiled without errors but %v", seed.name, mutation.desc, err)
				}
			}
		}
	}
	t.Logf("seeds=%d mutations=%d stride=%d compiled_and_run=%d transplant=%v", len(seeds), total, stride, accepted, transplant)
	if accepted == 0 {
		t.Fatal("mutation property exercised nothing")
	}
}

// 第二个性质：gameplay digest 覆盖 Program 的执行内容。只编译，不施法。
func TestCompiledMutationsChangeDigestWhenProgramChanges(t *testing.T) {
	environment := DefaultCompileEnvironment()
	seeds := mutationSeeds(t)
	library := buildMutationLibrary(t, seeds)
	executionOnly := func(program *Program) Program {
		copy := *program
		copy.name, copy.description, copy.identity = "", "", programIdentity{}
		copy.visuals, copy.castVisual, copy.hasCastVisual = nil, 0, false
		copy.visualCatalogRevision, copy.visualCatalogDigest = "", ""
		return copy
	}
	compared, index, stride := 0, 0, mutationStride()
	for _, seed := range seeds {
		baseline := compileMutation(t, seed.name, seed.data, environment)
		if baseline == nil {
			t.Fatalf("seed %s does not compile", seed.name)
		}
		for _, mutation := range mutateDefinition(t, seed.data, library, false) {
			if index++; index%stride != 0 {
				continue
			}
			// 表现（visual / presentation）与展示文本不属于 gameplay digest，由 presentation digest 覆盖。
			if strings.Contains(mutation.desc, "visual") || strings.Contains(mutation.desc, "presentation") || strings.Contains(mutation.desc, "$.name") || strings.Contains(mutation.desc, "$.description") {
				continue
			}
			program := compileMutation(t, seed.name+" "+mutation.desc, mutation.data, environment)
			if program == nil || program.identity.gameplayDigest != baseline.identity.gameplayDigest {
				continue
			}
			compared++
			if !reflect.DeepEqual(executionOnly(baseline), executionOnly(program)) {
				t.Errorf("%s %s: gameplay digest unchanged but the program changed", seed.name, mutation.desc)
			}
		}
	}
	t.Logf("same-digest mutations compared=%d", compared)
}
