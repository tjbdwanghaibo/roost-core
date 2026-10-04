package roostcore_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The service Redis-backed suites (kit/service) are build-tagged and skip themselves when
// REDIS_ADDR is unset, so nothing in `go test ./...` can tell whether they
// ever run. That guarantee lives in the workflow: an integration job that
// sets the variable, runs with the tag and fails on a REDIS_ADDR skip. This
// test pins those three facts so a workflow edit cannot quietly drop one.
func TestCIWorkflowRunsTheRedisSuitesAndRefusesTheirSkip(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	workflow := string(raw)
	for _, want := range []string{
		"REDIS_ADDR: 127.0.0.1:6379",
		"go test -tags integration",
		"go vet -tags integration ./...",
	} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow lost %q", want)
		}
	}
	// The skip guard's jq filter must still match REDIS_ADDR. RR-20261001-01
	// widened it to an alternation of every Redis gate variable, so match the
	// filter's shape rather than its old literal text.
	if !regexp.MustCompile(`\.Output\|test\("[^"]*\bREDIS_ADDR\b[^"]*"\)`).MatchString(workflow) {
		t.Errorf("workflow lost the skip guard's REDIS_ADDR filter (.Output|test(\"…REDIS_ADDR…\"))")
	}
}
