package roost

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// RR-20261006-57：生成工程的 Makefile 用哪个版本的生成器，必须是工程所依赖的
// roost-core 版本。
//
// 旧行为：Makefile 写 `go run roost-core/codegen/cmd/roost@$(CODEGEN_VERSION)`，
// CODEGEN_VERSION 取 `versions.codegen`，校验只要求 ≥ v1.15.0。合仓后这个号就是
// roost-core 的版本，而 roost-core v1.15.x 里没有 codegen/cmd/roost：钉 v1.15.x 的
// 工程 `make sync / generate / check-generated / ci` 全部失败；钉 v1.16～v1.22 的
// 用比 core 旧的生成器改写工程；core 钉 v1.23.0 而 codegen 默认 latest 时，生成器
// 又比 core 新。一个工程里两个号描述同一个模块，只会出现不一致。

// firstCoreWithGenerator is the first roost-core release that contains
// codegen/cmd/roost (the consolidation, 三仓合一仓 P5).
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
