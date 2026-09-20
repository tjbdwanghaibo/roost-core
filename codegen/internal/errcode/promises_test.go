package errcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two definitions with one code are a conflict the CSV must not paper over:
// the client would map one number to two meanings. The scan reports the code
// and both files. Removing the check left every test green (U-0030).
func TestExtractDefinitionsRejectsADuplicateCode(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"game/shop/errors.go":  "package shop\n\nimport \"github.com/tjbdwanghaibo/roost-core/errcode\"\n\nvar ErrA = errcode.Define(500101, \"shop.a\", \"a\")\n",
		"game/guild/errors.go": "package guild\n\nimport \"github.com/tjbdwanghaibo/roost-core/errcode\"\n\nvar ErrB = errcode.Define(500101, \"guild.b\", \"b\")\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := extractDefinitions(root)
	if err == nil {
		t.Fatal("two definitions of code 500101 were accepted")
	}
	for _, want := range []string{"duplicate errcode 500101", "shop", "guild"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}
