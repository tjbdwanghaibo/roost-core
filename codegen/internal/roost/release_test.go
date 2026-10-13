package roost

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFrameworkReleaseManifestStrictValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "framework-release.yaml")
	valid := []byte("schema: 3\nrelease: v1.16.0\nconsumer_go: [1.25.x, 1.26.x]\n")
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadFrameworkReleaseManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Release != "v1.16.0" || len(manifest.ConsumerGo) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	for name, raw := range map[string][]byte{
		"unknown":  bytes.Replace(valid, []byte("schema: 3"), []byte("schema: 3\nunknown: true"), 1),
		"latest":   bytes.Replace(valid, []byte("release: v1.16.0"), []byte("release: latest"), 1),
		"go":       bytes.Replace(valid, []byte("1.26.x"), []byte("1.26.0"), 1),
		"old-go":   bytes.Replace(valid, []byte("1.26.x"), []byte("1.24.x"), 1),
		"empty-go": bytes.Replace(valid, []byte("[1.25.x, 1.26.x]"), []byte("[]"), 1),
		// Schema 2 named three versions (codegen + framework.core/kit). One
		// module has one version, and the old shape is refused outright rather
		// than half-read — the codegen field drifting from the tag is what took
		// ten releases' worth of this gate down (U-0270).
		"schema-2":       bytes.Replace(valid, []byte("schema: 3"), []byte("schema: 2"), 1),
		"legacy-modules": bytes.Replace(valid, []byte("release: v1.16.0"), []byte("release: v1.16.0\nframework: {core: v1.15.18}"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFrameworkReleaseManifest(path); err == nil {
				t.Fatal("invalid release manifest was accepted")
			}
		})
	}
	future := bytes.Replace(valid, []byte("1.26.x"), []byte("1.27.x"), 1)
	if err := os.WriteFile(path, future, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrameworkReleaseManifest(path); err != nil {
		t.Fatalf("future Go release line should be configurable without a code change: %v", err)
	}
}

func TestPublishedFrameworkGoModRejectsLocalOrPseudoDependencies(t *testing.T) {
	for _, test := range []struct {
		name, goMod, want string
	}{
		{"replace", "module example.com/test\nreplace github.com/tjbdwanghaibo/roost-core => ../core\n", "replace directive"},
		{"replace-block", "module example.com/test\nreplace (\n example.com/a => ../a\n)\n", "replace directive"},
		{"pseudo", "module example.com/test\nrequire github.com/tjbdwanghaibo/roost-core v1.8.1-0.20260826111010-16f057d5e22f\n", "pseudo-version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePublishedFrameworkGoMod("example.com/test", "v1.0.0", []byte(test.goMod))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
	if err := validatePublishedFrameworkGoMod("example.com/test", "v1.0.0", []byte("module example.com/test\nrequire github.com/tjbdwanghaibo/roost-core v1.9.1\n")); err != nil {
		t.Fatal(err)
	}
}

func TestFrameworkGitHubOutputAndBuildVersion(t *testing.T) {
	manifest := FrameworkReleaseManifest{Schema: 3, Release: "v1.10.0", ConsumerGo: []string{"1.25.x", "1.26.x"}}
	path := filepath.Join(t.TempDir(), "github", "output")
	if err := appendFrameworkGitHubOutput(path, manifest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"release=v1.10.0", "consumer_go=1.25.x,1.26.x", `consumer_go_json=["1.25.x","1.26.x"]`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("GitHub output missing %q: %s", want, raw)
		}
	}
	previous := buildVersion
	buildVersion = "v1.10.0-test"
	t.Cleanup(func() { buildVersion = previous })
	var output bytes.Buffer
	if err := runVersion(nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "v1.10.0-test") {
		t.Fatalf("release build version missing: %s", output.String())
	}
}

// codegen/scripts/*-runtime.sh 编译生成物所用的 roost-core 版本必须能从仓里读出来，且就是生成器承诺的下限
// minimumVersions.Core：它们统一调 scripts/core-pin.sh，这条测试把该脚本的输出钉在 minimumVersions 上，
// manifest.go 的写法变了、脚本读不出来时在这里红，而不是在 CI 的守卫里 exit 2；ROOST_CORE_PIN 覆盖仍然生效。
func TestCorePinScriptPrintsTheGeneratorMinimum(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the runtime guards are POSIX sh scripts run on Linux CI")
	}
	script := filepath.Join("..", "..", "scripts", "core-pin.sh")
	cmd := exec.Command("sh", script)
	// The override would mask exactly what this test reads.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ROOST_CORE_PIN=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s: %v\n%s", script, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != minimumVersions.Core {
		t.Fatalf("core-pin.sh printed %q, want minimumVersions.Core %q: the runtime guards would compile the generator's output against a framework it does not promise", got, minimumVersions.Core)
	}

	// The override still wins, for running a guard against another release.
	cmd = exec.Command("sh", script)
	cmd.Env = append(os.Environ(), "ROOST_CORE_PIN=v9.9.9")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sh %s with ROOST_CORE_PIN: %v\n%s", script, err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "v9.9.9" {
		t.Fatalf("core-pin.sh ignored ROOST_CORE_PIN: printed %q", got)
	}
}

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
