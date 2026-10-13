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
	if err := updateFrameworkDependenciesTransactional(context.Background(), root, manifest, io.Discard, io.Discard, runner); err != nil {
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

// Every direct framework module in one `go get` — which after the
// consolidation is exactly one module.
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
	if err := updateFrameworkDependencies(context.Background(), root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	// One module since the consolidation: kit is a package path inside
	// roost-core, so there is nothing else to resolve alongside it.
	want := [][]string{
		{"get", "github.com/tjbdwanghaibo/roost-core@latest"},
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
	var get []string
	runner := func(_ context.Context, _ string, _, _ io.Writer, args ...string) error {
		if len(args) > 0 && args[0] == "get" {
			get = append([]string(nil), args...)
		}
		return nil
	}
	if err := updateFrameworkDependencies(context.Background(), root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(get, " ")
	if want := "roost-core@" + minimumVersions.Core; !strings.Contains(joined, want) {
		t.Fatalf("go get command missing %q: %v", want, get)
	}
	// And it must NOT ask for a module that does not exist: after the
	// consolidation kit is a package path inside roost-core.
	if strings.Contains(joined, "roost-core/kit@") || strings.Contains(joined, "roost-kit@") {
		t.Fatalf("go get asked for kit as a module of its own: %v", get)
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
	err := updateFrameworkDependencies(context.Background(), root, DefaultManifest("planet", "example.com/planet", nil, nil, nil), io.Discard, io.Discard, runner)
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
	if err := tidyProjectDependencies(context.Background(), root, io.Discard, io.Discard, runner); err != nil {
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
	err := tidyProjectDependencies(context.Background(), root, io.Discard, io.Discard, runner)
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
	m.Versions.Core = "v1.6.9"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), minimumVersions.Core) {
		t.Fatalf("unsupported core version accepted: %v", err)
	}
	m.Versions.Core = "main"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "latest or a semantic release") {
		t.Fatalf("non-release policy accepted: %v", err)
	}
	m.Versions.Core = "v2.0.0"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "module path") {
		t.Fatalf("incompatible major accepted: %v", err)
	}
	// A pre-release of the floor carries the floor's layout and is accepted.
	m.Versions.Core = minimumVersions.Core + "-alpha.1"
	if err := m.Validate(); err != nil {
		t.Fatalf("pre-release pin of the floor rejected: %v", err)
	}
	m.Versions.Core = "v1.0.0-alpha.1"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), minimumVersions.Core) {
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

// dependencyProject 是依赖解析事务测试用的最小工程：一份合法 manifest、go.mod / go.sum 与两份业务源码。
func dependencyProject(t *testing.T) (string, Manifest, map[string]string) {
	t.Helper()
	root := t.TempDir()
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	goMod := "module example.com/planet\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.25.0\n"
	business := "package planet\nimport \"github.com/tjbdwanghaibo/roost-core/wiring/mods\"\nvar _ = mods.ModBus\n"
	files := map[string]string{ManifestName: string(raw), "go.mod": goMod, "go.sum": "old checksum\n", "business.go": business, "untouched.go": "package planet\n"}
	for rel, body := range files {
		writeProjectFile(t, root, rel, body)
	}
	return root, manifest, files
}

// get 或 tidy 失败时原工程的输入一律不变：暂存目录里写到一半的 go.sum 不回写。
func TestFrameworkDependencyUpdateFailurePreservesInputs(t *testing.T) {
	for _, failure := range []string{"get", "tidy"} {
		t.Run(failure, func(t *testing.T) {
			root, manifest, files := dependencyProject(t)
			runner := func(_ context.Context, stage string, _, _ io.Writer, args ...string) error {
				if err := os.WriteFile(filepath.Join(stage, "go.sum"), []byte("partial checksum\n"), 0o644); err != nil {
					return err
				}
				if args[0] == "get" && failure == "get" || args[0] == "mod" && failure == "tidy" {
					return errors.New("resolver failure")
				}
				return nil
			}
			if err := updateFrameworkDependenciesTransactional(context.Background(), root, manifest, io.Discard, io.Discard, runner); err == nil {
				t.Fatal("expected failure")
			}
			for rel, before := range files {
				assertFileContent(t, filepath.Join(root, rel), []byte(before))
			}
		})
	}
}

// 解析期间原工程的输入（业务源码、manifest）被并发修改时拒绝提交，原文件保持并发写入后的内容。
func TestFrameworkDependencyUpdateRejectsConcurrentInputChanges(t *testing.T) {
	for _, rel := range []string{"business.go", ManifestName} {
		t.Run(rel, func(t *testing.T) {
			root, manifest, files := dependencyProject(t)
			concurrent := files[rel] + "\n// concurrent edit\n"
			runner := func(_ context.Context, stage string, _, _ io.Writer, args ...string) error {
				if args[0] == "get" {
					if err := os.WriteFile(filepath.Join(root, rel), []byte(concurrent), 0o644); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(stage, "go.mod"), []byte("module changed\n"), 0o644)
				}
				return nil
			}
			err := updateFrameworkDependenciesTransactional(context.Background(), root, manifest, io.Discard, io.Discard, runner)
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("concurrent input not rejected: %v", err)
			}
			files[rel] = concurrent
			for path, before := range files {
				assertFileContent(t, filepath.Join(root, path), []byte(before))
			}
		})
	}
}
