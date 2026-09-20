package roost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFrameworkDependencyUpdateStagesAndCommitsOnlyModuleFiles(t *testing.T) {
	root := t.TempDir()
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	businessPath := filepath.Join(root, "business.go")
	if err := os.WriteFile(businessPath, []byte("package planet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, stage string, _, _ io.Writer, args ...string) error {
		if args[0] == "get" {
			if err := os.WriteFile(filepath.Join(stage, "go.mod"), []byte("module example.com/planet\n\nrequire example.com/dependency v1.0.0\n"), 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(stage, "business.go"), []byte("package overwritten\n"), 0o644)
		}
		return os.WriteFile(filepath.Join(stage, "go.sum"), []byte("checksum\n"), 0o644)
	}
	if err := updateFrameworkDependenciesTransactional(root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(goMod, []byte("example.com/dependency")) {
		t.Fatalf("go.mod was not committed:\n%s", goMod)
	}
	business, err := os.ReadFile(businessPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(business) != "package planet\n" {
		t.Fatalf("staged business change leaked into project:\n%s", business)
	}
}

func TestUpdateFrameworkDependenciesResolvesAllDirectModulesTogether(t *testing.T) {
	root := t.TempDir()
	goMod := []byte("module example.com/planet\n\ngo 1.25\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	var commands [][]string
	runner := func(_ context.Context, gotRoot string, _, _ io.Writer, args ...string) error {
		if gotRoot != root {
			t.Fatalf("command root = %q, want %q", gotRoot, root)
		}
		commands = append(commands, append([]string(nil), args...))
		return nil
	}
	if err := updateFrameworkDependencies(root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{
			"get",
			"github.com/tjbdwanghaibo/roost-core@latest",
			"github.com/tjbdwanghaibo/roost-kit@latest",
		},
		{"mod", "tidy"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestUpdateFrameworkDependenciesUsesExplicitPolicies(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	// Pinned versions at the generator floor: the policy refuses anything
	// below it, because the code this generator emits does not compile there.
	manifest.Versions.Core = minimumVersions.Core
	manifest.Versions.Kit = minimumVersions.Kit
	var get []string
	runner := func(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
		if len(args) > 0 && args[0] == "get" {
			get = append([]string(nil), args...)
		}
		return nil
	}
	if err := updateFrameworkDependencies(root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(get, " ")
	for _, want := range []string{"roost-core@" + minimumVersions.Core, "roost-kit@" + minimumVersions.Kit} {
		if !strings.Contains(joined, want) {
			t.Fatalf("go get command missing %q: %v", want, get)
		}
	}
}

func TestUpdateFrameworkDependenciesRollsBackModuleFiles(t *testing.T) {
	root := t.TempDir()
	modPath := filepath.Join(root, "go.mod")
	sumPath := filepath.Join(root, "go.sum")
	oldMod := []byte("module example.com/planet\n\ngo 1.25\n")
	oldSum := []byte("old checksum\n")
	if err := os.WriteFile(modPath, oldMod, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sumPath, oldSum, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ string, _, _ io.Writer, _ ...string) error {
		if err := os.WriteFile(modPath, []byte("corrupt\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(sumPath, []byte("partial\n"), 0o644); err != nil {
			return err
		}
		return errors.New("registry unavailable")
	}
	err := updateFrameworkDependencies(root, DefaultManifest("planet", "example.com/planet", nil, nil, nil), io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("expected resolver error, got %v", err)
	}
	assertFileContent(t, modPath, oldMod)
	assertFileContent(t, sumPath, oldSum)
}

func TestTidyProjectDependenciesDoesNotUpgradeFramework(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var commands [][]string
	runner := func(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
		commands = append(commands, append([]string(nil), args...))
		return nil
	}
	if err := tidyProjectDependencies(root, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"mod", "tidy"}}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v, want %#v", commands, want)
	}
}

func TestTidyProjectDependenciesRollsBackModuleFiles(t *testing.T) {
	root := t.TempDir()
	modPath := filepath.Join(root, "go.mod")
	sumPath := filepath.Join(root, "go.sum")
	oldMod, oldSum := []byte("module example.com/planet\n"), []byte("old checksum\n")
	if err := os.WriteFile(modPath, oldMod, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sumPath, oldSum, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, _ string, _, _ io.Writer, _ ...string) error {
		if err := os.WriteFile(modPath, []byte("partial\n"), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(sumPath, []byte("partial\n"), 0o644); err != nil {
			return err
		}
		return errors.New("proxy unavailable")
	}
	err := tidyProjectDependencies(root, io.Discard, io.Discard, runner)
	if err == nil || !strings.Contains(err.Error(), "proxy unavailable") {
		t.Fatalf("expected tidy error, got %v", err)
	}
	assertFileContent(t, modPath, oldMod)
	assertFileContent(t, sumPath, oldSum)
}

func TestManifestVersionPolicies(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	if err := m.Validate(); err != nil {
		t.Fatalf("latest policy rejected: %v", err)
	}
	m.Versions.Kit = "v1.6.9"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), minimumVersions.Kit) {
		t.Fatalf("unsupported kit version accepted: %v", err)
	}
	m.Versions.Kit = "main"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "latest or a semantic release") {
		t.Fatalf("non-release policy accepted: %v", err)
	}
	m.Versions.Kit = "v2.0.0"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "module path") {
		t.Fatalf("incompatible major accepted: %v", err)
	}
	// A pre-release of the floor carries the floor's layout and is accepted.
	m.Versions.Kit = minimumVersions.Kit + "-alpha.1"
	if err := m.Validate(); err != nil {
		t.Fatalf("pre-release pin of the floor rejected: %v", err)
	}
	m.Versions.Kit = "v1.0.0-alpha.1"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), minimumVersions.Kit) {
		t.Fatalf("pre-release pin below the floor accepted: %v", err)
	}
}

func TestDependencyCommandForcesWorkspaceIsolation(t *testing.T) {
	got := appendWithoutGoWork([]string{"PATH=test", "GOWORK=D:/parent/go.work", "gowork=other"}, "GOWORK=off")
	if !reflect.DeepEqual(got, []string{"PATH=test", "GOWORK=off"}) {
		t.Fatalf("isolated environment = %#v", got)
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
