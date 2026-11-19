package roost

import (
	"os"
	"path/filepath"
	"testing"
)

// This scenario depends on all three generated modules, not any one two-module recipe.
func TestGeneratedMailboxCutoverAfterLoginPullFailure(t *testing.T) {
	root, roost, _ := moduleProject(t, "mailcutover")
	roost("module", "add", "mailbox", "rehome", "enter")
	src, err := os.ReadFile(filepath.Join("testdata", "mailbox_cutover_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "game", "controllers", "mailbox_cutover_test.go"), src, 0644); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-count=3", "-run", "^TestGeneratedMailboxCutover$|^TestWithMailbox(Enter|Rehome)", "./game/controllers")
}
