package roostcore_test

import (
	"errors"
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
	// Every workflow, not just ci.yml. Restricting this to one file is what let
	// nightly.yml keep pointing at ./internal/roost through the consolidation:
	// the generator moved under codegen/ and three scheduled fuzz/bench steps
	// would have failed nightly, where nobody reads a green dashboard
	// (整体检查 09-21). Workflows that cd into a GENERATED project are exempt —
	// their ./paths belong to that tree, not to this module.
	raw, err := readSelfContainedWorkflows(t)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	paths := map[string]bool{}
	for _, match := range ciPackagePath.FindAllStringSubmatch(string(raw), -1) {
		paths[match[1]] = true
	}
	if len(paths) == 0 {
		t.Fatal("the workflows name no ./package paths; the matcher or the workflows changed shape")
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info, statErr := os.Stat(filepath.FromSlash(name))
		if statErr != nil || !info.IsDir() {
			t.Errorf("a workflow references ./%s, which is not a directory in this module (renamed or removed package?)", name)
		}
	}
	if t.Failed() {
		t.Logf("paths checked: %s", strings.Join(names, ", "))
	}
}

// readAllWorkflows concatenates every workflow so one matcher covers them all.
func readAllWorkflows() ([]byte, error) {
	paths, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no workflows found")
	}
	sort.Strings(paths)
	var joined []byte
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		joined = append(joined, raw...)
		joined = append(joined, '\n')
	}
	return joined, nil
}

// generatesAProject lists the workflows that cd into a project the generator
// wrote and run `go` there. Their ./paths are paths in THAT tree, not in this
// module, so the check above cannot speak about them.
//
// The list is closed both ways on purpose: a new workflow is either
// self-contained (and gets checked) or named here. Leaving it open is how a
// hand-kept list rots.
var generatesAProject = map[string]bool{
	"framework-compat.yml": true,
	"upgrade-compat.yml":   true,
	"release.yml":          true,
	"demo-publish.yml":     true,
}

func readSelfContainedWorkflows(t *testing.T) ([]byte, error) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, errors.New("no workflows found")
	}
	sort.Strings(paths)
	var joined []byte
	var checked []string
	for _, path := range paths {
		if generatesAProject[filepath.Base(path)] {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		// Full-line comments are prose: a sentence naming ./cmd/roost is not
		// a step that runs there, and a guard that trips on prose gets worked
		// around instead of fixed.
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			joined = append(joined, line...)
			joined = append(joined, '\n')
		}
		checked = append(checked, filepath.Base(path))
	}
	if len(checked) == 0 {
		return nil, errors.New("every workflow is exempt; the list above is wrong")
	}
	t.Logf("workflows checked: %s", strings.Join(checked, ", "))
	return joined, nil
}
