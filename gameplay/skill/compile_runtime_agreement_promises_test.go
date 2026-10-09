package skill

import (
	"strings"
	"testing"
)

// 本文件钉住 N09 第四批（RR-20261005-NC-210 / 212 / 213 / 214 / 215）的同一条承诺：
// 编译器接受的定义，Runtime 和参考 Host 都按作者写的意思执行。旧实现里这几类字段
// 编译期不查，lower 用 map 零值兜底（名字查不到就得到槽位 0 / handle 0），或者字段
// 根本没有传到 Host，于是要么静默改错对象，要么每次施法都失败。

func agreementSkillJSON(input, memory, costs, enter string) string {
	text := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, enter, 1)
	text = strings.Replace(text, `"input_schema":{"type":"none"}`, `"input_schema":{"type":"`+input+`"}`, 1)
	text = strings.Replace(text, `"memory":{}`, `"memory":`+memory, 1)
	return strings.Replace(text, `"costs":[]`, `"costs":`+costs, 1)
}

func agreementSteps(steps ...string) string {
	return `{"flow":"sequence","steps":[` + strings.Join(append(steps, `{"flow":"finish"}`), ",") + `]}`
}

func agreementEffect(effect string) string { return `{"flow":"effect","effect":` + effect + `}` }

func agreementSelect(filter string) string {
	return `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":10},"filters":[` + filter + `],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"heal","target":"$local.t","amount":1}}}}`
}

type agreementCase struct {
	name, input, code, path string
}

func requireAgreementRejected(t *testing.T, cases []agreementCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			program, diagnostics := Compile(mustParseJSON(t, test.input), DefaultCompileEnvironment())
			if program != nil {
				t.Fatalf("%s compiled; diagnostics=%#v", test.name, diagnostics)
			}
			requireDiagnosticAt(t, diagnostics, DiagnosticCode(test.code), test.path)
		})
	}
}

const agreementEnterPath = "$.phases[0].on.enter.steps[0]"

// RR-20261005-NC-210：set_memory / add_memory / clear_memory 的 name 必须是声明过的
// memory，add_memory 只能用在 int memory 上。旧实现不查名字，lower 用
// `c.memory[name]` 的零值 0：有别的 memory 时静默写进槽位 0（类型也不检查），
// 没有 memory 时 executeMemory 返回 ErrProgramInvariant；bool memory 上的
// add_memory 每次施法 ErrRuntimeTypeMismatch。
func TestCompileRejectsMemoryEffectsOnUndeclaredOrNonIntMemory(t *testing.T) {
	declared := `{"count":{"type":"int","default":0},"flag":{"type":"bool","default":false}}`
	requireAgreementRejected(t, []agreementCase{
		{"set undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"missing","value":true}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"add undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"missing","value":1}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"clear undeclared", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"clear_memory","name":"missing"}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"set without any memory", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"charged","value":true}`))), string(DiagnosticReferenceUnknown), agreementEnterPath + ".effect.name"},
		{"add on bool memory", agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"flag","value":true}`))), string(DiagnosticTypeMismatch), agreementEnterPath + ".effect.name"},
	})
}

// RR-20261005-NC-210 原触发的运行期后果：声明了 count 的技能里 set_memory 一个未声明
// 的名字，旧实现把 bool 写进 count 的槽位（槽位 0）。修复后它不再能编译，这里同时
// 钉住控制：声明过的名字照常写自己的槽位。
func TestDeclaredMemoryEffectsWriteTheirOwnSlot(t *testing.T) {
	declared := `{"count":{"type":"int","default":0},"flag":{"type":"bool","default":false}}`
	input := agreementSkillJSON("none", declared, "[]", agreementSteps(agreementEffect(`{"type":"add_memory","name":"count","value":2}`), agreementEffect(`{"type":"set_memory","name":"flag","value":true}`)))
	program, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, diagnostics)
	host := runtimeTestHost(DefaultCompileEnvironment())
	runtime := NewRuntime(host, RuntimeOptions{})
	if _, err := runtime.Activate(program, CastInput{Caster: 1}); err != nil {
		t.Fatal(err)
	}
}

