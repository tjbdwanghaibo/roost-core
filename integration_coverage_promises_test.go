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
// `//go:build integration` test file, grouped by the environment those files
// key on: REDIS_ADDR (a Redis-only suite), the full ROOST_DATAENGINE_IT
// environment, or ROOST_REVIEW_CLUSTER (a real Redis Cluster that no CI job
// provides; those suites have their own manual entry, see
// TestRedisClusterScriptNamesEveryClusterKeyedSuite).
//
// A directory is Redis-only when any of its files keys on REDIS_ADDR (the
// Redis job runs the whole package), full-environment when it has a
// full-environment file and no Redis-only one, and cluster when any file keys
// on ROOST_REVIEW_CLUSTER — that last set overlaps the other two on purpose:
// the cluster file inside a Redis-only or full-environment package is still
// run by nobody unless the cluster script names the package.
func integrationTestPackages(t *testing.T) (redisOnly, fullEnv, cluster []string) {
	t.Helper()
	kinds := map[string]map[string]bool{}
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
		if kinds[dir] == nil {
			kinds[dir] = map[string]bool{}
		}
		// A file may gate some tests on REDIS_ADDR and others on the cluster
		// (service/mail does), so the cluster flag is independent of the
		// Redis-only / full-environment split; a file keyed on the cluster
		// alone belongs to no other runner.
		kind := "full"
		switch {
		case strings.Contains(text, "ROOST_DATAENGINE_IT"):
		case strings.Contains(text, "REDIS_ADDR"):
			kind = "redis"
		case strings.Contains(text, "ROOST_REVIEW_CLUSTER"):
			kind = ""
		}
		if kind != "" {
			kinds[dir][kind] = true
		}
		if strings.Contains(text, "ROOST_REVIEW_CLUSTER") {
			kinds[dir]["cluster"] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir, has := range kinds {
		switch {
		case has["redis"]:
			redisOnly = append(redisOnly, dir)
		case has["full"]:
			fullEnv = append(fullEnv, dir)
		}
		if has["cluster"] {
			cluster = append(cluster, dir)
		}
	}
	sort.Strings(redisOnly)
	sort.Strings(fullEnv)
	sort.Strings(cluster)
	if len(redisOnly) == 0 || len(fullEnv) == 0 {
		t.Fatalf("expected both Redis-only and full-environment integration suites; got redis=%v full=%v", redisOnly, fullEnv)
	}
	return redisOnly, fullEnv, cluster
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
	redisOnly, _, _ := integrationTestPackages(t)
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

// RR-20261001-01：service 测试把 Redis 变体挂在哪个环境变量上是各轮审查自己定的
// （REDIS_ADDR、ROOST_REVIEW_REDIS、ROOST_REDIS_TEST_ADDR、ROOST_REVIEW3_BACKEND、ROOST_BUGFIX5_BACKEND…），
// 没设的变量让用例静默 SKIP 或落回 Memory 替身，Redis job 看起来绿、其实没跑。
// 这条测试把测试文件里 os.Getenv 到的每个 Redis 门变量钉到 ci.yml Redis job 的 env 上。
var redisGateVariable = regexp.MustCompile(`os\.Getenv\("((?:[A-Z0-9_]*REDIS[A-Z0-9_]*)|(?:ROOST_[A-Z0-9]*_BACKEND))"\)`)

func TestCIRedisJobSetsEveryRedisGateVariable(t *testing.T) {
	wanted := map[string][]string{}
	for _, root := range []string{filepath.Join("kit", "service"), "service"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return walkErr
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range redisGateVariable.FindAllStringSubmatch(string(raw), -1) {
				name := m[1]
				// A real Redis Cluster has its own manual runner (redis-cluster-suites.sh).
				if name == "ROOST_REVIEW_CLUSTER" {
					continue
				}
				wanted[name] = append(wanted[name], filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(wanted) == 0 {
		t.Fatal("no service test reads a Redis gate variable; the regexp or the layout changed")
	}
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(raw)
	for name, files := range wanted {
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `:\s*\S`).MatchString(ci) {
			t.Errorf("ci.yml never sets %s, but %v gate their Redis variants on it (they skip or fall back to the in-memory store, and the job reports green for nothing)", name, files)
		}
	}
}

// The fault-matrix script runs `go test` from the module root and must name
// exactly the full-environment suites:
// every one of them (a missing package is a silently empty cell) and nothing
// that has no integration tests (a `[no test files]` cell reports green for
// nothing).
func TestFaultMatrixScriptNamesEveryFullEnvironmentSuite(t *testing.T) {
	_, fullEnv, _ := integrationTestPackages(t)
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

// Suites keyed on ROOST_REVIEW_CLUSTER need a real Redis Cluster, which
// neither the Redis job nor the isolated environment provides, so they skip
// everywhere in CI. The manual entry kit/scripts/integration/redis-cluster-suites.sh
// must name every package that has such a file — a package it leaves out is
// run by nobody, and a package it names without one is a `[no test files]`
// or all-skip cell that reports green for nothing.
func TestRedisClusterScriptNamesEveryClusterKeyedSuite(t *testing.T) {
	_, _, cluster := integrationTestPackages(t)
	script := filepath.Join("kit", "scripts", "integration", "redis-cluster-suites.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	args := goTestPackageArgs(string(raw))
	if len(args) == 0 {
		t.Fatalf("%s has no `go test -tags=integration` line", script)
	}
	for _, dir := range cluster {
		if !covers(args, dir) {
			t.Errorf("%s: %s has ROOST_REVIEW_CLUSTER integration tests that no runner names (they skip without a cluster, and CI has none); args: %v", script, dir, args)
		}
	}
	for _, arg := range args {
		if strings.HasSuffix(arg, "/...") {
			t.Errorf("%s names %s: the cluster list is spelled out per package so that a package without cluster tests cannot hide inside a pattern", script, arg)
			continue
		}
		if dir := strings.TrimPrefix(arg, "./"); !covers(cluster, dir) {
			t.Errorf("%s names %s, which has no ROOST_REVIEW_CLUSTER test files: that cell reports green for nothing", script, arg)
		}
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
