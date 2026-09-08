package ai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func expectTreeErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// Registrations are addressed by name from documents; a nameless or
// duplicated factory would make a document bind to the wrong behaviour.
func TestTreeRegistryRefusesNamelessAndDuplicateFactories(t *testing.T) {
	registry := NewRegistry[treeCtx](nowOf)
	factory := func(json.RawMessage) (Node[treeCtx], error) {
		return &Condition[treeCtx]{Check: func(*treeCtx) bool { return true }}, nil
	}
	expectTreeErr(t, registry.RegisterCondition("", factory), ErrTreeInvalid, "condition registration requires a name and factory")
	expectTreeErr(t, registry.RegisterCondition("ok", nil), ErrTreeInvalid, "condition registration requires a name and factory")
	if err := registry.RegisterCondition("ok", factory); err != nil {
		t.Fatal(err)
	}
	expectTreeErr(t, registry.RegisterCondition("ok", factory), ErrTreeInvalid, `duplicate condition "ok"`)
	expectTreeErr(t, registry.RegisterAction("", factory), ErrTreeInvalid, "action registration requires a kind and factory")
	if err := registry.RegisterAction("go", factory); err != nil {
		t.Fatal(err)
	}
	expectTreeErr(t, registry.RegisterAction("go", factory), ErrTreeInvalid, `duplicate action "go"`)
}

// The earlier fail-fast test only checked the error class; each rule is
// pinned here by its message and JSON path, so a dropped rule turns exactly
// one case red instead of being caught by a neighbour.
func TestParseTreeRefusesEachDefectByMessageAndPath(t *testing.T) {
	registry := wireRegistry(t)
	leaf := `{"node":"condition","name":"hp_below","args":{}}`
	deep := leaf
	for i := 0; i <= maxTreeDepth; i++ {
		deep = `{"node":"inverter","child":` + deep + `}`
	}
	cases := []struct{ name, document, text string }{
		{"schema", `{"schema":"roost.ai/v9","root":` + leaf + `}`, `"roost.ai/v9"`},
		{"root required", `{"schema":"roost.ai/v1"}`, "$.root is required"},
		{"missing node kind", `{"schema":"roost.ai/v1","root":{"name":"x"}}`, `$.root: missing "node"`},
		{"unknown node kind", `{"schema":"roost.ai/v1","root":{"node":"paralel"}}`, `$.root: unknown node "paralel"`},
		{"unknown action", `{"schema":"roost.ai/v1","root":{"node":"action","kind":"fly","args":{}}}`, `$.root: unknown action "fly"`},
		{"unknown condition", `{"schema":"roost.ai/v1","root":{"node":"sequence","children":[{"node":"condition","name":"nope","args":{}}]}}`, `$.root.children[0]: unknown condition "nope"`},
		{"guard without condition", `{"schema":"roost.ai/v1","root":{"node":"guard","child":` + leaf + `}}`, "$.root.condition is required"},
		{"repeat without child", `{"schema":"roost.ai/v1","root":{"node":"repeat","count":2}}`, "child is required"},
		{"cooldown ticks not positive", `{"schema":"roost.ai/v1","root":{"node":"cooldown","ticks":0,"child":` + leaf + `}}`, "$.root.ticks must be positive"},
		{"unknown parallel policy", `{"schema":"roost.ai/v1","root":{"node":"parallel","policy":"most","children":[` + leaf + `]}}`, `$.root.policy: unknown policy "most"`},
		{"too deep", `{"schema":"roost.ai/v1","root":` + deep + `}`, "exceeds depth 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseTree([]byte(tc.document), registry)
			sentinel := ErrTreeInvalid
			if tc.name == "schema" {
				sentinel = ErrTreeSchema
			}
			expectTreeErr(t, err, sentinel, tc.text)
		})
	}
}