// RR-20261005-NC-212：catalog 里同一类条目的 key 必须唯一。旧实现只查 handle 唯一，
// 两个 unit template 用同一个 key 时，authority 表取后一个的 handle，typecheck 的
// unitTemplateEntry 取前一个的参数策略：bool 绑定到后一个 template 的 int 参数也能
// 编译，单个条目时同一定义被 TYPE_MISMATCH 拒绝。
func TestCompileEnvironmentRejectsDuplicateCatalogKeys(t *testing.T) {
	environment := DefaultCompileEnvironment()
	second := environment.Gameplay.UnitTemplates.Entries[0]
	second.Handle = 2
	second.Parameters = []UnitTemplateParameterPolicy{{Name: "power", ValueType: valueKindInt, Quantity: quantityCount, Minimum: 0, Maximum: 10}}
	environment.Gameplay.UnitTemplates.Entries = append(environment.Gameplay.UnitTemplates.Entries, second)
	environment.Digest = authorityDigest(environment)
	requireDiagnosticAt(t, validateCompileEnvironment(environment), DiagnosticCatalogDuplicateHandle, "$.gameplay.unit_templates[1].key")

	input := strings.Replace(string(mustReadFixture(t, "owned_trap.json")), `"duration_ticks":10}`, `"duration_ticks":10,"parameter_bindings":{"power":true}}`, 1)
	program, diagnostics := Compile(mustParseJSON(t, input), environment)
	if program != nil {
		t.Fatalf("duplicate unit template key compiled a bool binding for an int parameter; diagnostics=%#v", diagnostics)
	}

	for name, mutate := range map[string]func(*CompileEnvironment){
		"attributes": func(e *CompileEnvironment) {
			entry := e.Gameplay.Attributes.Entries[0]
			entry.Handle = 9
			e.Gameplay.Attributes.Entries = append(e.Gameplay.Attributes.Entries, entry)
		},
		"statuses": func(e *CompileEnvironment) {
			entry := e.Gameplay.Statuses.Entries[0]
			entry.Handle = 9
			e.Gameplay.Statuses.Entries = append(e.Gameplay.Statuses.Entries, entry)
		},
		"resources": func(e *CompileEnvironment) {
			e.Gameplay.Resources.Entries = append(e.Gameplay.Resources.Entries, ResourceCatalogEntry{Handle: 9, Key: "mana", Maximum: 1})
		},
		"tags": func(e *CompileEnvironment) {
			e.Gameplay.Tags.Entries = append(e.Gameplay.Tags.Entries, GameplayTagCatalogEntry{Handle: 9, Key: "spell", Classes: GameplayTagDeclarable})
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			mutate(&environment)
			environment.Digest = authorityDigest(environment)
			if !diagnosticsHaveErrors(validateCompileEnvironment(environment)) {
				t.Fatalf("duplicate %s key accepted", name)
			}
		})
	}
}

// RR-20261005-NC-213：Runtime / Host 不执行的字段不能编译通过。chain 的
// allow_repeat 与 hop_interval_ticks、attribute_modifier 的 stack_policy 与
// max_stacks 都被 lower 进 Program、算进摘要，但 ChainSelectShape /
// AttributeModifierCommand 没有对应字段，作者写的语义被静默丢掉。
func TestCompileRejectsFieldsTheRuntimeDoesNotExecute(t *testing.T) {
	chain := func(shape string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(`{"flow":"select","select":{"from":"$input.target","kind":"entity","shape":{"type":"chain","hop_range":1,"max_targets":2,`+shape+`},"filters":[],"order":{"by":"stable_id","direction":"asc"},"limit":2},"consume":{"mode":"each","as":"t","do":{"flow":"effect","effect":{"type":"damage","target":"$local.t","amount":1,"damage_type":"physical"}}}}`))
	}
	modifier := func(extra string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"add","value":5,"duration_ticks":10`+extra+`}`)))
	}
	requireAgreementRejected(t, []agreementCase{
		{"chain allow_repeat", chain(`"allow_repeat":true,"hop_interval_ticks":0`), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.shape.allow_repeat"},
		{"chain hop_interval_ticks", chain(`"allow_repeat":false,"hop_interval_ticks":3`), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.shape.hop_interval_ticks"},
		{"modifier stack_policy", modifier(`,"stack_policy":"stack"`), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.stack_policy"},
		{"modifier max_stacks", modifier(`,"max_stacks":3`), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.max_stacks"},
	})
	for name, input := range map[string]string{
		"chain defaults":    chain(`"allow_repeat":false,"hop_interval_ticks":0`),
		"modifier defaults": modifier(""),
	} {
		t.Run("control "+name, func(t *testing.T) {
			_, diagnostics := Compile(mustParseJSON(t, input), DefaultCompileEnvironment())
			requireNoErrors(t, diagnostics)
		})
	}
}

// RR-20261005-NC-214：effect、filter、cost 里引用的 status / attribute / resource
// 名字必须在 catalog 里。旧实现只有 damage_type、element、gameplay tag、collision、
// unit template 等查了；这几处由 lookup*Handle 的 map 零值兜底成 handle 0：
// add_status / resource / cost 每次施法被 Host 拒绝，remove_status、has_status、
// attribute_modifier、attribute_compare 静默作用在 handle 0 上。
func TestCompileRejectsUnknownCatalogNames(t *testing.T) {
	requireAgreementRejected(t, []agreementCase{
		{"add_status", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"missing","duration_ticks":5,"stacks":1}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.status"},
		{"remove_status", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"remove_status","target":"$input.target","status":"missing"}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.status"},
		{"attribute_modifier", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"missing","operation":"add","value":5,"duration_ticks":10}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.attribute"},
		{"resource", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"missing","operation":"add","amount":1}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".effect.resource"},
		{"cost", agreementSkillJSON("none", "{}", `[{"resource":"missing","amount":1}]`, agreementSteps(agreementEffect(`{"type":"heal","target":"$caster","amount":1}`))), string(DiagnosticCapabilityUnknown), "$.costs[0].resource"},
		{"has_status filter", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"has_status","status":"missing"}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".select.filters[0].status"},
		{"attribute_compare filter", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"attribute_compare","attribute":"missing","op":"gt","value":0}`))), string(DiagnosticCapabilityUnknown), agreementEnterPath + ".select.filters[0].attribute"},
	})
	sustain := strings.Replace(agreementSkillJSON("none", "{}", "[]", `{"flow":"effect","effect":{"type":"heal","target":"$caster","amount":1}}`), `"policy":{"mode":"tap"}`, `"policy":{"mode":"toggle","pulse_interval_ticks":2,"max_duration_ticks":10,"sustain_costs":[{"resource":"missing","amount":1}]}`, 1)
	sustain = strings.Replace(sustain, `"on":{"enter":`, `"on":{"release":{"flow":"finish"},"enter":`, 1)
	requireAgreementRejected(t, []agreementCase{{"sustain cost", sustain, string(DiagnosticCapabilityUnknown), "$.activation.policy.sustain_costs[0].resource"}})
}

