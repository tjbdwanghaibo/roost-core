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

// 服务层 Redis 用例按自己的唯一前缀清理写过的键（infra/storage/redis/redistest）。守住这一点的是 service-redis job 末尾的残留键检查：
// 它必须排在 job 的每个 go test 步骤之后，统计整个实例的键数并在非零时失败。这条测试钉住那一步的存在、位置和判定。
func TestCIRedisJobRefusesKeysLeftBehind(t *testing.T) {
	raw, err := os.ReadFile(".github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	workflow := string(raw)
	start := strings.Index(workflow, "\n  service-redis:\n")
	if start < 0 {
		t.Fatal("ci.yml has no service-redis job")
	}
	job := workflow[start+1:]
	// job 到下一行两格缩进为止：下一个 job 的键，或它前面的两格注释（Windows job 的注释里也有 "go test"）。
	if next := regexp.MustCompile(`\n  [^ \n]`).FindStringIndex(job); next != nil {
		job = job[:next[0]]
	}
	const step = "- name: no Redis keys left behind"
	at := strings.Index(job, step)
	if at < 0 {
		t.Fatalf("service-redis job lost the %q step", step)
	}
	if last := strings.LastIndex(job, "go test "); last > at {
		t.Errorf("a go test step runs after %q; the residual check must come last so it sees every suite's keys", step)
	}
	check := job[at:]
	for _, want := range []string{"redis-cli info keyspace", `if [ "$left" != 0 ]`, "exit 1"} {
		if !strings.Contains(check, want) {
			t.Errorf("%q step lost %q", step, want)
		}
	}
}
