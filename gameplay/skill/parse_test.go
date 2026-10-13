package skill

import (
	"strings"
	"testing"
)

func TestParseGeneratedAcceptsDirectSkillRoot(t *testing.T) {
	result, err := ParseGenerated([]byte(minimalSkillJSON))
	if err != nil {
		t.Fatal(err)
	}
	if result.Definition == nil || result.Rejection != nil {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Definition.ID != "skill.test.minimal" {
		t.Fatalf("definition ID = %q, want skill.test.minimal", result.Definition.ID)
	}
}

func TestParseGeneratedRejectsSkillJSONWrapper(t *testing.T) {
	_, err := ParseGenerated([]byte(`{"skill_json":{}}`))
	if err == nil {
		t.Fatal("expected wrapper rejection")
	}
}

func TestParseGeneratedAcceptsRejection(t *testing.T) {
	input := `{"schema":"roost.skill/v2","error":{"code":"UNSUPPORTED_CAPABILITY","message":"missing capability","unsupported":["terrain anchor"]}}`
	result, err := ParseGenerated([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if result.Rejection == nil || result.Definition != nil {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Rejection.Error.Code != "UNSUPPORTED_CAPABILITY" {
		t.Fatalf("rejection code = %q", result.Rejection.Error.Code)
	}
}

func TestParseRejectsDuplicateNestedKey(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `"policy":{"mode":"tap"}`, `"policy":{"mode":"tap","mode":"hold"}`, 1)
	_, err := Parse([]byte(input))
	if err == nil {
		t.Fatal("expected duplicate key error")
	}
	if !strings.Contains(err.Error(), "$.activation.policy.mode") {
		t.Fatalf("error %q does not contain duplicate path", err)
	}
}

func TestParseRejectsNonCanonicalJSON(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "unknown root field",
			input: strings.Replace(minimalSkillJSON, `"id":"skill.test.minimal"`, `"id":"skill.test.minimal","meta":{}`, 1),
		},
		{
			name:  "unknown nested field",
			input: strings.Replace(minimalSkillJSON, `"policy":{"mode":"tap"}`, `"policy":{"mode":"tap","extra":true}`, 1),
		},
		{
			name:  "second JSON value",
			input: minimalSkillJSON + `{}`,
		},
		{
			name:  "trailing text",
			input: minimalSkillJSON + ` trailing`,
		},
		{
			name:  "wrong schema",
			input: strings.Replace(minimalSkillJSON, `roost.skill/v2`, `roost.skill/v3`, 1),
		},
		{
			name:  "number encoded as string",
			input: strings.Replace(minimalSkillJSON, `"cooldown_ticks":0`, `"cooldown_ticks":"0"`, 1),
		},
		{
			name:  "missing input schema",
			input: strings.Replace(minimalSkillJSON, `  "input_schema":{"type":"none"},`+"\n", "", 1),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Parse([]byte(tt.input)); err == nil {
				t.Fatal("expected parse failure")
			}
		})
	}
}

func TestParseRejectsNonCanonicalSelectFields(t *testing.T) {
	input := `{
  "schema":"roost.skill/v2",
  "id":"skill.test.select",
  "name":"Select",
  "description":"Selects a target.",
  "activation":{"type":"active","policy":{"mode":"tap"}},
  "input_schema":{"type":"none"},
  "cooldown_ticks":0,
  "costs":[],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{
    "flow":"select",
    "select":{"from":"$caster","kind":"entities","shape":{"type":"single"},"filters":[],"limit":1},
    "consume":{"mode":"one","bind":"target","then":{"flow":"finish"}}
  }}}]
}`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("expected plural kind and legacy bind to be rejected")
	}
}

func TestParseRejectsNonCanonicalEffectResultFields(t *testing.T) {
	input := `{
  "schema":"roost.skill/v2",
  "id":"skill.test.effect",
  "name":"Effect",
  "description":"Deals damage.",
  "activation":{"type":"active","policy":{"mode":"tap"}},
  "input_schema":{"type":"entity"},
  "cooldown_ticks":0,
  "costs":[],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{
    "flow":"effect",
    "effect":{"type":"damage","target":"$input.target","amount":10,"damage_type":"physical"},
    "bind":"damage_result",
    "then":{"flow":"finish"}
  }}}]
}`
	if _, err := Parse([]byte(input)); err == nil {
		t.Fatal("expected legacy effect bind/then fields to be rejected")
	}
}

