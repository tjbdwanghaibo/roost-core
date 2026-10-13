package roost

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The generated project's own CI runs `docker compose config` on the
// production compose file and `shellcheck` on every deploy script, so a
// generator defect in either shows up as a red pipeline in every consumer.
// These tests pin the two defects that did exactly that on 2026-09-05.

// Compose short-syntax volumes treat a source that does not start with `/`,
// `./` or `../` as a NAMED volume. `${ROOST_CONFIG_ROOT}/config.game.yaml`
// with ROOST_CONFIG_ROOT=configs/service therefore rendered as a reference to
// an undefined volume ("service "game" refers to undefined volume"). Long
// syntax with an explicit `type: bind` has no such ambiguity.
func TestProductionComposeMountsTheConfigAsAnExplicitBind(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	compose := renderProductionCompose(m)
	if !strings.Contains(compose, "type: bind") {
		t.Fatalf("config mount is not an explicit bind:\n%s", compose)
	}
	if short := regexp.MustCompile(`(?m)^\s*- \$\{ROOST_CONFIG_ROOT[^\n]*:/etc/roost/config\.yaml`); short.MatchString(compose) {
		t.Fatalf("config mount still uses short syntax, which compose may read as a named volume:\n%s", compose)
	}
	if !strings.Contains(compose, "type: volume") || !strings.Contains(compose, "source: planet-game-wal") {
		t.Fatalf("WAL volume lost its explicit long-syntax mount:\n%s", compose)
	}
}

// Every place the generator points compose at the repository's own configs
// must do so with an absolute path: compose resolves a relative bind source
// against the compose FILE's directory (deploy/docker/), not the caller's cwd.
func TestGeneratedComposeInvocationsUseAnAbsoluteConfigRoot(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"Makefile", ".github/workflows/ci.yml"} {
		body := string(plan[path].Body)
		if strings.Contains(body, "ROOST_CONFIG_ROOT=configs/service") || strings.Contains(body, "ROOST_CONFIG_ROOT: configs/service") {
			t.Errorf("%s passes a relative ROOST_CONFIG_ROOT to compose:\n%s", path, body)
		}
	}
}

// shellcheck findings in the generated scripts: SC1007 (`CDPATH= cd`) and
// SC2194 (a `case` on a constant word). Both are warnings that fail the
// generated project's "deployment shell syntax" step.
func TestDeployScriptsCarryNoKnownShellcheckFindings(t *testing.T) {
	for name, m := range map[string]Manifest{
		"stateful":  DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"stateless": DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files := renderProductionDeployment(m)
			var scripts []string
			for path, body := range files {
				if !strings.HasSuffix(path, ".sh") {
					continue
				}
				if strings.Contains(body, "CDPATH= cd") {
					t.Errorf("%s: SC1007 — `CDPATH= cd`", path)
				}
				if regexp.MustCompile(`(?m)^\s*case " [^$"]*" in`).MatchString(body) {
					t.Errorf("%s: SC2194 — case on a constant word", path)
				}
				if strings.Contains(body, `case "$SERVICE" in )`) {
					t.Errorf("%s: empty alternation is a syntax error", path)
				}
				full := filepath.Join(dir, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
				scripts = append(scripts, full)
				if out, err := exec.Command("sh", "-n", full).CombinedOutput(); err != nil {
					t.Errorf("%s: sh -n: %v\n%s", path, err, out)
				}
			}
			if len(scripts) == 0 {
				t.Fatal("no deploy scripts rendered")
			}
			if _, err := exec.LookPath("shellcheck"); err != nil {
				t.Logf("shellcheck not installed; the generated project's CI runs it")
				return
			}
			if out, err := exec.Command("shellcheck", scripts...).CombinedOutput(); err != nil {
				t.Errorf("shellcheck: %v\n%s", err, out)
			}
		})
	}
}

// The compatibility matrix's "minimum" dependency set must be exactly the
// floor the generator enforces, or the job fails at `roost project new`
// ("versions.core requires >= <floor> or latest; got v1.8.0") instead of
// testing anything. Two literals in two files, one fact: pin them together.
func TestFrameworkCompatMinimumSetMatchesTheGeneratorFloor(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "framework-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`-roost-core-version (\S+) -roost-kit-version (\S+)\)`)
	match := re.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("framework-compat.yml has no minimum dependency set")
	}
	got := VersionSpec{Core: match[1], Kit: match[2]}
	if got != minimumVersions {
		t.Fatalf("workflow minimum set %+v != generator floor %+v", got, minimumVersions)
	}
	// The minimum lane also runs a make target through the pinned generator
	// and checks every invocation names it (RR-20261006-57).
	if want := "grep -v 'cmd/roost@" + minimumVersions.Core + " '"; !strings.Contains(string(raw), want) {
		t.Fatalf("framework-compat.yml minimum lane does not check the Makefile generator against %s (want %q)", minimumVersions.Core, want)
	}
}