// RR-20261005-NC-215：两个参考 Host（MemoryHost、combatcomponent）与 Runtime 自己都
// 拒绝的取值，编译期就拒绝：attribute_modifier 的 operation 不在属性的
// ModifierOperations 里、duration_ticks <= 0，add_status duration_ticks 为 0，
// resource 的 operation 不是 set / add / spend / sub，cost 的字面量为负，
// attribute_compare 的 op 不是比较运算。旧实现这些都编译通过，施法时每次失败
// （attribute_compare 是静默全部不匹配）。
func TestCompileRejectsValuesEveryHostRejects(t *testing.T) {
	modifier := func(operation, duration string) string {
		return agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"`+operation+`","value":5,"duration_ticks":`+duration+`}`)))
	}
	requireAgreementRejected(t, []agreementCase{
		{"modifier operation", modifier("set", "10"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.operation"},
		{"modifier zero duration", modifier("add", "0"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"modifier negative duration", modifier("add", "-4"), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"add_status zero duration", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":0,"stacks":1}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.duration_ticks"},
		{"resource operation", agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"mana","operation":"mul_bp","amount":1}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".effect.operation"},
		{"negative cost", agreementSkillJSON("none", "{}", `[{"resource":"mana","amount":-1}]`, agreementSteps(agreementEffect(`{"type":"heal","target":"$caster","amount":1}`))), string(DiagnosticShapeInvalid), "$.costs[0].amount"},
		{"attribute_compare op", agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"attribute_compare","attribute":"health","op":"zz","value":0}`))), string(DiagnosticShapeInvalid), agreementEnterPath + ".select.filters[0].op"},
	})
}

// 控制：合法取值照常编译并在 MemoryHost 上施法成功；名字、取值与 catalog 一致时
// 本批新增的检查不能误报。
func TestCatalogConsistentEffectsStillCompileAndRun(t *testing.T) {
	cases := map[string]string{
		"modifier":   agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"attribute_modifier","target":"$input.target","attribute":"move_speed","operation":"mul_bp","value":9000,"duration_ticks":10}`))),
		"status":     agreementSkillJSON("entity", "{}", "[]", agreementSteps(agreementEffect(`{"type":"add_status","target":"$input.target","status":"slow","duration_ticks":5,"stacks":1}`), agreementEffect(`{"type":"remove_status","target":"$input.target","status":"slow"}`))),
		"resource":   agreementSkillJSON("entity", "{}", `[{"resource":"mana","amount":0}]`, agreementSteps(agreementEffect(`{"type":"resource","target":"$caster","resource":"mana","operation":"spend","amount":1}`))),
		"filters":    agreementSkillJSON("none", "{}", "[]", agreementSteps(agreementSelect(`{"type":"missing_status","status":"slow"},{"type":"attribute_compare","attribute":"health","op":"gte","value":0}`))),
		"chain":      string(mustReadFixture(t, "chain_lightning.json")),
		"memory set": agreementSkillJSON("none", `{"flag":{"type":"bool","default":false}}`, "[]", agreementSteps(agreementEffect(`{"type":"set_memory","name":"flag","value":true}`), agreementEffect(`{"type":"clear_memory","name":"flag"}`))),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			environment := DefaultCompileEnvironment()
			program, diagnostics := Compile(mustParseJSON(t, input), environment)
			requireNoErrors(t, diagnostics)
			castInput := CastInput{Caster: 1}
			if strings.Contains(input, `"input_schema":{"type":"entity"}`) || name == "chain" {
				castInput.Target = 2
			}
			runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
			castID, err := runtime.Activate(program, castInput)
			if err != nil {
				t.Fatal(err)
			}
			if cast, _ := runtime.InspectCast(castID); cast.Status != CastFinished {
				t.Fatalf("cast = %#v, want finished", cast)
			}
		})
	}
}
