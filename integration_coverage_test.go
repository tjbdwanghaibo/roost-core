package roostcore_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 带 integration tag 的测试文件，每一个都要有人跑：“要跑什么”不能是手写的列表，真相是文件系统里有哪些 `//go:build integration`
// 的测试文件，这条测试让 CI job 与故障矩阵脚本的列表以它为准。

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

// service 测试把 Redis 变体挂在各自的环境变量上，没设的变量让用例静默 SKIP 或落回 Memory 替身：这条测试把测试文件里 os.Getenv 到的
// 每个 Redis 门变量钉到 ci.yml Redis job 的 env 上。
var redisGateVariable = regexp.MustCompile(`os\.Getenv\("((?:[A-Z0-9_]*REDIS[A-Z0-9_]*)|(?:ROOST_[A-Z0-9]*_BACKEND))"\)`)

func TestCIRedisJobSetsEveryRedisGateVariable(t *testing.T) {
	wanted := map[string][]string{}
	// 接线服务已从 wiring/service 平铺到 wiring；仅按实际 service 域查找对应接线，
	// DataEngine 等基础框架的集成环境由故障矩阵负责，不属于这个 Redis 服务 job。
	roots := []string{"service"}
	domains, err := os.ReadDir("service")
	if err != nil {
		t.Fatal(err)
	}
	for _, domain := range domains {
		if domain.IsDir() {
			wire := filepath.Join("wiring", domain.Name())
			if info, err := os.Stat(wire); err == nil && info.IsDir() {
				roots = append(roots, wire)
			}
		}
	}
	for _, root := range roots {
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
	script := filepath.Join("wiring", "scripts", "integration", "dataengine-env.sh")
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
// everywhere in CI. The manual entry wiring/scripts/integration/redis-cluster-suites.sh
// must name every package that has such a file — a package it leaves out is
// run by nobody, and a package it names without one is a `[no test files]`
// or all-skip cell that reports green for nothing.
func TestRedisClusterScriptNamesEveryClusterKeyedSuite(t *testing.T) {
	_, _, tagged := integrationTestPackages(t)
	// 不带 integration 标签、直接读 ROOST_REVIEW_CLUSTER 的用例同样只有这个入口（CI 与隔离环境都没有集群）。
	cluster := mergeSorted(tagged, packagesReadingEnv(t, "ROOST_REVIEW_CLUSTER", false, false)["ROOST_REVIEW_CLUSTER"])
	script := filepath.Join("wiring", "scripts", "integration", "redis-cluster-suites.sh")
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
			t.Errorf("%s: %s has ROOST_REVIEW_CLUSTER-gated tests that no runner names (they skip without a cluster, and CI has none); args: %v", script, dir, args)
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
		if strings.Contains(trimmed, "wiring/scripts/integration/dataengine-env.sh test") {
			found = true
		}
	}
	if !found {
		t.Error("no workflow runs `wiring/scripts/integration/dataengine-env.sh test`; the fault matrix (Mongo primary failover, NATS outage, JetStream leader failover, toxiproxy half-open) has no CI home since the consolidation")
	}
}

// envReadPattern 匹配测试文件里对环境变量名字面量的读取。
var envReadPattern = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("([A-Z0-9_]+)"\)`)

// packagesReadingEnv 返回按变量名分组的包目录：哪些包的 _test.go 直接读取名字等于 name
// 的环境变量（byPrefix 为 true 时以 name 开头即算）。integrationOnly 为 true 时只看 `//go:build integration` 文件；false 时带不带标签都算——
// 不带标签、却要真实依赖才不跳过的用例，同样需要一个设置该变量的入口。
func packagesReadingEnv(t *testing.T, name string, integrationOnly, byPrefix bool) map[string][]string {
	t.Helper()
	found := map[string]map[string]bool{}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "testdata", "vendor", "artifacts":
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
		if integrationOnly && !strings.HasPrefix(strings.TrimLeft(text, "\n"), "//go:build integration") {
			return nil
		}
		for _, match := range envReadPattern.FindAllStringSubmatch(text, -1) {
			variable := match[1]
			if variable != name && !(byPrefix && strings.HasPrefix(variable, name)) {
				continue
			}
			if found[variable] == nil {
				found[variable] = map[string]bool{}
			}
			found[variable][filepath.ToSlash(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for variable, dirs := range found {
		for dir := range dirs {
			out[variable] = append(out[variable], dir)
		}
		sort.Strings(out[variable])
	}
	return out
}

func mergeSorted(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range append(append([]string{}, a...), b...) {
		if !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// ROOST_REDIS_TEST_ADDR 准入的真实 Redis 用例（infra/storage/redis、versionstore、framework/cache 等，多数不带
// integration 标签）只有 ci.yml 的 Redis job 设置这个变量：每个读它的包都必须在该 job 的 go test 参数里，
// 包里测试文件读到的其他单机 Redis 门变量也必须由 job 设置；否则这些用例到处跳过，没人跑。
func TestCIRedisJobRunsEveryRedisTestAddressPackage(t *testing.T) {
	gated := packagesReadingEnv(t, "ROOST_REDIS_TEST_ADDR", false, false)["ROOST_REDIS_TEST_ADDR"]
	if len(gated) == 0 {
		t.Fatal("no test reads ROOST_REDIS_TEST_ADDR; the pattern or the layout changed")
	}
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ci := string(raw)
	args := goTestPackageArgs(ci)
	for _, dir := range gated {
		if !covers(args, dir) {
			t.Errorf("%s gates real-Redis tests on ROOST_REDIS_TEST_ADDR but no ci.yml `go test -tags integration` step runs it (they skip everywhere); go test args: %v", dir, args)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			text, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range redisGateVariable.FindAllStringSubmatch(string(text), -1) {
				name := m[1]
				// 集群、隔离环境、mirror-local 私有环境的变量各有入口，不属于这个单机 Redis job。
				if name == "ROOST_REVIEW_CLUSTER" || strings.HasPrefix(name, "ROOST_DATAENGINE_IT_") || strings.HasPrefix(name, "ROOST_MIRROR_LOCAL_") {
					continue
				}
				if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `:\s*\S`).MatchString(ci) {
					t.Errorf("ci.yml never sets %s, but %s/%s gates its Redis variant on it", name, dir, entry.Name())
				}
			}
		}
	}
}

// 只在显式变量下运行的真实依赖用例要有固定入口：optInEntries 把变量（前缀）对到负责的脚本，每个读取这些变量的
// integration 包，脚本里都要有一条带 integration 标签、点名该包的 go test；setOnLine 为 true 时这一行还必须把
// 该变量设为 1（开关型变量），为 false 时变量由脚本 source 的 env.sh 提供（地址型变量）。
// 大积压恢复放在 mirror-local.sh 的私有环境（test-core），不放 dataengine-env.sh test：共享隔离环境由多个会话
// 同时使用，10 万条记录的持续写入会干扰别人的用例。
var optInEntries = []struct {
	variablePrefix, script string
	setOnLine              bool
}{
	{"ROOST_DATAENGINE_IT_BACKLOG", filepath.Join("scripts", "mirror-local.sh"), true},
	{"ROOST_MIRROR_LOCAL", filepath.Join("scripts", "mirror-local.sh"), false},
}

func TestOptInRealDependencySuitesHaveAnEntry(t *testing.T) {
	for _, entry := range optInEntries {
		gated := packagesReadingEnv(t, entry.variablePrefix, true, true)
		if len(gated) == 0 {
			t.Errorf("no integration test reads %s*; update optInEntries", entry.variablePrefix)
			continue
		}
		raw, err := os.ReadFile(entry.script)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		for variable, dirs := range gated {
			for _, dir := range dirs {
				found := false
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "#") || !strings.Contains(line, "go test") || !strings.Contains(line, "integration") {
						continue
					}
					setsVariable := !entry.setOnLine || regexp.MustCompile(`(^|\s)`+regexp.QuoteMeta(variable)+`=1(\s|$)`).MatchString(line)
					if setsVariable && covers(strings.Fields(line), dir) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: no `go test -tags integration` line names ./%s with %s available (setOnLine=%v), so the opt-in suite there is run by nobody", entry.script, dir, variable, entry.setOnLine)
				}
			}
		}
	}
}