// Historical generators below v1.11.0 emit go.mod requirements on the
// pre-rename module paths (github.com/tjbdwanghaibo/cube-core, cube-kit);
// `go get` of those fails forever, so a project created by them can never be
// upgraded in CI. The upgrade matrix must start at the first release that
// emitted roost-* paths.
func TestUpgradeCompatHistoryStartsAtTheRoostModulePaths(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "upgrade-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`codegen: \[([^\]]+)\]`)
	match := re.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("upgrade-compat.yml has no codegen matrix")
	}
	for _, entry := range strings.Split(match[1], ",") {
		version := strings.TrimSpace(entry)
		if !versionAtLeast(version, 1, 11, 0) {
			t.Errorf("matrix entry %s predates the roost-* module paths and cannot resolve", version)
		}
	}
}

// `! cmd` as a statement skips errexit (SC2251): the negated grep in
// release.yml reports nothing and the step continues, so the hygiene it
// implements never fails. Bare `*.tar.gz` globs are SC2035.
func TestReleaseWorkflowShellHasNoBareNegationsOrGlobs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "! ") {
			t.Errorf("release.yml:%d: bare negation skips errexit: %s", i+1, trimmed)
		}
		if strings.Contains(trimmed, "sha256sum *.") {
			t.Errorf("release.yml:%d: bare glob: %s", i+1, trimmed)
		}
	}
}

// kustomize v5.7+ (kubectl 1.34+) rejects an overlay whose base directory is
// one of the overlay's own ancestors: "cycle detected: candidate root
// deploy/k8s contains visited root deploy/k8s/overlays/staging". The base
// therefore has its own directory, and every overlay must point at it.
func TestKubernetesBaseIsNotAnAncestorOfItsOverlays(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	files := renderProductionDeployment(m)
	if _, ok := files["deploy/k8s/base/kustomization.yaml"]; !ok {
		t.Fatal("base kustomization is not under deploy/k8s/base/")
	}
	if _, ok := files["deploy/k8s/kustomization.yaml"]; ok {
		t.Fatal("a kustomization at deploy/k8s/ makes the base an ancestor of its overlays")
	}
	dir := t.TempDir()
	for _, env := range []string{"staging", "production"} {
		overlay := files["deploy/k8s/overlays/"+env+"/kustomization.yaml"]
		if !strings.Contains(overlay, "- ../../base") || strings.Contains(overlay, "- ../..\n") {
			t.Errorf("%s overlay does not reference ../../base:\n%s", env, overlay)
		}
		if strings.Contains(overlay, "commonLabels") {
			t.Errorf("%s overlay uses deprecated commonLabels", env)
		}
	}
	for path, body := range files {
		if !strings.HasPrefix(path, "deploy/k8s/") {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Logf("kubectl not installed; the generated project's CI renders the overlays")
		return
	}
	for _, env := range []string{"staging", "production"} {
		out, err := exec.Command("kubectl", "kustomize", filepath.Join(dir, "deploy", "k8s", "overlays", env)).CombinedOutput()
		if err != nil {
			t.Errorf("kubectl kustomize %s: %v\n%s", env, err, out)
		}
		if strings.Contains(string(out), "deprecated") {
			t.Errorf("kubectl kustomize %s warns about a deprecated field:\n%s", env, out)
		}
	}
}

// The generated go.mod, the Dockerfile's builder image and the generator's
// own toolchain requirement are one fact. When the Dockerfile lagged at 1.25
// while the framework needed 1.27, every generated project's image build
// failed at `go mod download` with "go.mod requires go >= 1.27.0".
func TestGeneratedGoVersionMatchesTheGeneratorsOwn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("generator go.mod has no go directive")
	}
	if match[1] != generatedGoVersion {
		t.Fatalf("generatedGoVersion = %s but the generator itself requires go %s", generatedGoVersion, match[1])
	}
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	if body := string(plan["Dockerfile"].Body); !strings.Contains(body, "ARG GO_VERSION="+generatedGoVersion+"\n") {
		t.Errorf("Dockerfile builder image is not golang:%s:\n%s", generatedGoVersion, body)
	}
	if body := string(plan["go.mod"].Body); !strings.Contains(body, "\ngo "+generatedGoVersion+".0\n") {
		t.Errorf("go.mod directive is not go %s.0:\n%s", generatedGoVersion, body)
	}
}

