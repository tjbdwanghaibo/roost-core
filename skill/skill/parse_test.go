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
