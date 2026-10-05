package roost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
)

func TestStagedProjectCommitAndCheckSeeTableJSONRetirement(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"config"}})
	if err != nil {
		t.Fatal(err)
	}
	schema := filepath.Join(root, "configs", "schema", "monster.go")
	source := "package schema\n//roost:table name=monster file=monster.csv json=monster.json key=ID\ntype Monster struct { ID int32 `csv:\"id\" json:\"id\"` }\n"
	if err := os.WriteFile(schema, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	csvDir := filepath.Join(root, "configs", "table")
	if err := os.MkdirAll(csvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	csv := filepath.Join(csvDir, "monster.csv")
	if err := os.WriteFile(csv, []byte("id\n1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	jsonPath := filepath.Join(root, "configs", "data", "monster.json")
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schema, []byte("package schema\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(csv); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkGenerated(context.Background(), root, manifest, io.Discard); err == nil || !strings.Contains(err.Error(), "configs/data/monster.json") {
		t.Fatalf("check missed retired JSON: %v", err)
	}
	stage := filepath.Join(t.TempDir(), "stage")
	if err := copyProject(context.Background(), root, stage); err != nil {
		t.Fatal(err)
	}
	if err := tablegen.Run([]string{"-meta", filepath.Join(stage, "configs", "schema"), "-csv", filepath.Join(stage, "configs", "table"), "-json", filepath.Join(stage, "configs", "data"), "-force"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	changes, err := planStagedProjectCommit(root, stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitSyncChanges(changes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Fatalf("retired JSON remains in real project: %v", err)
	}
}
