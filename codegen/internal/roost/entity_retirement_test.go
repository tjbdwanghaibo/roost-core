package roost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
)

func TestStagedProjectCommitSeesEntityRetirement(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"entity"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	wire := filepath.Join(root, "game", "entities", "player", "player_gen_wire.go")
	if _, err := os.Stat(wire); err != nil {
		t.Fatalf("initial wire missing: %v", err)
	}
	source := filepath.Join(root, "game", "entities", "player", "entity.go")
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(raw), "//roost:entity", "// retired entity", 1)
	if updated == string(raw) {
		t.Fatal("scaffold did not contain entity marker")
	}
	if err := os.WriteFile(source, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := copyProject(context.Background(), root, stage); err != nil {
		t.Fatal(err)
	}
	if err := entity.Run([]string{"-dir", filepath.Join(stage, "game")}, io.Discard); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := planStagedProjectCommit(root, stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitSyncChanges(changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wire); !os.IsNotExist(err) {
		t.Fatalf("retired wire remains in project: %v", err)
	}
}
