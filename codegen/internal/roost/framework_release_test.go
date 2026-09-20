package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrameworkReleaseManifestStrictValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "framework-release.yaml")
	valid := []byte("schema: 2\ncodegen: v1.15.0\nframework: {core: v1.14.0, kit: v1.13.0}\nconsumer_go: [1.25.x, 1.26.x]\n")
	if err := os.WriteFile(path, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadFrameworkReleaseManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Framework.Kit != "v1.13.0" || len(manifest.ConsumerGo) != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	for name, raw := range map[string][]byte{
		"unknown":  bytes.Replace(valid, []byte("schema: 2"), []byte("schema: 2\nunknown: true"), 1),
		"latest":   bytes.Replace(valid, []byte("core: v1.14.0"), []byte("core: latest"), 1),
		"go":       bytes.Replace(valid, []byte("1.26.x"), []byte("1.26.0"), 1),
		"old-go":   bytes.Replace(valid, []byte("1.26.x"), []byte("1.24.x"), 1),
		"empty-go": bytes.Replace(valid, []byte("[1.25.x, 1.26.x]"), []byte("[]"), 1),
		// schema 1 still listed skill and service; the folded-in modules must not
		// be declared any more, and the old schema number is refused outright.
		"schema-1":       bytes.Replace(valid, []byte("schema: 2"), []byte("schema: 1"), 1),
		"legacy-modules": bytes.Replace(valid, []byte("kit: v1.13.0}"), []byte("kit: v1.13.0, skill: v1.10.3}"), 1),
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
	manifest := FrameworkReleaseManifest{Codegen: "v1.10.0", Framework: FrameworkReleaseVersionSpec{Core: "v1.9.1", Kit: "v1.9.2"}, ConsumerGo: []string{"1.25.x", "1.26.x"}}
	path := filepath.Join(t.TempDir(), "github", "output")
	if err := appendFrameworkGitHubOutput(path, manifest); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codegen=v1.10.0", "core=v1.9.1", "kit=v1.9.2", "consumer_go=1.25.x,1.26.x", `consumer_go_json=["1.25.x","1.26.x"]`} {
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
