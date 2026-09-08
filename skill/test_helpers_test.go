package skill

import (
	"os"
	"path/filepath"
	"testing"
)

const minimalSkillJSON = `{
  "schema":"roost.skill/v2",
  "id":"skill.test.minimal",
  "name":"Minimal",
  "description":"Immediately finishes.",
  "activation":{"type":"active","policy":{"mode":"tap"}},
  "input_schema":{"type":"none"},
  "cooldown_ticks":0,
  "costs":[],
  "memory":{},
  "initial_phase":"cast",
  "phases":[{
    "id":"cast",
    "timeout_ticks":0,
    "on":{"enter":{"flow":"finish","reason":"done"}}
  }]
}`

func mustParseJSON(t *testing.T, input string) *Definition {
	t.Helper()
	definition, err := Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
