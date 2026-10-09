package roost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const dependencyLegacySource = "package planet\nimport \"github.com/tjbdwanghaibo/roost-kit/mods\"\nvar _ = mods.ModBus\n"

func legacyDependencyProject(t *testing.T) (string, Manifest, map[string]string) {
	t.Helper()
	root := t.TempDir()
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{ManifestName: string(raw), "go.mod": legacyGoMod, "go.sum": "old checksum\n", "business.go": dependencyLegacySource, "untouched.go": "package planet\n"}
	for rel, body := range files {
		writeProjectFile(t, root, rel, body)
	}
	return root, manifest, files
}

// RR-20260930-CG-14：成功提交依赖时，框架自身的 import/manifest
// 迁移也必须回写；依赖命令对业务文件的任意改动仍须隔离。
func TestFrameworkDependencyConsolidationCommitsOnlyPlannedMigration(t *testing.T) {
	root, manifest, _ := legacyDependencyProject(t)
	var migratedSource, migratedManifest []byte
	runner := func(_ context.Context, stage string, _, _ io.Writer, args ...string) error {
		if args[0] != "get" {
			return nil
		}
		var err error
		migratedSource, err = os.ReadFile(filepath.Join(stage, "business.go"))
		if err != nil {
			return err
		}
		migratedManifest, err = os.ReadFile(filepath.Join(stage, ManifestName))
		if err != nil {
			return err
		}
		if !bytes.Contains(migratedSource, []byte("roost-core/wiring/mods")) {
			t.Fatal("resolver saw unmigrated imports")
		}
		if err := os.WriteFile(filepath.Join(stage, "go.mod"), []byte("module example.com/planet\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.18.0\n"), 0o644); err != nil {
			return err
		}
		for rel, body := range map[string]string{"business.go": "package overwritten\n", ManifestName: "resolver overwrite\n", "untouched.go": "package overwritten\n"} {
			if err := os.WriteFile(filepath.Join(stage, rel), []byte(body), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	if err := updateFrameworkDependenciesTransactional(context.Background(), root, manifest, io.Discard, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, filepath.Join(root, "business.go"), migratedSource)
	assertFileContent(t, filepath.Join(root, ManifestName), migratedManifest)
	assertFileContent(t, filepath.Join(root, "untouched.go"), []byte("package planet\n"))
	m, err := loadManifestForUpgrade(root)
	if err != nil || m.Versions.legacyModulePolicies() {
		t.Fatalf("legacy policies retained: %+v, %v", m.Versions, err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !bytes.Contains(mod, []byte("roost-core v1.18.0")) || bytes.Contains(mod, []byte("roost-kit")) {
		t.Fatalf("incorrect dependencies: %v\n%s", err, mod)
	}
}

func TestFrameworkDependencyConsolidationFailurePreservesInputs(t *testing.T) {
	for _, failure := range []string{"get", "tidy", "consolidate"} {
		t.Run(failure, func(t *testing.T) {
			root, manifest, files := legacyDependencyProject(t)
			if failure == "consolidate" {
				files["unmapped.go"] = "package planet\nimport _ \"github.com/tjbdwanghaibo/roost-skill/not_in_map\"\n"
				writeProjectFile(t, root, "unmapped.go", files["unmapped.go"])
			}
			runner := func(_ context.Context, stage string, _, _ io.Writer, args ...string) error {
				if failure == "consolidate" {
					t.Fatal("resolver called after invalid migration")
				}
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

func TestFrameworkDependencyConsolidationRejectsConcurrentInputChanges(t *testing.T) {
	for _, rel := range []string{"business.go", ManifestName} {
		t.Run(rel, func(t *testing.T) {
			root, manifest, files := legacyDependencyProject(t)
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
