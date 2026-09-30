package errcode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractDefinitionsIgnoresCommentAndStringLiterals(t *testing.T) {
	root := t.TempDir()
	source := `package game
// retired: errcode.Define(500999, "ghost", "removed")
/* errcode.Define(500998, "block", "removed") */
const example = "errcode.Define(500997, \"string\", \"removed\")"
var live = errcode.Define(500101, "live", "active")
`
	if err := os.WriteFile(filepath.Join(root, "errors.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	defs, err := extractDefinitions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Code != 500101 {
		t.Fatalf("definitions = %#v", defs)
	}
}
