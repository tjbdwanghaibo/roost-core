package configschema

import (
	"math"
	"strings"
	"testing"
)

func TestNonFiniteAndOverflowingConfigNumbersAreRejected(t *testing.T) {
	for _, raw := range []any{"NaN", "+Inf", math.NaN(), math.Inf(-1)} {
		if got, err := ParseFloat("ratio", raw); err == nil {
			t.Errorf("nonfinite %v accepted: %v", raw, got)
		}
	}
	for _, raw := range []any{uint64(1) << 63, uint64(math.MaxUint64), uint(math.MaxUint64)} {
		if got, err := ParseInt("count", raw); err == nil {
			t.Errorf("overflow %v accepted as %d", raw, got)
		}
	}
}
func TestUnderscoreClosedChecksBothSchemaForms(t *testing.T) {
	type cfg struct {
		Result struct {
			Wait int `config:"wait"`
		} `config:"saga.result_,closed"`
	}
	schema := MustOf(cfg{})
	src := source(t, map[string]any{"saga.result_wait": 1, "saga.result_typo": 2})
	for _, s := range []Schema{schema, {Keys: schema.Keys}} {
		errs := s.Check(src, false)
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), "result_typo") {
			t.Fatalf("closed underscore prefix accepted unknown key: %v", errs)
		}
	}
}
func TestMapSourcePreservesUnderscoreYAMLHierarchy(t *testing.T) {
	src := NewMapSource(map[string]any{"saga": map[string]any{"result_": map[string]any{"wait": 5}}})
	if _, found := src.Get("saga.result_wait"); found {
		t.Fatal("nested YAML silently treated as flattened runtime key")
	}
	if got, found := src.Get("saga.result_.wait"); !found || got != 5 {
		t.Fatalf("runtime YAML path missing: %v %v", got, found)
	}
}
