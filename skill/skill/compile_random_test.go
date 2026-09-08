package skill

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileRandomSitesAreDeterministic(t *testing.T) {
	input := strings.Replace(minimalSkillJSON, `{"flow":"finish","reason":"done"}`, `{"flow":"select","select":{"from":"$caster","kind":"entity","shape":{"type":"circle","radius":5},"filters":[],"order":{"by":"random","direction":"asc"},"limit":1},"consume":{"mode":"one","as":"target","then":{"flow":"finish"}},"on_empty":{"flow":"finish"}}`, 1)
	a, da := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	b, db := compileToArtifacts(mustParseJSON(t, input), DefaultCompileEnvironment())
	requireNoErrors(t, da)
	requireNoErrors(t, db)
	if !reflect.DeepEqual(a.identity, b.identity) {
		t.Fatalf("identity/random artifacts differ: %#v %#v", a.identity, b.identity)
	}
}
