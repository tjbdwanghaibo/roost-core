package roost

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
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
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "framework-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`-roost-core-version (\S+) -roost-kit-version (\S+) -codegen-version (\S+)\)`)
	match := re.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("framework-compat.yml has no minimum dependency set")
	}
	got := VersionSpec{Core: match[1], Kit: match[2], Codegen: match[3]}
	if got != minimumVersions {
		t.Fatalf("workflow minimum set %+v != generator floor %+v", got, minimumVersions)
	}
}

// Historical generators below v1.11.0 emit go.mod requirements on the
// pre-rename module paths (github.com/tjbdwanghaibo/cube-core, cube-kit);
// `go get` of those fails forever, so a project created by them can never be
// upgraded in CI. The upgrade matrix must start at the first release that
// emitted roost-* paths.
func TestUpgradeCompatHistoryStartsAtTheRoostModulePaths(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "upgrade-compat.yml"))
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
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
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

// A project generated before the base moved keeps its old manifests straight
// under deploy/k8s/. They carry no generated header, so the generic
// "obsolete output" rule cannot see them; sync recognises them by the fixed
// names and the roost namespace instead — and leaves a hand-written file and
// the never-owned secret example alone.
func TestSyncRemovesTheLegacyKubernetesBase(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"game"}, Mods: []string{"configdata"}, Features: []string{"config"}}); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{
		"deploy/k8s/kustomization.yaml":   "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: roost\nresources:\n  - game.yaml\n",
		"deploy/k8s/namespace.yaml":       "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: roost\n",
		"deploy/k8s/game.yaml":            "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: planet-game\n  namespace: roost\n",
		"deploy/k8s/game-pdb.yaml":        "apiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata:\n  name: planet-game\n  namespace: roost\n",
		"deploy/k8s/service-account.yaml": "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: planet\n  namespace: roost\n",
		"deploy/k8s/network-policy.yaml":  "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: planet-default-deny-ingress\n  namespace: roost\n",
	}
	kept := map[string]string{
		"deploy/k8s/extra.yaml":               "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: mine\n  namespace: team\n",
		"deploy/k8s/secret.game.example.yaml": "# Copy to secret.game.local.yaml\napiVersion: v1\nkind: Secret\nmetadata:\n  namespace: roost\n",
	}
	for rel, body := range legacy {
		if err := os.WriteFile(filepath.Join(target, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range kept {
		if err := os.WriteFile(filepath.Join(target, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := SyncProject(target)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	for rel := range legacy {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s survived sync (removed=%v)", rel, result.Removed)
		}
	}
	for rel := range kept {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was removed; it is not codegen's", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "deploy", "k8s", "base", "kustomization.yaml")); err != nil {
		t.Errorf("base kustomization missing after sync: %v", err)
	}
}

// The generated go.mod, the Dockerfile's builder image and the generator's
// own toolchain requirement are one fact. When the Dockerfile lagged at 1.25
// while the framework needed 1.27, every generated project's image build
// failed at `go mod download` with "go.mod requires go >= 1.27.0".
func TestGeneratedGoVersionMatchesTheGeneratorsOwn(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
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