func TestParseAcceptsCanonicalSelectConsumersAndEffectResult(t *testing.T) {
	input := `{
  "schema":"roost.skill/v2",
  "id":"skill.test.canonical",
  "name":"Canonical",
  "description":"Exercises canonical variants.",
  "activation":{"type":"active","policy":{"mode":"tap"}},
  "input_schema":{"type":"none"},
  "cooldown_ticks":0,
  "costs":[],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{
    "flow":"select",
    "select":{"from":"$caster","kind":"entity","shape":{"type":"single"},"filters":[],"limit":1},
    "consume":{"mode":"one","as":"target","then":{
      "flow":"effect",
      "effect":{"type":"damage","target":"$local.target","amount":10,"damage_type":"physical"},
      "result":{"as":"damage_result","success":{"flow":"finish","reason":"done"},"failure":{"flow":"finish","reason":"failed"}}
    }},
    "on_empty":{"flow":"finish","reason":"empty"}
  }}}]
}`
	definition := mustParseJSON(t, input)
	if len(definition.Phases) != 1 {
		t.Fatalf("phase count = %d, want 1", len(definition.Phases))
	}
}

func TestParseRejectsFieldsFromAnotherEffectVariant(t *testing.T) {
	tests := []struct {
		name   string
		effect string
	}{
		{
			name:   "knockback rejects toward",
			effect: `{"type":"knockback","target":"$caster","from":"$caster.position","toward":"$caster.position","distance":2}`,
		},
		{
			name:   "pull rejects from",
			effect: `{"type":"pull","target":"$caster","from":"$caster.position","toward":"$caster.position","distance":2}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"effect","effect":`+tt.effect+`}`, 1)
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("expected cross-variant field rejection")
			}
		})
	}
}

