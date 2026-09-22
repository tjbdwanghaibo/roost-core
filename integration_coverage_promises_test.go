package roostcore_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// U-0275 · C2 · RR-20260922-03 / U-0276 · C2 · RR-20260922-02：带 integration tag 的测试文件，
// 每一个都要有人跑。
//
// 旧行为：ci.yml 的 Redis job 只跑 `./kit/service/...`，`service/mail` 的五个 Redis 用例在 glob 之外，
// 而"no Redis test was skipped"守卫只看那个 glob 的输出；故障矩阵脚本跑 `./saga ./remoteentity`
// （两个目录已经没有测试文件）、core 侧四个套件因 `../roost-core` 不存在被跳过且退出码 0，
// 并且没有任何 workflow 调用它。两处的共同点：**"要跑什么"是手写的列表，而真相是文件系统里
// 有哪些 `//go:build integration` 的测试文件。** 这条测试让列表以真相为准。

// integrationTestPackages lists every package directory that has at least one
// `//go:build integration` test file, and whether that file keys on REDIS_ADDR
// (a Redis-only suite) or on the full ROOST_DATAENGINE_IT environment.
func integrationTestPackages(t *testing.T) (redisOnly, fullEnv []string) {
	t.Helper()
	seen := map[string]string{}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "testdata" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		// A build constraint is the file's first line; the same string inside
		// a string literal (this file has one) is not a constraint.
		if !strings.HasPrefix(strings.TrimLeft(text, "\n"), "//go:build integration") {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		kind := "full"
		if strings.Contains(text, "REDIS_ADDR") && !strings.Contains(text, "ROOST_DATAENGINE_IT") {
			kind = "redis"
		}
		if prev, ok := seen[dir]; !ok || prev == "full" {
			seen[dir] = kind
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir, kind := range seen {
		if kind == "redis" {
			redisOnly = append(redisOnly, dir)
		} else {
			fullEnv = append(fullEnv, dir)
		}
	}
	sort.Strings(redisOnly)
	sort.Strings(fullEnv)
	if len(redisOnly) == 0 || len(fullEnv) == 0 {
		t.Fatalf("expected both Redis-only and full-environment integration suites; got redis=%v full=%v", redisOnly, fullEnv)
	}
	return redisOnly, fullEnv
}

// goTestPackageArgs extracts the ./package arguments of every `go test` line
// in text that carries the integration tag.
var goTestIntegration = regexp.MustCompile(`go test[^\n]*-tags[= ]integration[^\n]*`)

func goTestPackageArgs(text string) []string {
	var out []string
	for _, line := range goTestIntegration.FindAllString(text, -1) {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "./") {
				out = append(out, strings.TrimSuffix(field, "/"))
			}
		}
	}
	return out
}

// covers reports whether a package dir is named by one of the go test
// arguments, either literally or through a `/...` pattern.
func covers(args []string, dir string) bool {
	for _, arg := range args {
		if strings.HasSuffix(arg, "/...") {
			prefix := strings.TrimSuffix(strings.TrimPrefix(arg, "./"), "/...")
			if dir == prefix || strings.HasPrefix(dir, prefix+"/") {
				return true
			}
			continue
		}
		if strings.TrimPrefix(arg, "./") == dir {
			return true
		}
	}
	return false
}

// The Redis job in ci.yml must name every Redis-only integration suite, and
// its skip guard is only as good as that list.
func TestCIRedisJobRunsEveryRedisIntegrationSuite(t *testing.T) {
	redisOnly, _ := integrationTestPackages(t)
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	args := goTestPackageArgs(string(raw))
	if len(args) == 0 {
		t.Fatal("ci.yml has no `go test -tags integration` line; the Redis job changed shape")
	}
	for _, dir := range redisOnly {
		if !covers(args, dir) {
			t.Errorf("%s has Redis-backed integration tests that no ci.yml step runs (they skip without REDIS_ADDR, and the skip guard cannot see a package it never ran); go test args: %v", dir, args)
		}
	}
}

// The fault-matrix script runs `go test` from the module root and must name
// exactly the full-environment suites:
// every one of them (a missing package is a silently empty cell) and nothing
// that has no integration tests (a `[no test files]` cell reports green for
// nothing).
func TestFaultMatrixScriptNamesEveryFullEnvironmentSuite(t *testing.T) {
	_, fullEnv := integrationTestPackages(t)
	script := filepath.Join("kit", "scripts", "integration", "dataengine-env.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	args := goTestPackageArgs(string(raw))
	if len(args) == 0 {
		t.Fatalf("%s has no `go test -tags=integration` line", script)
	}
	for _, dir := range fullEnv {
		if !covers(args, dir) {
			t.Errorf("%s: %s has full-environment integration tests the fault matrix never runs; args: %v", script, dir, args)
		}
	}
	// And nothing in the list may point at a directory without such tests.
	for _, arg := range args {
		if strings.HasSuffix(arg, "/...") {
			continue
		}
		dir := strings.TrimPrefix(arg, "./")
		if !covers(fullEnv, dir) {
			t.Errorf("%s names %s, which has no integration-tagged test files: that cell is `[no test files]` and reports green for nothing", script, arg)
		}
	}
	// The old code took the core checkout from ROOST_CORE_DIR with a sibling
	// default; prose about that history is fine, the variable is not.
	if strings.Contains(string(raw), "ROOST_CORE_DIR") {
		t.Errorf("%s still looks for a sibling roost-core checkout; since the consolidation the core suites are in this module and the path does not exist (the script printed \"NOT run\" and exited 0)", script)
	}
}

// Somebody has to call the script. Before the consolidation that was kit's
// own nightly; the subtree merge did not bring kit's .github along.
func TestSomeWorkflowRunsTheFaultMatrix(t *testing.T) {
	raw, err := readAllWorkflows()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "kit/scripts/integration/dataengine-env.sh test") {
			found = true
		}
	}
	if !found {
		t.Error("no workflow runs `kit/scripts/integration/dataengine-env.sh test`; the fault matrix (Mongo primary failover, NATS outage, JetStream leader failover, toxiproxy half-open) has no CI home since the consolidation")
	}
}
