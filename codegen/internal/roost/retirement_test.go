package roost

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entity"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/tablegen"
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

func TestSyncCommitsProtocolRetirement(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{"protocol/proto/protocol.proto", "protocol/pb/protocol.pb.go", "protocol/msgid/msgid_gen.go", "protocol/protocol_manifest.json"}
	for _, rel := range wantRemoved {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("initial generated output %s: %v", rel, err)
		}
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(paths[0]))); err != nil {
		t.Fatal(err)
	}
	result, err := SyncProject(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range wantRemoved {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("retired %s remains: %v", rel, err)
		}
		if !contains(result.Removed, rel) {
			t.Fatalf("%s missing from removed result: %v", rel, result.Removed)
		}
	}
}

func TestGenerateCheckDetectsMarkerlessProtocolDrift(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"protocol/proto/protocol.proto", "protocol/protocol_manifest.json"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(append([]byte(nil), raw...), []byte("\n ")...), 0o644); err != nil {
			t.Fatal(err)
		}
		err = Generate(root, GenerateOptions{Check: true, Stdout: io.Discard})
		if err == nil || !strings.Contains(err.Error(), rel) {
			t.Fatalf("-check accepted changed %s: %v", rel, err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
