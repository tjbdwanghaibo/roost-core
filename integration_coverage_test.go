package roostcore_test

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
		// 同时要求 physx 的原生套件由专用 runner 执行；下方独立门禁验证入口与用例名。
		if requiresPhysics(text) {
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
		// 以 ROOST_IT_AUTH_ 准入的文件（R11 的鉴权实例用例，变量名里含 REDIS_ADDR 等字样）只属于
		// auth-credentials.sh，见 TestAuthCredentialsScriptNamesEverySuite。
		case strings.Contains(text, "ROOST_IT_AUTH_"):
			kind = ""
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

// 连接 Mod 从环境变量读凭据（R11）的真实依赖用例以 ROOST_IT_AUTH_* 准入，要开了鉴权的 Mongo / Redis / NATS / etcd，CI 与
// 共享隔离环境都不开鉴权，所以它们只有 wiring/scripts/integration/auth-credentials.sh 一个入口（脚本自起私有实例）。
// 脚本要列出每个含这类文件的包，不能列没有的包（那一格是空绿）。
func TestAuthCredentialsScriptNamesEverySuite(t *testing.T) {
	gated := packagesReadingEnv(t, "ROOST_IT_AUTH_", true, true)
	var suites []string
	for _, dirs := range gated {
		suites = mergeSorted(suites, dirs)
	}
	if len(suites) == 0 {
		t.Fatal("no integration test reads a ROOST_IT_AUTH_ variable; the R11 suites moved or the scan changed")
	}
	script := filepath.Join("wiring", "scripts", "integration", "auth-credentials.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	args := goTestPackageArgs(string(raw))
	if len(args) == 0 {
		t.Fatalf("%s has no `go test -tags=integration` line", script)
	}
	for _, dir := range suites {
		if !covers(args, dir) {
			t.Errorf("%s: %s has ROOST_IT_AUTH_-gated tests that no runner names; args: %v", script, dir, args)
		}
	}
	for _, arg := range args {
		if dir := strings.TrimPrefix(arg, "./"); strings.HasSuffix(arg, "/...") || !covers(suites, dir) {
			t.Errorf("%s names %s, which has no ROOST_IT_AUTH_ test files (or is a pattern): that cell reports green for nothing", script, arg)
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
	// JetStream 日志原语的真实语义与 leader kill：私有三节点集群，地址与数据目录由脚本 test 子命令导出。
	{"ROOST_NATS_CLUSTER", filepath.Join("scripts", "nats-cluster.sh"), false},
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

// 生成工程里需要真实依赖的用例：codegen/internal/entity/testdata 下复制进临时生成工程运行的目录、demo/ 下进入
// game-demo 的 *_test.go.tmpl 模板，以及仓库内不带 integration 标签、把用例写进新生成工程再运行的测试（读
// ROOST_DATAENGINE_IT_*、又不在 dataengine-env.sh test 的包清单里）。go test ./... 看不到前两类，门变量没人设置时
// 第三类到处跳过；每条用例都要由 generatedRealDependencyEntries 的某个脚本以 -run 选中，脚本挂在
// dataengine-env.sh test（nightly fault-matrix job 调用）里，或在 generatedRealDependencyElsewhere 里写明由
// 哪个专门入口（性能、私有环境）负责。testdata 目录按整目录计（真实依赖的准入常写在共用辅助函数里），模板与仓库内
// 测试按读到真实依赖变量的 Test 函数计。
var generatedRealDependencyEntries = []struct {
	source, script, run string
}{
	{"codegen/internal/entity/testdata/dataengine", "scripts/test-dataengine-generated.sh", "^TestGeneratedDataEngine"},
	{"codegen/internal/entity/testdata/remoteflow", "scripts/test-remote-generated.sh", "^TestGeneratedRemote"},
	{"scripts/testdata/remoteflow", "scripts/test-remote-generated.sh", "^TestGeneratedRemote"},
	{"modules/alliance/files/internal/service/alliance/admin_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestAllianceStopFoundingSendsNewAlliancesToTheOtherInstances$"},
	{"modules/purchase/files/game/purchase/queue_integration_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestPurchaseRedis"},
	{"modules/mailbox/files/game/controllers/mailbox/mailbox_real_integration_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestMailboxRealDependencies"},
	{"demo/internal/service/game/activity_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestActivityBoardKeepsTheLatestCumulativeScore$"},
	{"demo/game/controllers/player/finish_dungeon_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestDungeonClearsBoardCountsEachRunOnceRealRedis$"},
	{"demo/db/migrations/player_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestPlayerMigrationRealMongo$"},
	{"demo/internal/service/game/terrain_test.go.tmpl", "scripts/test-demo-generated.sh", "^(TestTerrainRestartRealMongo|TestTerrainEditLedgerRealMongo|TestBuildingRestartRealMongo)$"},
	{"demo/internal/service/game/terrain_building_test.go.tmpl", "scripts/test-demo-generated.sh", "^(TestTerrainRestartRealMongo|TestTerrainEditLedgerRealMongo|TestBuildingRestartRealMongo)$"},
	{"codegen/internal/roost", "scripts/test-demo-generated.sh", "^TestGateGeneratedBusinessConsumer$"},
	{"codegen/internal/roost", "scripts/test-demo-generated.sh", "^TestGeneratedSagaNativeStepsAndEffectConsumer$"},
	{"codegen/internal/dao/testdata/runtime", "codegen/scripts/dao-golden-runtime.sh", "^Test"},
	{"demo/internal/service/game/terrain_wal_crash_test.go.tmpl", "scripts/test-demo-generated.sh", "^TestTerrainWALCrashRealMongo$"},
}

// 由专门入口负责、不进 dataengine-env.sh test 的用例：mention 必须原样出现在 script 里。
var generatedRealDependencyElsewhere = []struct {
	test, script, mention, why string
}{
	{"TestAllianceE2EPreloadOrder", "scripts/test-alliance-e2e.sh", "TestAllianceE2EPreloadOrder", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2ECrashTakeoverAndScaleIn", "scripts/test-alliance-e2e.sh", "TestAllianceE2ECrashTakeoverAndScaleIn", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2EOneAllianceUnderRace", "scripts/test-alliance-e2e.sh", "TestAllianceE2EOneAllianceUnderRace", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2ERedisOutage", "scripts/test-alliance-e2e.sh", "TestAllianceE2ERedisOutage", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2EGamePlayers", "scripts/test-alliance-e2e.sh", "TestAllianceE2EGamePlayers", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2ERejectedFoundingAfterRestart", "scripts/test-alliance-e2e.sh", "TestAllianceE2ERejectedFoundingAfterRestart", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2ERobotOfflineJoin", "scripts/test-alliance-e2e.sh", "TestAllianceE2ERobotOfflineJoin", "联盟服务阶段 5 真实依赖端到端：生成 game-demo 起真实进程（alliance 两实例、game 两进程与九个托管服务），kill -9 / SIGSTOP / 断 toxiproxy 的 redis 代理，自起私有 etcd；只对 scripts/mirror-local.sh 起的私有环境跑（会断开它的 redis 代理），约 7 分钟（本机实测 400s，含生成与编译），不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2ERobotCrossGameJoin", "scripts/test-alliance-e2e.sh", "TestAllianceE2ERobotCrossGameJoin", "联盟服务阶段 5 真实依赖端到端（同上入口与环境）：生成工程的机器人跑 alliance_cross_join，两个 game 进程上的玩家互相入会，ALLIANCE_E2E_ROBOT_LOGIN=portal 走登录入口；不进 dataengine-env.sh test 与 pretag"},
	{"TestAllianceE2EShortPerf", "scripts/test-alliance-e2e.sh", "TestAllianceE2EShortPerf", "联盟服务阶段 5 短测：ALLIANCE_E2E_PERF=1 时由 scripts/test-alliance-e2e.sh 跑（单轮 ≤ 30 分钟），性能入口"},
	{"TestAllianceE2EMirrorRevalidation", "scripts/test-alliance-e2e.sh", "TestAllianceE2EMirrorRevalidation", "联盟服务阶段 5 短测：ALLIANCE_E2E_PERF=1 时由 scripts/test-alliance-e2e.sh 跑（单轮 ≤ 30 分钟），性能入口"},
	{"TestAllianceE2ERPCConcurrency", "scripts/test-alliance-e2e.sh", "TestAllianceE2ERPCConcurrency", "总线 RPC 接收端派发前后对照：ALLIANCE_E2E_PERF=1 时由 scripts/test-alliance-e2e.sh 跑（单轮约 2 分钟），性能入口"},
	{"TestAllianceE2EJoinRetryableSources", "scripts/test-alliance-e2e.sh", "TestAllianceE2EJoinRetryableSources", "写额度 512、256 并发 Join 的“可重试”按答复点归类：ALLIANCE_E2E_PERF=1 时由 scripts/test-alliance-e2e.sh 跑（3 轮各 15 秒，约 3 分钟加生成与起服），来源定位入口"},
	{"TestAllianceE2EWarScanStartup", "scripts/test-alliance-e2e.sh", "TestAllianceE2EWarScanStartup", "联盟起服宣战补发扫描随联盟数的代价：ALLIANCE_E2E_PERF=1 时由 scripts/test-alliance-e2e.sh 跑（每档 ≤ 30 分钟），性能入口"},
	{"TestAllianceReportWindowFollowsTheCoordinatorReservationTTL", "scripts/test-slg-generated.sh", "'^TestAllianceReportWindowFollowsTheCoordinatorReservationTTL$'", "缺省模式就跑，不需要真实依赖：内存版本存储 + 只记次数的协调器替身，补报截止跟随协调器的 ReservationTTL（零值按 DefaultReservationTTL）；夹具与真实依赖用例同包，所以登记在这张表"},
	{"TestAllianceReceiverBudgetRejectsLateSuccess", "scripts/test-slg-generated.sh", "'^TestAllianceReceiverBudgetRejectsLateSuccess$'", "缺省模式就跑，不需要真实依赖：bus.NewRPCContext 以不带期限的基础 context 模拟 dispatchRpc，接收端预算到期后按 ctx 等待的回调结束、迟到的成功不回复、期限内成功照常回复、非正预算拒绝；夹具与真实依赖用例同包，所以登记在这张表"},
	{"TestSLGAllianceRPCReceiverBudget", "scripts/test-slg-generated.sh", "'^TestSLGAllianceRPCReceiverBudget$'", "ROOST_SLG_REAL=1，只用隔离 NATS：两个真实 Bus 的普通 RPC，调用方 300ms 超时返回时接收端回调仍在运行且其 context 没有调用方期限（框架现行契约：dispatchRpc 用接收端 Bus 的基础 context）；接收端 receiverBudget 200ms 到期后迟到成功不回复成功、期限内成功照常回复；不含 JetStream RPC（它按调用方截止建立处理期限）"},
	{"TestSLGAllianceActivity", "scripts/test-slg-generated.sh", "'^TestSLGAllianceActivity$'", "ROOST_SLG_REAL=1，隔离 Redis / NATS：业务 Redis CAS 联盟，两个静态 sid 客户端经正式 Activity RPC 调用协调器子进程（正常排空重启两次）；成员撤权与 Interest alliance / scout 来源归并、Receiver 解码视野帧（权限读取失败不发布、重连新 Full、旧 epoch 被拒）；协调器已应用而本地未确认的贡献在重启后补报只计一次，超过 ReservationTTL 拒绝自动补报；冻结与两服阶段屏障；原服奖励义务写入未 ACK 时重启协调器，OwedDispatches 找回原窗口、原 token 重投不改记录；不含 Player 到账、Remote 联盟权威或 TCP 客户端"},
	{"TestSLGAlliancePlayerDelivery", "scripts/test-slg-generated.sh", "'^TestSLGAlliancePlayerDelivery$'", "ROOST_SLG_REAL=1，同一条活动流程 ACK 后继续：每个原服依次起 write / recover / verify 子进程，各用 strict 文件 WAL 与真实 Mongo 的唯一库；建档与到账两条记录未投影、原服未标记完成时被 SIGKILL，冷加载后钱包与崩溃前一致，从原服义务账本发现 1 条待发并重投不增币，第三进程发现 0 条；并发重放、错服、改金额被拒；不含实体定时器、两服并发或结果未知"},
	{"TestSLGAllianceRemoteDelivery", "scripts/test-slg-generated.sh", "'^TestSLGAllianceRemoteDelivery$'", "ROOST_SLG_REAL=1，联盟权威换成生成的 Remote 托管实体（库名映射绑定到唯一库），同一父进程内 sid 2000 交接到 2001：marker / route epoch 上升、旧 Runtime 写入被拒、冷加载事实一致后继续补报、阶段与两个原服 Player 的 SIGKILL 到账恢复；不含跨机分区、lease 中途过期、Mirror 或 JetStream"},
	{"TestMMORPGRealPersistenceAcrossProcesses", "scripts/test-mmorpg-generated.sh", "^TestMMORPGRealPersistenceAcrossProcesses$", "ROOST_MMORPG_REAL=1，两个独立进程先后用同一文件 WAL 与真实 Mongo：正常停机后冷加载，续完托管交易、重放战斗领奖，核对 Buff 期限已进驱动索引后启动驱动投递循环（不手发 Tick、不调用 DeliverDue），循环补触发一次离线到期的 Buff（投递计数 1、无重投）；不是崩溃注入"},
	{"TestMMORPGRealCrashRecovery", "scripts/test-mmorpg-generated.sh", "^TestMMORPGRealCrashRecovery$", "ROOST_MMORPG_REAL=1，子进程经 strict 文件 WAL 提交野外战斗在途状态（买家阵亡复活计时、卖家击杀拾取领奖并上 Buff、托管交易卖家已确认）、关掉后台投影使 WAL 留有 12 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，四个实体 DAO 字节与定时器 ID 一致、两个期限已进驱动索引，原参数重试不变；启动驱动投递循环（不手发 Tick），两个过期玩家各补触发一次（计数 2），到期交易退款一次、迟到确认被拒，重新上 Buff 后业务时间推进到期限时循环按时投递（计数 3、无重投）；正常关闭后第三个进程冷加载重放不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestCardRPGRealCrashRecovery", "scripts/test-card-rpg-generated.sh", "^TestCardRPGRealCrashRecovery$", "ROOST_CARD_RPG_REAL=1，子进程经 strict 文件 WAL 提交首登、十连、升级、开局与第一回合，关掉后台投影使 WAL 留有 5 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，核对资产 / 战斗摘要 / 定时器 ID，按新配置重放抽卡与升级不重掷不重扣；冷加载报告的回合期限由驱动 DeliverDue 补触发一次（不启动投递循环），续战获胜、三次领奖一份；正常关闭后第三个进程冷加载核对并重试领奖 / 抽卡不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestTowerDefenseRealCrashRecovery", "scripts/test-tower-defense-generated.sh", "^TestTowerDefenseRealCrashRecovery$", "ROOST_TOWER_DEFENSE_REAL=1，子进程经 strict 文件 WAL 提交四人建档、开房、建塔与准备并打完第一波第一步，关掉后台投影使 WAL 留有 14 条未投影记录后被父进程 SIGKILL（崩溃点在波次进行中）；父进程用同一 WAL 与真实 Mongo 冷加载，整份房间状态（塔 / 路径 / 怪物血量与位置 / 共享金币 / 回执）与定时器 ID 逐字节一致、8 条命令重放不变；启动驱动投递循环（不调用 DeliverDue），冷加载后补触发一次只算一步（投递计数 1），之后按期限逐个投递打完三波、无重投；四人领奖各一份，正常关闭后第三个进程冷加载核对并重试命令与领奖不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestSurvivalExtractionRealCrashRecovery", "scripts/test-survival-extraction-generated.sh", "^TestSurvivalExtractionRealCrashRecovery$", "ROOST_SURVIVAL_EXTRACTION_REAL=1，子进程经 strict 文件 WAL 提交四人建档、开局、四人入场与 6 条命令（制作与撤离读条都在途、一人在野外），关掉后台投影使 WAL 留有 15 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo、晚 10 秒的业务时间冷加载，整份探险状态与定时器 ID 逐字节一致、命令重放不变；启动驱动投递循环（不调用 DeliverDue），冷加载后补触发一次（投递计数 1）：完成制作与撤离、环境伤害只算一次、结束时刻不变；再按期投递一次让三人撤离收局、无重投；四人兑付 100/99/98/98 不重复，正常关闭后第三个进程冷加载核对并重试入场 / 命令 / 兑付不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestMergeCasualRealCrashRecovery", "scripts/test-merge-casual-generated.sh", "^TestMergeCasualRealCrashRecovery$", "ROOST_MERGE_CASUAL_REAL=1，子进程经 strict 文件 WAL 提交首登、两次生产、一次广告到账与开挑战、关掉后台投影使 WAL 留有 5 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，核对存档摘要 / 视图 / 定时器 ID 与驱动索引里的期限，重放生产与原广告证明不重掷不重发；启动驱动投递循环（不手发 Tick、不调用 DeliverDue），循环补触发一次判负过期挑战（投递计数 1），重新开局后业务时间推进到新期限时循环按时投递一次（计数 2、无重投），再开局获胜、三次领奖一份；正常关闭后第三个进程冷加载核对并重试不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestIdleSimRealCrashRecovery", "scripts/test-idle-sim-generated.sh", "^TestIdleSimRealCrashRecovery$", "ROOST_IDLE_SIM_REAL=1，子进程经 strict 文件 WAL 提交初始化、两个工坊各排一个任务、买倍率卡与半程结算、关掉后台投影使 WAL 留有 5 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，核对存档摘要 / 视图 / 定时器 ID 与驱动索引里的期限，重放全部命令得到原回执；启动驱动投递循环（不调用 DeliverDue、不手发 Tick），循环补触发一次只积分到原窗口（投递计数 1），收取后业务时间推进到下一秒的刷新期限时循环按时投递一次（计数 2、无重投），再收取 / 领取 / 每日奖励 / 订单后重放不变；正常关闭后第三个进程冷加载重放全部命令不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestAutoChessRealCrashRecovery", "scripts/test-auto-chess-generated.sh", "^TestAutoChessRealCrashRecovery$", "ROOST_AUTO_CHESS_REAL=1，子进程经 strict 文件 WAL 提交八人建档、成局与每人买卡 / 上阵 / 准备（冻结四场战斗）、关掉后台投影使 WAL 留有 33 条未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，核对整局状态 / 定时器 ID 与驱动索引里的期限，重放 24 条玩家命令得到原回执；启动驱动投递循环（不手发 Tick）只补一个阶段（投递计数 1）、之后每个期限投递一次推进一个阶段到终局、无重投，八人各领奖三次各一份；正常关闭后第三个进程冷加载重放全部命令与领奖不变；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestPartyGameRealCrashRecovery", "scripts/test-party-room-generated.sh", "^TestPartyGameRealCrashRecovery$", "ROOST_PARTY_ROOM_REAL=1，子进程经 strict 文件 WAL 提交三人建档、成局开房、连接与第一回合抢星，关掉后台投影使 WAL 留有未投影记录后被 SIGKILL；新进程冷加载房间逐字节恢复、心跳按持久阶段重新启动、重投输入回执不变，心跳一拍内结算、发奖一次、经 DataEngine 删档，第三个进程核对金币不变、房间不再加载"},
	{"TestMOBARealSettlementAcrossProcesses", "scripts/test-moba-generated.sh", "^TestMOBARealSettlementAcrossProcesses$", "ROOST_MOBA_REAL=1，两个独立进程先后用同一文件 WAL 与真实 Mongo：第一进程经多实体 Sender 结算一局，正常停机后第二进程冷加载，先核对结算身份 / 奖励 / 战绩已恢复，再重放同一结算不重复发放、换胜者被拒；不是崩溃注入，也不恢复进行中的对局"},
	{"TestMOBARealActiveRoundCrashRecovery", "scripts/test-moba-generated.sh", "^TestMOBARealActiveRoundCrashRecovery$", "ROOST_MOBA_REAL=1，子进程经 strict 文件 WAL 把一局 5v5 提交到第 20 帧、关掉后台投影使 WAL 留有未投影记录后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载，核对 20 帧与参考摘要、比赛目录不另分配对局，resume 提升 Epoch 后旧代际被拒，经 RecoverRoomWithSnapshots 重建 Room（可靠通道、已提交门、检查点）续到第 40 帧、成员 sid 不变、可靠通道无包；只覆盖 strict 已确认的提交，不含结果未知或断电"},
	{"TestMOBARealCrossSIDHostRouting", "scripts/test-moba-generated.sh", "^TestMOBARealCrossSIDHostRouting$", "ROOST_MOBA_REAL=1 ROOST_MOBA_ROUTING=1，父进程（固定权威 Round）与两个原服、两个 battle host 共四个子进程经真实 NATS 互调，各用自己的文件 WAL、共用本次唯一 Mongo 库：宿主 2000→2001 交接后旧宿主、旧 session、错来源 sid 被拒；原服 1000 提交两笔奖励未 ack 即被 SIGKILL，重启后重投，十份回执各一份、全部确认；battle host 用 RecoverRoomWithSnapshots 重建 Room，两条下行通道是替身"},
	{"TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL", "scripts/test-moba-generated.sh", "^TestMOBARealActiveRoundSnapshotRecoveryAfterSIGKILL$", "ROOST_MOBA_REAL=1，子进程经 strict 文件 WAL 把一局 5v5 提交到第 100 帧、关掉后台投影（每帧恰好一条未投影记录）后被父进程 SIGKILL；父进程用同一 WAL 与真实 Mongo 冷加载、resume 换代，用 RecoverRoomWithSnapshots 重建 Room；两个客户端从早于 64 帧窗口的第 1 帧 / 第 21 帧请求追帧，经冷加载出的第 100 帧已提交检查点快照恢复（共用一次取快照）、续帧到头部与权威同摘要；可靠通道与广播在发送那一刻都不超过已提交帧，lockstep 切帧 / 快照 / 字节 / 失败 / 追帧页 / unservable 指标增量与实际一致；只覆盖 strict 已确认的提交"},
	{"TestGeneratedDataEnginePressure", "scripts/perf/dataengine.sh", "ROOST_DATAENGINE_PERF=1", "性能入口"},
	{"TestGeneratedRemoteMirrorLocal", "scripts/mirror-local.sh", "^TestGeneratedRemoteMirrorLocal$", "私有环境两进程故障，自起依赖"},
	{"TestNestGameLoad", "scripts/perf/nest-game.sh", "^TestNestGameLoad$", "性能入口"},
	{"TestSpatialComponentNestCommitAndRollback", "scripts/perf/nest-game.sh", "^TestSpatialComponentNestCommitAndRollback$", "nest-game.sh --check-spatial，与负载同一份生成流程"},
	{"TestHeartbeatDriverTicksGeneratedUnits", "scripts/perf/nest-game.sh", "^TestHeartbeatDriverTicksGeneratedUnits$", "nest-game.sh --check-heartbeat（D20 第 4 期心跳驱动接线短测），与负载同一份生成流程"},
	{"TestGeneratedGateNestDAOAndWAL", "codegen/internal/roost/demo_test.go", "^TestGeneratedGate(NestDAOAndWAL|TransportSelection|ProjectionCapabilities)$", "TestGateGeneratedBusinessConsumer 把它写进新生成的 game-demo 后运行"},
	{"TestGeneratedGateProjectionCapabilities", "codegen/internal/roost/demo_test.go", "^TestGeneratedGate(NestDAOAndWAL|TransportSelection|ProjectionCapabilities)$", "同一生成工程入口验证投影包装保留目标能力与失败批次语义"},
	{"TestSagaNativeStepsAndEffectRealDependencies", "codegen/internal/roost/add_saga_test.go", "^TestSagaNativeStepsAndEffectRealDependencies$", "TestGeneratedSagaNativeStepsAndEffectConsumer 把 testdata/saga_native 复制进新生成的工程后运行"},
}

var (
	realDependencyEnvRead = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("((?:ROOST_DATAENGINE_IT_[A-Z0-9_]*)|(?:[A-Z0-9_]*(?:MONGO|REDIS|NATS|ETCD)[A-Z0-9_]*))"\)`)
	testFuncDecl          = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
)

type generatedRealDependencyTest struct{ source, name string }

// discoverGeneratedRealDependencyTests 按上面的三类规则找出用例，source 是 testdata 目录、模板文件或包目录。
func discoverGeneratedRealDependencyTests(t *testing.T, envSuites []string) []generatedRealDependencyTest {
	t.Helper()
	testdataDirs := map[string]bool{}
	var found []generatedRealDependencyTest
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "artifacts":
				return filepath.SkipDir
			}
			return nil
		}
		slashed := filepath.ToSlash(path)
		inTestdata := strings.Contains("/"+slashed, "/testdata/")
		template := strings.HasSuffix(slashed, "_test.go.tmpl")
		if !strings.HasSuffix(slashed, "_test.go") && !strings.HasSuffix(slashed, "_test.go.txt") && !template {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		switch {
		case inTestdata:
			if realDependencyEnvRead.MatchString(text) {
				testdataDirs[filepath.ToSlash(filepath.Dir(path))] = true
			}
		case template:
			// integration 模板可能统一由 helper 读取依赖地址；整个文件显式选择真实环境，所有 Test 都必须有入口。
			if strings.HasPrefix(strings.TrimSpace(text), "//go:build integration") && realDependencyEnvRead.MatchString(text) {
				for _, match := range testFuncDecl.FindAllStringSubmatch(text, -1) {
					found = append(found, generatedRealDependencyTest{slashed, match[1]})
				}
				return nil
			}
			for _, name := range testFuncsReading(text, realDependencyEnvRead) {
				found = append(found, generatedRealDependencyTest{slashed, name})
			}
		case !strings.HasPrefix(strings.TrimLeft(text, "\n"), "//go:build integration"):
			dir := filepath.ToSlash(filepath.Dir(path))
			if covers(envSuites, dir) {
				return nil
			}
			for _, name := range testFuncsReading(text, regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("ROOST_DATAENGINE_IT_[A-Z0-9_]*"\)`)) {
				found = append(found, generatedRealDependencyTest{dir, name})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for dir := range testdataDirs {
		files, err := filepath.Glob(filepath.Join(filepath.FromSlash(dir), "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		// embed 夹具与直接复制的 Go 测试都必须登记真实依赖入口。
		embedded, err := filepath.Glob(filepath.Join(filepath.FromSlash(dir), "*_test.go.txt"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, embedded...)
		for _, file := range files {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range testFuncDecl.FindAllStringSubmatch(string(raw), -1) {
				found = append(found, generatedRealDependencyTest{dir, match[1]})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].source != found[j].source {
			return found[i].source < found[j].source
		}
		return found[i].name < found[j].name
	})
	return found
}

// testFuncsReading 返回函数体里有 pattern 匹配的 Test 函数（函数体取到下一个顶层 func 为止）。
func testFuncsReading(text string, pattern *regexp.Regexp) []string {
	var names []string
	bounds := testFuncDecl.FindAllStringSubmatchIndex(text, -1)
	for _, bound := range bounds {
		end := len(text)
		if next := strings.Index(text[bound[1]:], "\nfunc "); next >= 0 {
			end = bound[1] + next
		}
		if pattern.MatchString(text[bound[0]:end]) {
			names = append(names, text[bound[2]:bound[3]])
		}
	}
	return names
}

// shellFunctionBody 取 `name() {` 到下一个顶格 `}` 之间、去掉整行注释的内容。
func shellFunctionBody(t *testing.T, script, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(script))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != name+"() {" {
			continue
		}
		var body []string
		for _, inner := range lines[i+1:] {
			if inner == "}" {
				return body
			}
			if !strings.HasPrefix(strings.TrimSpace(inner), "#") {
				body = append(body, inner)
			}
		}
	}
	t.Fatalf("%s has no %s() function", script, name)
	return nil
}

func TestGeneratedProjectRealDependencyTestsHaveAnEntry(t *testing.T) {
	const envScript = "wiring/scripts/integration/dataengine-env.sh"
	raw, err := os.ReadFile(filepath.FromSlash(envScript))
	if err != nil {
		t.Fatal(err)
	}
	envSuites := goTestPackageArgs(string(raw))
	discovered := discoverGeneratedRealDependencyTests(t, envSuites)
	if len(discovered) == 0 {
		t.Fatal("no generated-project real-dependency test found; the discovery rules or the layout changed")
	}

	// 每个入口脚本：-run 原样出现，并在 dataengine-env.sh test 里以缺省模式调用（不带改变选用范围的变量）。
	envTest := shellFunctionBody(t, envScript, "environment_test")
	modeVariables := regexp.MustCompile(`\b(ROOST_REMOTE_FAULT|ROOST_REMOTE_LOAD|ROOST_REMOTE_RUN|ROOST_DATAENGINE_PERF)=`)
	for _, entry := range generatedRealDependencyEntries {
		script, err := os.ReadFile(filepath.FromSlash(entry.script))
		if err != nil {
			t.Errorf("%s: %v", entry.source, err)
			continue
		}
		if !strings.Contains(string(script), "'"+entry.run+"'") {
			t.Errorf("%s no longer selects '%s' (the -run for %s)", entry.script, entry.run, entry.source)
		}
		called := false
		for _, line := range envTest {
			if strings.Contains(line, "bash "+entry.script) && !modeVariables.MatchString(line) {
				called = true
			}
		}
		if !called {
			t.Errorf("%s environment_test does not run `bash %s` in its default mode, so %s is run by nobody (nightly fault-matrix calls dataengine-env.sh test)", envScript, entry.script, entry.source)
		}
	}
	for _, other := range generatedRealDependencyElsewhere {
		script, err := os.ReadFile(filepath.FromSlash(other.script))
		if err != nil || !strings.Contains(string(script), other.mention) {
			t.Errorf("%s is said to be run by %s (%s), but that script does not contain %q (err=%v)", other.test, other.script, other.why, other.mention, err)
		}
	}

	// 每条用例都有入口；每个入口与“别处”条目都至少对应一条用例。
	entryUsed := make([]bool, len(generatedRealDependencyEntries))
	elsewhereUsed := map[string]bool{}
	for _, test := range discovered {
		covered := false
		for i, entry := range generatedRealDependencyEntries {
			if entry.source == test.source && regexp.MustCompile(entry.run).MatchString(test.name) {
				covered, entryUsed[i] = true, true
			}
		}
		for _, other := range generatedRealDependencyElsewhere {
			if other.test == test.name {
				covered, elsewhereUsed[other.test] = true, true
			}
		}
		if !covered {
			t.Errorf("%s (%s) needs real dependencies but no entry runs it: add it to a script's -run in generatedRealDependencyEntries (hooked into dataengine-env.sh test) or name its dedicated entry in generatedRealDependencyElsewhere", test.name, test.source)
		}
	}
	for i, entry := range generatedRealDependencyEntries {
		if !entryUsed[i] {
			t.Errorf("entry %s -run '%s' matches no real-dependency test in %s: an empty cell reports green for nothing", entry.script, entry.run, entry.source)
		}
	}
	for _, other := range generatedRealDependencyElsewhere {
		if !elsewhereUsed[other.test] {
			t.Errorf("generatedRealDependencyElsewhere names %s, which is not a generated-project real-dependency test any more", other.test)
		}
	}
	if t.Failed() {
		for _, test := range discovered {
			t.Logf("discovered: %s %s", test.source, test.name)
		}
	}
}

// requiresPhysics 依据构建表达式区分原生测试，不能只调整 tag 顺序绕过覆盖。
func requiresPhysics(text string) bool {
	line := strings.SplitN(strings.TrimLeft(text, "\n"), "\n", 2)[0]
	expr, err := constraint.Parse(line)
	if err != nil {
		return false
	}
	enabled := func(integration, native bool) bool {
		return expr.Eval(func(tag string) bool {
			switch tag {
			case "integration":
				return integration
			case "cgo":
				return true
			case "physx":
				return native
			default:
				return false
			}
		})
	}
	return enabled(true, true) && !enabled(true, false) && !enabled(false, true)
}

func TestPhysicsServiceRunnerCoversNativeIntegration(t *testing.T) {
	raw, err := os.ReadFile("scripts/physics/verify-service.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	args := goTestPackageArgs(script)
	testNames := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	count := 0
	for _, root := range []string{"service", "wiring"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			text := string(content)
			if !requiresPhysics(text) {
				return nil
			}
			count++
			if !covers(args, filepath.ToSlash(filepath.Dir(path))) {
				t.Errorf("native integration suite missing from physics runner: %s", path)
			}
			for _, name := range testNames.FindAllStringSubmatch(text, -1) {
				if !strings.Contains(script, name[1]) {
					t.Errorf("physics runner -run does not name %s", name[1])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if count == 0 {
		t.Fatal("physics integration suites were not discovered")
	}
}

// 生成器签名变了而玩法纵切夹具没人跑：v1.33.0 上 card-rpg / mmorpg / merge-casual 三个夹具因 D22-1 改掉 Tick 的
// 业务时间参数编译失败，九个 scripts/test-<玩法>-generated.sh 当时不在任何门禁里，没人发现。现在它们由 pretag
// 每次发版在缺省模式（离线、替身提交器）跑一遍；三个离线生成器专项（heartbeat / level / sync-modes）同批跑
// （维护者 2026-10-31 决定纳入）。pretagExemptGeneratedScripts 是不进 pretag 的生成工程脚本（目前都要外部依赖），
// 每条写明由谁跑；新增的 scripts/test-*-generated.sh 要么在 pretag 里，要么在这里登记原因。
// 玩法接入手册（docs/framework/*-INTEGRATION.md）点名的其他验收脚本同样适用：scripts/test-mmorpg-scenes.sh
// 不匹配 *-generated.sh，v1.35.0 起 DAO 同步载荷换成 PB 后它的 BSON 客户端一直红，直到 2026-11-04 才发现。
var pretagExemptGeneratedScripts = map[string]string{
	"scripts/test-mmorpg-cross-sid.sh":     "需要已启动的双 sid game-demo 与隔离 Mongo / NATS / Redis / etcd；按 MMORPG-INTEGRATION §13 手动跑",
	"scripts/test-dataengine-generated.sh": "需要隔离 Mongo；由 wiring/scripts/integration/dataengine-env.sh test 跑（见 generatedRealDependencyEntries）",
	"scripts/test-remote-generated.sh":     "需要隔离 Mongo / Redis / NATS；由 dataengine-env.sh test 与 mirror-local.sh 跑",
	"scripts/test-demo-generated.sh":       "需要隔离 Mongo / Redis / NATS；由 dataengine-env.sh test 与 mirror-local.sh 跑；game-demo 的离线 build / vet / test 在 go test ./... 与 release.sh 里",
	"scripts/test-alliance-e2e.sh":         "联盟服务真实依赖端到端：需要 scripts/mirror-local.sh 起的私有环境（含 toxiproxy）与 etcd，约 7 分钟（本机实测 400s，含生成与编译）；按 MMORPG-INTEGRATION“公会与跨服读取”手动跑",
}

// TestGeneratedScriptsAreInPretag：每个 scripts/test-*-generated.sh，以及玩法接入手册点名的每个 scripts/test-*.sh，
// 都在 scripts/pretag.sh 的非注释行里出现，或登记在 pretagExemptGeneratedScripts。保证的是“发版时调用”这一层：
// 只查脚本路径写在 pretag 的命令里，不检查脚本本身是否真的跑到了用例（那由各脚本自己的通过数断言负责）；
// 登记了豁免却已删除（或手册不再点名）的脚本同样报错，免得豁免表过期。
func TestGeneratedScriptsAreInPretag(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("scripts/pretag.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var commands strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		commands.WriteString(line)
		commands.WriteByte('\n')
	}
	scripts, err := filepath.Glob(filepath.FromSlash("scripts/test-*-generated.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) == 0 {
		t.Fatal("no scripts/test-*-generated.sh found; the guard is looking in the wrong place")
	}
	// 手册里写成验收入口的脚本不一定叫 *-generated.sh（如多地图 TCP 的 test-mmorpg-scenes.sh），一并纳入。
	manuals, err := filepath.Glob(filepath.FromSlash("docs/framework/*-INTEGRATION.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`scripts/test-[a-z0-9-]+\.sh`)
	for _, manual := range manuals {
		text, err := os.ReadFile(manual)
		if err != nil {
			t.Fatal(err)
		}
		for _, script := range named.FindAllString(string(text), -1) {
			if _, err := os.Stat(filepath.FromSlash(script)); err != nil {
				t.Errorf("%s names %s, which does not exist", filepath.ToSlash(manual), script)
				continue
			}
			if !strings.HasSuffix(script, "-generated.sh") && !slices.Contains(scripts, filepath.FromSlash(script)) {
				scripts = append(scripts, filepath.FromSlash(script))
			}
		}
	}
	seen := map[string]bool{}
	genre := 0
	for _, path := range scripts {
		script := filepath.ToSlash(path)
		seen[script] = true
		inPretag := strings.Contains(commands.String(), script)
		why, exempt := pretagExemptGeneratedScripts[script]
		switch {
		case inPretag && exempt:
			t.Errorf("%s runs in scripts/pretag.sh but is still exempted (%s); drop the exemption", script, why)
		case !inPretag && !exempt:
			t.Errorf("%s is not run by scripts/pretag.sh: a generator change that breaks its fixtures would ship unnoticed; add it to pretag's generated-project list, or exempt it in pretagExemptGeneratedScripts with who runs it", script)
		case inPretag:
			genre++
		}
	}
	for script := range pretagExemptGeneratedScripts {
		if !seen[script] {
			t.Errorf("pretagExemptGeneratedScripts names %s, which no longer exists", script)
		}
	}
	// 每个玩法接入手册（docs/framework/<玩法>-INTEGRATION.md）都有自己的纵切脚本，脚本必须在 pretag 里。
	for _, manual := range manuals {
		name := strings.ToLower(strings.TrimSuffix(filepath.Base(manual), "-INTEGRATION.md"))
		script := "scripts/test-" + name + "-generated.sh"
		if !seen[script] {
			t.Errorf("%s has no %s", filepath.ToSlash(manual), script)
			continue
		}
		if _, exempt := pretagExemptGeneratedScripts[script]; exempt {
			t.Errorf("%s is the vertical slice of %s and must run in pretag, not be exempted", script, filepath.ToSlash(manual))
		}
	}
	if genre == 0 {
		t.Error("scripts/pretag.sh runs no generated-project script")
	}
}