// Parse 拒绝未知字段与重复键，键按字节精确匹配：{"id":…,"ID":…} 与 "Cooldown_Ticks" 这类大小写变体都拒绝
// （encoding/json 默认大小写不敏感）。名字作键的 map（memory 等）不受大小写规则约束。
func TestParseRejectsCaseVariantKeys(t *testing.T) {
	replace := func(old, new string) string {
		if !strings.Contains(minimalSkillJSON, old) {
			t.Fatalf("fixture does not contain %q", old)
		}
		return strings.Replace(minimalSkillJSON, old, new, 1)
	}
	cases := map[string]string{
		"top-level duplicate by case":    replace(`"id":"skill.test.minimal",`, `"id":"skill.test.minimal","ID":"skill.test.other",`),
		"top-level non-canonical key":    replace(`"cooldown_ticks":0`, `"Cooldown_Ticks":0`),
		"nested duplicate by case":       replace(`{"flow":"finish","reason":"done"}`, `{"flow":"finish","reason":"done","Reason":"other"}`),
		"nested header by case":          replace(`{"flow":"finish","reason":"done"}`, `{"FLOW":"finish","reason":"done"}`),
		"phase field by case":            replace(`"timeout_ticks":0`, `"Timeout_Ticks":0`),
		"activation policy by case":      replace(`"policy":{"mode":"tap"}`, `"policy":{"Mode":"tap"}`),
		"value expression by case":       replace(`"costs":[]`, `"costs":[{"resource":"mana","amount":{"op":"add","Args":[1,2]}}]`),
		"memory declaration field case":  replace(`"memory":{}`, `"memory":{"counter":{"type":"int","Default":0}}`),
		"presentation field by case":     replace(`"memory":{}`, `"memory":{},"presentation":{"Icon_Keywords":["fire"]}`),
		"generated rejection field case": `{"schema":"roost.skill/v2","error":{"code":"UNSUPPORTED_CAPABILITY","Message":"x","message":"y"}}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if strings.HasPrefix(name, "generated") {
				if result, err := ParseGenerated([]byte(input)); err == nil {
					t.Fatalf("ParseGenerated accepted a case-variant key: %#v", result.Rejection)
				}
				return
			}
			if definition, err := Parse([]byte(input)); err == nil {
				t.Fatalf("Parse accepted a case-variant or unknown key; id=%q cooldown=%d", definition.ID, definition.CooldownTicks)
			}
		})
	}
}

// 控制：以名字为键的 map 照常区分大小写，规范键的定义照常通过。
func TestParseKeepsNameKeyedMapsAndCanonicalKeys(t *testing.T) {
	if _, err := Parse([]byte(minimalSkillJSON)); err != nil {
		t.Fatalf("canonical definition rejected: %v", err)
	}
	input := strings.Replace(minimalSkillJSON, `"memory":{}`, `"memory":{"counter":{"type":"int","default":0},"Counter":{"type":"int","default":1}}`, 1)
	definition, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("distinct memory names rejected at Parse: %v", err)
	}
	if len(definition.Memory) != 2 {
		t.Fatalf("memory = %#v, want two distinct names", definition.Memory)
	}
}

// 源文档里任何一处变化——效果 / 策略 / 输入 / 形状 / 过滤器的类型、消耗数量、cast window 表达式——都改变
// 源文档 digest（ProgramIdentityView.SourceDocumentDigest）；同一份文档摘要稳定。摘要不能取 json.Marshal(Definition)：
// 接口字段不带类型、`json:"-"` 字段被跳过。历史：RR-20261006-33。

func digestIntValue(value int64) Value { return Value{Node: IntValueDefinition{Value: value}} }

// digestBaseDefinition 是一份只用来求源文档 digest 的定义（不编译）；edit 改其中一处。
func digestBaseDefinition(edit func(*Definition)) *Definition {
	definition := &Definition{
		Schema: "roost.skill/v2", ID: "skill.test.digest", Name: "Digest", Description: "Source document digest.",
		Activation:   ActiveActivationDefinition{Policy: TapPolicyDefinition{}},
		InputSchema:  NoneInputSchemaDefinition{},
		InitialPhase: "cast",
		Phases:       []PhaseDefinition{{ID: "cast", On: PhaseEventsDefinition{Enter: FinishFlowDefinition{}}}},
	}
	edit(definition)
	return definition
}

func digestEnter(flow FlowDefinition) func(*Definition) {
	return func(definition *Definition) { definition.Phases[0].On.Enter = flow }
}

func digestSelect(shape ShapeDefinition, filters ...FilterDefinition) func(*Definition) {
	return digestEnter(SelectFlowDefinition{
		Select:  SelectDefinition{From: Value{Node: ReferenceValueDefinition{Reference: "$caster"}}, Kind: "entity", Shape: shape, Filters: filters, Limit: 1},
		Consume: SelectEachConsumeDefinition{As: "target", Do: FinishFlowDefinition{}},
	})
}

func TestSourceDocumentDigestSeesEveryDifference(t *testing.T) {
	maximumRange := int64(10)
	toggle := TogglePolicyDefinition{PulseIntervalTicks: 5, MaxDurationTicks: 50, SustainCosts: []Cost{{Resource: "mana", Amount: digestIntValue(1)}}}
	hold := HoldPolicyDefinition{PulseIntervalTicks: 5, MaxDurationTicks: 50, SustainCosts: []Cost{{Resource: "mana", Amount: digestIntValue(1)}}}
	window := func(windup, recovery int64) func(*Definition) {
		return func(definition *Definition) {
			definition.Activation = ActiveActivationDefinition{Policy: TapPolicyDefinition{}, CastWindow: &CastWindowDefinition{
				WindupTicksExpression: digestIntValue(windup), WindupTicksMax: 10, RecoveryTicksExpression: digestIntValue(recovery), RecoveryTicksMax: 10,
			}}
		}
	}
	for _, test := range []struct {
		name        string
		left, right func(*Definition)
	}{
		{"effect type set_memory / add_memory",
			digestEnter(EffectFlowDefinition{Effect: SetMemoryEffectDefinition{Name: "x", Value: digestIntValue(1)}}),
			digestEnter(EffectFlowDefinition{Effect: AddMemoryEffectDefinition{Name: "x", Value: digestIntValue(1)}})},
		{"cost amount",
			func(definition *Definition) {
				definition.Costs = []Cost{{Resource: "mana", Amount: digestIntValue(10)}}
			},
			func(definition *Definition) {
				definition.Costs = []Cost{{Resource: "mana", Amount: digestIntValue(20)}}
			}},
		{"cast window windup expression", window(1, 1), window(2, 1)},
		{"cast window recovery expression", window(1, 1), window(1, 2)},
		{"cast policy toggle / hold",
			func(definition *Definition) { definition.Activation = ActiveActivationDefinition{Policy: toggle} },
			func(definition *Definition) { definition.Activation = ActiveActivationDefinition{Policy: hold} }},
		{"input schema entity / direction",
			func(definition *Definition) {
				definition.InputSchema = EntityInputSchemaDefinition{MaximumRange: &maximumRange}
			},
			func(definition *Definition) {
				definition.InputSchema = DirectionInputSchemaDefinition{MaximumRange: &maximumRange}
			}},
		{"input schema drag / two_point",
			func(definition *Definition) {
				definition.InputSchema = DragInputSchemaDefinition{MaximumRange: 10, MinimumLength: 1, MaximumLength: 5, ClampPolicy: "reject"}
			},
			func(definition *Definition) {
				definition.InputSchema = TwoPointInputSchemaDefinition{MaximumRange: 10, MinimumLength: 1, MaximumLength: 5, ClampPolicy: "reject"}
			}},
		{"select shape line / rectangle",
			digestSelect(LineShapeDefinition{Length: digestIntValue(4), Width: digestIntValue(1), Direction: Value{Node: ReferenceValueDefinition{Reference: "$input.direction"}}}),
			digestSelect(RectangleShapeDefinition{Length: digestIntValue(4), Width: digestIntValue(1), Direction: Value{Node: ReferenceValueDefinition{Reference: "$input.direction"}}})},
		{"select shape single / owned_entities", digestSelect(SingleShapeDefinition{}), digestSelect(OwnedEntitiesShapeDefinition{})},
		{"select filter ability_tag / owned_entity_tag",
			digestSelect(SingleShapeDefinition{}, AbilityTagFilterDefinition{Tag: "spell"}),
			digestSelect(SingleShapeDefinition{}, OwnedEntityTagFilterDefinition{Tag: "spell"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			left, right := canonicalDefinitionDigest(digestBaseDefinition(test.left)), canonicalDefinitionDigest(digestBaseDefinition(test.right))
			if left == right {
				t.Errorf("source document digest %s for both definitions; they differ in %s", left, test.name)
			}
			if again := canonicalDefinitionDigest(digestBaseDefinition(test.left)); again != left {
				t.Errorf("source document digest not stable: %s then %s", left, again)
			}
		})
	}
}

// 维护者报告的现象，端到端：同一份 JSON 只把 set_memory 改成 add_memory，编译后 gameplay digest 变了，源文档 digest 也要变。
func TestSourceDocumentDigestSeesTheEffectType(t *testing.T) {
	base := strings.Replace(minimalSkillJSON, `"memory":{}`, `"memory":{"x":{"type":"int","default":0}}`, 1)
	base = strings.Replace(base, `{"flow":"finish","reason":"done"}`, `{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"set_memory","name":"x","value":1}},{"flow":"finish"}]}`, 1)
	changed := strings.Replace(base, `"set_memory"`, `"add_memory"`, 1)
	setIdentity := InspectIdentity(mustCompileProgram(t, mustParseJSON(t, base)))
	addIdentity := InspectIdentity(mustCompileProgram(t, mustParseJSON(t, changed)))
	if setIdentity.GameplayDigest == addIdentity.GameplayDigest {
		t.Fatalf("gameplay digest %s for set_memory and add_memory", setIdentity.GameplayDigest)
	}
	if setIdentity.SourceDocumentDigest == addIdentity.SourceDocumentDigest {
		t.Errorf("source document digest %s for set_memory and add_memory: the source changed, the digest did not", setIdentity.SourceDocumentDigest)
	}
}
