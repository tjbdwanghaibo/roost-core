package roost

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGeneratorsDoNotMoveTheProcessIntoTheTreeTheyGenerate(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/planet\n\ngo "+generatedGoVersion+".0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var childGoMod, generatorWd, generatorRoot string
	probe := generator{Name: "probe", Always: true, Run: func(at string, _ io.Writer) error {
		generatorRoot = at
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		generatorWd = wd
		// A child started without Dir, as another goroutine of a library
		// caller (or a parallel test) would start one during this window.
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			return err
		}
		childGoMod = strings.TrimSpace(string(out))
		return nil
	}}
	if err := runGenerators(context.Background(), root, DefaultManifest("planet", "example.com/planet", nil, nil, nil), []generator{probe}, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if !sameDir(t, generatorRoot, root) {
		t.Errorf("the generator was handed %q, want the project root %q", generatorRoot, root)
	}
	if sameDir(t, generatorWd, root) {
		t.Errorf("the process working directory was the generated tree while generators ran: %s", generatorWd)
	}
	if childGoMod != "" && within(t, childGoMod, root) {
		t.Errorf("a child started without Dir while generators ran inherited the generated tree as its working directory (go env GOMOD = %s)", childGoMod)
	}
}

// The end-to-end shape of the same promise: while SyncProject runs, children
// that another goroutine starts without Dir never run inside the .roost-sync-*
// staging tree. Before the fix a few dozen of ~5000 such children over five
// syncs landed in a stage on macOS (probe in the RR record).
func TestSyncProjectStagesNeverBecomeAChildsWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root}); err != nil {
		t.Fatal(err)
	}
	stagesParent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var mu sync.Mutex
	var inStage []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			out, err := exec.Command("go", "env", "GOMOD").Output()
			if err != nil {
				continue
			}
			goMod, evalErr := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
			if evalErr != nil {
				continue
			}
			if strings.HasPrefix(goMod, filepath.Join(stagesParent, ".roost-")) {
				mu.Lock()
				inStage = append(inStage, goMod)
				mu.Unlock()
			}
		}
	}()
	for i := 0; i < 3; i++ {
		if _, err := SyncProject(root); err != nil {
			stop.Store(true)
			<-done
			t.Fatal(err)
		}
	}
	stop.Store(true)
	<-done
	if len(inStage) > 0 {
		t.Errorf("%d children started without Dir ran inside a staging tree, e.g. %s", len(inStage), inStage[0])
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	if a == "" || b == "" {
		return false
	}
	ea, errA := filepath.EvalSymlinks(a)
	eb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ea == eb
}

func within(t *testing.T, path, dir string) bool {
	t.Helper()
	ep, errP := filepath.EvalSymlinks(path)
	ed, errD := filepath.EvalSymlinks(dir)
	if errP != nil || errD != nil {
		return false
	}
	rel, err := filepath.Rel(ed, ep)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

const firstCoreWithGenerator = "v1.16.0"

func TestVersionsCodegenIsRefused(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	m.Versions.Core = minimumVersions.Core
	m.Versions.Codegen = "v1.15.0"
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "versions.codegen") || !strings.Contains(err.Error(), "versions.core") {
		t.Fatalf("a manifest pinning versions.codegen separately from versions.core must be refused, pointing at versions.core; got %v", err)
	}
}

func TestTheMakefileRunsTheGeneratorAtTheCoreVersion(t *testing.T) {
	major, minor, patch, ok := releaseVersion(firstCoreWithGenerator)
	if !ok || !versionAtLeast(minimumVersions.Core, major, minor, patch) {
		t.Fatalf("the lowest accepted core %s predates %s, the first release that contains the generator", minimumVersions.Core, firstCoreWithGenerator)
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for _, core := range []string{minimumVersions.Core, "latest"} {
		m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
		m.Versions.Core = core
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(renderMakefile(m)), 0o644); err != nil {
			t.Fatal(err)
		}
		// make -n prints what the target would run without running it: the
		// exact generator the project's own `make check-generated` / CI uses.
		cmd := exec.Command("make", "-n", "check-generated", "generate", "sync")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("make -n: %v\n%s", err, out)
		}
		want := "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@" + core + " "
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if !strings.Contains(line, want) {
				t.Errorf("versions.core=%s: make runs %q, want the generator at %s", core, line, core)
			}
		}
	}
}
