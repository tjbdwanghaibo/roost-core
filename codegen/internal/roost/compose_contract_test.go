package roost

import (
	"bytes"
	"go/parser"
	"go/token"
	"gopkg.in/yaml.v3"
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
