package skill

// RR-20261006-33：源文档 digest（ProgramIdentityView.SourceDocumentDigest）之前是 json.Marshal(Definition) 的摘要。
// Definition 里的接口字段（effect、cast policy、input schema、select 的 shape / filter ...）只按具体类型的字段编码、
// 不带类型，字段完全相同只差类型的两个定义摘要相同；带 `json:"-"` 的字段（Cost.Amount、cast window 的
// windup / recovery 表达式）整个被跳过，只改它们摘要也不变。实测：set_memory 改成 add_memory，gameplay digest 变了，
// 源文档 digest 不变。
//
// 承诺：源文档里任何一处变化——效果 / 策略 / 输入 / 形状 / 过滤器的类型、消耗数量、cast window 表达式——都改变
// 源文档 digest；同一份文档摘要稳定。

import (
	"strings"
	"testing"
)

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
