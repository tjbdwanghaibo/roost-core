package kit_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ciPackagePath matches the `./dir` and `./dir/sub` arguments handed to
// `go test`, `go vet` and `go run` in the workflow. `./...` is excluded on
// purpose: it names the whole module, not a directory.
var ciPackagePath = regexp.MustCompile(`(?:^|\s)\./([A-Za-z0-9_][A-Za-z0-9_/-]*)`)

// TestCIWorkflowPackagePathsExist pins every package path literal in
// .github/workflows/ci.yml to a directory that actually exists in this module.
//
// The workflow is the one place package names are spelled as strings the Go
// toolchain never checks at build time. When `sync` became `room` in v1.10.0
// the benchmark step kept pointing at ./sync, so that step failed on every run
// while `go test ./...` above it stayed green. A rename that forgets the
// workflow must now fail here, in the ordinary test run, not in CI.
func TestCIWorkflowPackagePathsExist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	paths := map[string]bool{}
	for _, match := range ciPackagePath.FindAllStringSubmatch(string(raw), -1) {
		paths[match[1]] = true
	}
	if len(paths) == 0 {
		t.Fatal("ci.yml names no ./package paths; the matcher or the workflow changed shape")
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info, statErr := os.Stat(filepath.FromSlash(name))
		if statErr != nil || !info.IsDir() {
			t.Errorf("ci.yml references ./%s, which is not a directory in this module (renamed or removed package?)", name)
		}
	}
	if t.Failed() {
		t.Logf("paths checked: %s", strings.Join(names, ", "))
	}
}
