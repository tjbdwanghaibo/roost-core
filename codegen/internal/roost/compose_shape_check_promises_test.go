package roost

// RR-20260930-17：生成工程自带一步 compose 结构检查——deploy/docker/compose_check_test.go 读
// `docker compose config --format json` 解析后的结果，断言每个 Service 的 tmpfs 恰好一条绝对路径挂载、
// read_only / user / cap_drop / security_opt、stop_grace_period、healthcheck、config bind 与命名卷；
// 生成的 CI（generated-and-deployment 作业）与 Makefile 的 compose-check 都设置 ROOST_COMPOSE_CHECK 跑它。
// 旧行为：CI 只跑 `docker compose config --quiet`，只校验语法，RR-20260927-33 那种按逗号拆成四条挂载的
// tmpfs 照样绿。这里第二个用例真的用 docker 跑生成出来的检查：正确的 compose 绿，tmpfs 改回 RR-33 的
// 错误形状时红。

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const composeCheckRel = "deploy/docker/compose_check_test.go"

func TestGeneratedProjectCarriesAComposeShapeCheckAndRunsItInCI(t *testing.T) {
	t.Parallel()
	m := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := plan[composeCheckRel]
	if !ok {
		t.Fatalf("the plan has no %s", composeCheckRel)
	}
	if !file.Owned {
		t.Errorf("%s is not generator-owned; sync would not update it", composeCheckRel)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), composeCheckRel, file.Body, parser.AllErrors)
	if err != nil {
		t.Fatalf("%s does not parse: %v\n%s", composeCheckRel, err, file.Body)
	}
	if parsed.Name.Name != "docker" {
		t.Errorf("%s is package %s, want docker", composeCheckRel, parsed.Name.Name)
	}
	for _, want := range []string{composeCheckEnv, `"docker", "compose"`, `"config", "--format", "json"`, "ghcr.io/example/planet@sha256:", "RR-20260927-33"} {
		if !bytes.Contains(file.Body, []byte(want)) {
			t.Errorf("%s does not contain %q", composeCheckRel, want)
		}
	}
	ci := string(plan[".github/workflows/ci.yml"].Body)
	if !strings.Contains(ci, composeCheckEnv+`: "1"`) || !strings.Contains(ci, "go test -count=1 ./deploy/docker/") {
		t.Errorf("generated CI does not run the compose shape check with %s set:\n%s", composeCheckEnv, ci)
	}
	if quiet, check := strings.Index(ci, "config --quiet"), strings.Index(ci, "./deploy/docker/"); quiet < 0 || check < quiet {
		t.Errorf("the compose shape check must follow `docker compose config --quiet` in the CI job")
	}
	makefile := string(plan["Makefile"].Body)
	if !strings.Contains(makefile, "compose-check:\n\tROOST_IMAGE=") || !strings.Contains(makefile, "\n\t"+composeCheckEnv+"=1 go test -count=1 ./deploy/docker/\n") {
		t.Errorf("make compose-check does not run the compose shape check:\n%s", makefile)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The generated check, run for real: a module holding only the generated test
// and the generated compose, `go test` with the switch set. The good compose is
// green; the compose with RR-20260927-33's tmpfs shape is red and names tmpfs.
func TestGeneratedComposeShapeCheckCatchesTheTmpfsShapeComposeUpRefuses(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed; the generated compose shape check needs docker compose config")
	}
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose is not usable here: %v\n%s", err, out)
	}
	m := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	good := plan["deploy/docker/docker-compose.prod.yaml"].Body
	broken := bytes.ReplaceAll(good, []byte(`tmpfs: ["/tmp:rw,noexec,nosuid,size=64m"]`), []byte(`tmpfs: [/tmp:rw,noexec,nosuid,size=64m]`))
	if bytes.Equal(good, broken) {
		t.Fatalf("the rendered compose has no quoted tmpfs line to break:\n%s", good)
	}
	for _, tc := range []struct {
		name    string
		compose []byte
		pass    bool
	}{
		{"generated compose passes", good, true},
		{"RR-20260927-33 tmpfs fails", broken, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			write := func(rel string, body []byte) {
				path := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", []byte("module example.com/planet\n\ngo "+generatedGoVersion+".0\n"))
			write(composeCheckRel, plan[composeCheckRel].Body)
			write("deploy/docker/docker-compose.prod.yaml", tc.compose)
			cmd := exec.Command("go", "test", "-count=1", "./deploy/docker/")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod", composeCheckEnv+"=1")
			out, err := cmd.CombinedOutput()
			if tc.pass && err != nil {
				t.Fatalf("the generated compose check failed on the generated compose: %v\n%s", err, out)
			}
			if !tc.pass && err == nil {
				t.Fatalf("the generated compose check passed a compose whose tmpfs splits into four mounts:\n%s", out)
			}
			if !tc.pass && !bytes.Contains(out, []byte("tmpfs resolved to 4 mounts")) {
				t.Fatalf("the failure does not name the tmpfs shape:\n%s", out)
			}
		})
	}
}