// The generated project's own workflows go through actionlint + shellcheck in
// its CI, and this repository's consumer-acceptance job lints them too. The
// two findings that failed the v1.13.1 release run — a bare `! cmd` statement
// (SC2251, skips errexit) and a bare glob (SC2035) — must not come back in
// any rendered workflow.
func TestGeneratedWorkflowsHaveNoBareNegationsOrGlobs(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for path, file := range plan {
		if !strings.HasPrefix(path, ".github/workflows/") {
			continue
		}
		seen++
		for i, line := range strings.Split(string(file.Body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "! ") {
				t.Errorf("%s:%d: bare negation skips errexit: %s", path, i+1, trimmed)
			}
			if regexp.MustCompile(`(^|\s)\*\.[a-z]`).MatchString(trimmed) && !strings.HasPrefix(trimmed, "#") {
				t.Errorf("%s:%d: bare glob: %s", path, i+1, trimmed)
			}
		}
	}
	if seen < 5 {
		t.Fatalf("only %d workflows rendered", seen)
	}
}

// The dev compose initiates the Mongo replica set with a member address, and
// the driver DISCOVERS that address and dials it. A generated binary runs on
// the developer's machine and is configured with 127.0.0.1:27017, so the
// member must be 127.0.0.1:27017 too: initiating with "mongo:27017" (the
// compose service name) left every host-run game process at
// "ReplicaSetNoPrimary … lookup mongo: server misbehaving" — found by the
// boot gate the first time it ran.
func TestDevComposeReplicaSetMemberIsTheAddressTheConfigDials(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil)
	compose := renderCompose(m)
	if !strings.Contains(compose, "host:'127.0.0.1:27017'") {
		t.Fatalf("replica set member is not the host-reachable address:\n%s", compose)
	}
	config := renderServiceConfig(m, "game", false)
	if !strings.Contains(config, "mongodb://127.0.0.1:27017/?replicaSet=rs0") {
		t.Fatalf("service config does not dial 127.0.0.1:27017 with replicaSet=rs0:\n%s", config)
	}
}

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

// 生成的 compose 形状检查真实运行：一个只含生成测试与生成 compose 的模块，打开开关跑 `go test`。正常的 compose 通过；
// 带 compose up 会拒绝的 tmpfs 形状的 compose 失败并点名 tmpfs。
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

func TestProductionComposeTmpfsIsOneAbsoluteMountPerService(t *testing.T) {
	for name, m := range map[string]Manifest{
		"stateful":  DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil),
		"stateless": DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			body := renderProductionCompose(m)
			var compose struct {
				Services map[string]struct {
					Tmpfs       []string `yaml:"tmpfs"`
					SecurityOpt []string `yaml:"security_opt"`
					CapDrop     []string `yaml:"cap_drop"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal([]byte(body), &compose); err != nil {
				t.Fatalf("decode production compose: %v\n%s", err, body)
			}
			if len(compose.Services) != len(m.Services) {
				t.Fatalf("compose has %d services, manifest has %d:\n%s", len(compose.Services), len(m.Services), body)
			}
			for service, spec := range compose.Services {
				if len(spec.Tmpfs) != 1 {
					t.Errorf("service %s: tmpfs parsed as %d items %q, want exactly one mount", service, len(spec.Tmpfs), spec.Tmpfs)
					continue
				}
				if mount := spec.Tmpfs[0]; !strings.HasPrefix(mount, "/") || mount != "/tmp:rw,noexec,nosuid,size=64m" {
					t.Errorf("service %s: tmpfs = %q, want the single absolute mount /tmp:rw,noexec,nosuid,size=64m", service, mount)
				}
				// 同一行的另外两个 flow 序列今天各只有一个值；核对它们也按一项解析，
				// 以后有人往里加带逗号的值时这里会先红。
				if len(spec.SecurityOpt) != 1 || spec.SecurityOpt[0] != "no-new-privileges:true" {
					t.Errorf("service %s: security_opt = %q, want [no-new-privileges:true]", service, spec.SecurityOpt)
				}
				if len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
					t.Errorf("service %s: cap_drop = %q, want [ALL]", service, spec.CapDrop)
				}
			}
		})
	}
}
