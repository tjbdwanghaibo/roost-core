package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

func TestKitConfigSchemasMatchKitDeclarations(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kit to compare the snapshot")
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "run", "../../../wiring/internal/configschemagen", "-out", "kitconfig_gen.go", "-check")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kitconfig_gen.go is not the kit Mods' current declarations: %v\n%s", err, out)
	}
}

// fullConfigManifest 是覆盖面最大的工程：game 模板（九个框架服务各自成进程、game 调用全部）加上 catalog 里全部带配置的
// Mod、player TCP 接入层与 saga。
func fullConfigManifest(t *testing.T) Manifest {
	t.Helper()
	m := gameTemplateManifest(t)
	game := m.Services["game"]
	for name := range modCatalog {
		if _, declared := kitConfigSchemas[name]; declared && !contains(game.Mods, name) && !contains(m.SharedMods, name) {
			game.Mods = append(game.Mods, name)
		}
	}
	m.Services["game"] = game
	m.Access = map[string]AccessSpec{"player": {Service: "game", Transports: []string{"tcp"}}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

// 生成的每份配置（开发、生产示例、k8s Secret 示例）按服务实际注册的 Mod 的声明检查：类型 / 范围 / 枚举 / 必填都对，
// 框架段里没有任何声明之外的键。
func TestGeneratedConfigsMatchDeclarations(t *testing.T) {
	m := fullConfigManifest(t)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	all := allFrameworkConfigSchema()
	checked := 0
	for _, service := range sortedServiceNames(m) {
		schema, err := serviceConfigSchema(m, service)
		if err != nil {
			t.Fatalf("%s: %v", service, err)
		}
		for _, file := range serviceConfigFiles(service) {
			body, ok := plan[file.rel]
			if !ok {
				continue
			}
			bad, unread, err := checkServiceConfigText(body.Body, file.secret, file.production, schema, configschema.Schema{}, all)
			if err != nil {
				t.Fatalf("%s: %v", file.rel, err)
			}
			for _, problem := range bad {
				t.Errorf("%s: %s", file.rel, problem)
			}
			if len(unread) > 0 {
				t.Errorf("%s: the generator writes keys no mod of %s reads: %s", file.rel, service, strings.Join(unread, ", "))
			}
			checked++
		}
	}
	if checked < 20 {
		t.Fatalf("checked %d config files; the manifest no longer renders the framework services", checked)
	}
}

// 守卫自己要能红：在生成的配置里写一个声明之外的键、一个超出声明范围的值、一个写错类型的值、漏掉一个必填键，
// 检查都要点名它们。
func TestGeneratedConfigCheckCatchesDrift(t *testing.T) {
	m := fullConfigManifest(t)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := serviceConfigSchema(m, "mail")
	if err != nil {
		t.Fatal(err)
	}
	dev := string(plan["configs/service/config.mail.yaml"].Body)
	for _, tc := range []struct{ name, edited, want string }{
		{"undeclared key", strings.Replace(dev, "\nredis:\n", "\nredis:\n  pool_sise: 8\n", 1), "redis.pool_sise is not a key any framework mod declares"},
		{"out of range", strings.Replace(dev, "  pool_size: 32\n", "  pool_size: -1\n", 1), "redis.pool_size must not be negative"},
		{"wrong type", strings.Replace(dev, "  claim_lease: 30s\n", "  claim_lease: 30\n", 1), "mail.claim_lease = 30 needs a unit"},
		{"missing required", strings.Replace(dev, "  send_ttl: 720h\n", "", 1), "mail.send_ttl is required"},
	} {
		if tc.edited == dev {
			t.Fatalf("%s: the edit did not apply; the generated mail config changed shape:\n%s", tc.name, dev)
		}
		bad, _, err := checkServiceConfigText([]byte(tc.edited), false, false, schema, configschema.Schema{}, allFrameworkConfigSchema())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(bad, "\n"), tc.want) {
			t.Errorf("%s: check = %q, want %q", tc.name, bad, tc.want)
		}
	}
}

// 生成器的几处常量与声明的示例值同源：停机预算、目录。改了声明而没改常量（或反过来），生成的预算 / 部署挂载与
// 配置文件对不上。
func TestGeneratorConstantsMatchTheDeclaredExamples(t *testing.T) {
	for _, tc := range []struct {
		group, key, want string
	}{
		{"dataengine", "dataengine.startup_timeout", seconds(generatedDataEngineStartupTimeout)},
		{"dataengine", "dataengine.shutdown_timeout", seconds(generatedDataEngineShutdownTimeout)},
		{"statslog", "stats_log.dir", defaultStatsLogDir},
		{"configdata", "config_data.dir", defaultConfigDataDir},
		{"activity", "activity.groups_file", activityGroupsFile},
	} {
		key, ok := kitConfigSchemas[tc.group].Lookup(tc.key)
		if !ok || !key.Starter || key.Example != tc.want {
			t.Errorf("%s example = %q (declared=%v), the generator's constant is %q", tc.key, key.Example, ok, tc.want)
		}
	}
	if key, _ := playerTCPSchema().Lookup("player_access.tcp.shutdown_timeout"); key.Example != seconds(generatedPlayerTCPShutdownTimeout) {
		t.Errorf("player_access.tcp.shutdown_timeout example = %q, generatedPlayerTCPShutdownTimeout = %s", key.Example, seconds(generatedPlayerTCPShutdownTimeout))
	}
}

// 生成的 player TCP 接入层也读 kit/nest 的 nest.request_timeout（派发预算的回退）：两份声明必须相同，否则 App 启动时
// 合并声明报冲突。
func TestPlayerTCPDeclarationAgreesWithKit(t *testing.T) {
	var kit configschema.Schema
	for _, schema := range kitConfigSchemas {
		kit.Keys = append(kit.Keys, schema.Keys...)
	}
	for _, key := range playerTCPSchema().Keys {
		if declared, ok := kit.Lookup(key.Name); ok {
			if _, err := configschema.Merge(configschema.Schema{Keys: []configschema.Key{declared}}, configschema.Schema{Keys: []configschema.Key{key}}); err != nil {
				t.Errorf("%s: the player TCP declaration %+v differs from kit's %+v", key.Name, key, declared)
			}
		}
	}
	if _, ok := playerTCPSchema().Lookup("nest.request_timeout"); !ok {
		t.Fatal("the player TCP declaration no longer reads nest.request_timeout; update this test")
	}
}

// doctor 的 config-schema 检查对新生成的 game-demo（含 demo 模板自己改过的配置：GM 管理端点、player TCP 打开）
// 不报失败。这里只按框架声明检查（不编译工程）：game 的业务代码读的 activity.* / platform.* 在这一半里仍是
// “本服务没有 Mod 声明”的警告；完整的 doctor 编译工程、读回 game 服务自己的声明之后零警告
// （TestDoctorReadsBusinessDeclarationsFromTheProcess）。
func TestGameDemoConfigsPassTheDoctorConfigCheck(t *testing.T) {
	root := copyOfNewProject(t, "game-demo")
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	items := checkConfigDeclarations(root, m, nil)
	if len(items) == 0 {
		t.Fatal("no config-schema items")
	}
	for _, item := range items {
		if item.Status == StatusFail {
			t.Errorf("%s: %s", item.Name, item.Detail)
		}
		if item.Status == StatusWarn {
			t.Logf("%s: %s", item.Name, item.Detail)
		}
	}
}

func projectConfigReadViolations(t *testing.T, root string) (reads, unread []string) {
	t.Helper()
	packages, err := configschema.GoPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) == 0 {
		t.Fatalf("%s has no Go packages", root)
	}
	reads, err = configschema.UndeclaredReads(packages, nil)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, files := range packages {
		all = append(all, files...)
	}
	unread, err = configschema.UnreadFields(map[string][]string{root: all})
	if err != nil {
		t.Fatal(err)
	}
	return reads, unread
}

func TestGeneratedProjectsReadConfigOnlyThroughDeclarations(t *testing.T) {
	t.Parallel()
	roots := map[string]string{}
	for _, fixture := range []string{"game-demo", "saga", "configdata", "bare"} {
		roots[fixture] = copyOfNewProject(t, fixture)
	}
	// 覆盖面最大的形状：game 模板加上全部带配置的 Mod、player TCP 接入层与 saga。
	plan, err := renderProject(fullConfigManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	for rel, file := range plan {
		if strings.HasSuffix(rel, ".go") {
			writeProjectFile(t, full, rel, string(file.Body))
		}
	}
	roots["full-render"] = full
	for name, root := range roots {
		reads, unread := projectConfigReadViolations(t, root)
		for _, read := range reads {
			t.Errorf("%s: %s reads config outside a declaration; declare the key (on the service, see game/settings in game-demo) and read it with app.LoadConfig", name, strings.TrimPrefix(read, root+string(filepath.Separator)))
		}
		for _, field := range unread {
			t.Errorf("%s: %s declares a config key nothing in the project reads", name, strings.TrimPrefix(field, root+string(filepath.Separator)))
		}
	}
}

// 守卫能红：在 game-demo 的副本里加一处直接读 viper、一个没人读的声明字段，两类都被点名。
func TestGeneratedProjectConfigGuardCatchesDrift(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "game-demo")
	settingsPath := filepath.Join(root, "game", "settings", "settings.go")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "\tPaymentSecret string `", "\tForgotten int `config:\"forgotten\"`\n\tPaymentSecret string `", 1)
	if edited == string(raw) {
		t.Fatal("the settings template changed shape; update this test")
	}
	if err := os.WriteFile(settingsPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	gmPath := filepath.Join(root, "internal", "service", "game", "gm.go")
	raw, err = os.ReadFile(gmPath)
	if err != nil {
		t.Fatal(err)
	}
	edited = strings.Replace(string(raw), "\tlocalSID := identity.Sid\n", "\tlocalSID := identity.Sid\n\t_ = registry.Config().GetString(\"gm.secret_knob\")\n", 1)
	if edited == string(raw) {
		t.Fatal("gm.go changed shape; update this test")
	}
	if err := os.WriteFile(gmPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	reads, unread := projectConfigReadViolations(t, root)
	if len(reads) != 1 || !strings.Contains(reads[0], filepath.Join("internal", "service", "game", "gm.go")) {
		t.Errorf("reads = %v; want only the GetString added to gm.go", reads)
	}
	if len(unread) != 1 || !strings.Contains(unread[0], "Forgotten") {
		t.Errorf("unread = %v; want only Forgotten", unread)
	}
	// doctor 的 config-reads 检查是同一个守卫：对用户工程同样点名两处。
	if item := checkConfigReads(root); item.Status != StatusFail || !strings.Contains(item.Detail, "internal/service/game/gm.go:") || !strings.Contains(item.Detail, "Forgotten (config:\"forgotten\")") {
		t.Errorf("doctor config-reads = %s %q; want a FAIL naming gm.go and Forgotten", item.Status, item.Detail)
	}
	if item := checkConfigReads(copyOfNewProject(t, "game-demo")); item.Status != StatusOK {
		t.Errorf("doctor config-reads on a fresh game-demo = %s %q", item.Status, item.Detail)
	}
}

// 新生成的 game-demo，doctor 的 config-schema 检查零 WARN、零 FAIL：game 的业务代码读的 activity.* / platform.*
// 由 game 服务自己声明（game/settings），doctor 编译工程、经 --print-config-schema 读回这份声明。
// 去掉服务的 ConfigSchema 之后，同一行 WARN 回来（守卫能红）。
func TestDoctorReadsBusinessDeclarationsFromTheProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the generated game-demo")
	}
	t.Parallel()
	root := buildableGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	source, cleanup := processConfigSchemas(ctx, root)
	defer cleanup()
	items := checkConfigDeclarations(root, m, source)
	if len(items) < 10 {
		t.Fatalf("config-schema items = %d; the game-demo no longer has its framework services", len(items))
	}
	for _, item := range items {
		if item.Status != StatusOK {
			t.Errorf("%s: %s %s", item.Name, item.Status, item.Detail)
		}
		if item.Name == "config-schema:game" && !strings.Contains(item.Detail, "(4 business key(s))") {
			t.Errorf("config-schema:game = %q; want the four keys game/settings declares beyond the framework's (activity.key_prefix, activity.groups_file, platform.key_prefix, platform.payment_secret)", item.Detail)
		}
	}

	// 变异：服务不再声明它读的键。
	servicePath := filepath.Join(root, "internal", "service", "game", "service.go")
	raw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := regexp.MustCompile(`(?m)^func \(\*Service\) ConfigSchema\(\) app\.ConfigSchema \{ return settings\.Schema\(\) \}\n`).ReplaceAllString(string(raw), "")
	mutated = strings.Replace(mutated, "var _ app.ModConfigSchema = (*Service)(nil)\n", "var _ = settings.Schema\n", 1)
	if mutated == string(raw) {
		t.Fatal("service.go changed shape; update this test")
	}
	if err := os.WriteFile(servicePath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	source, cleanup = processConfigSchemas(ctx, root)
	defer cleanup()
	for _, item := range checkConfigDeclarations(root, m, source) {
		if item.Name != "config-schema:game" {
			continue
		}
		if item.Status != StatusWarn || !strings.Contains(item.Detail, "no mod of game declares activity.groups_file, activity.key_prefix, platform.key_prefix, platform.payment_secret") {
			t.Errorf("without the service's declaration: config-schema:game = %s %q; want the WARN naming the four keys", item.Status, item.Detail)
		}
	}
}

// buildableGameDemo 是 game-demo 夹具的私有副本，roost-core replace 到本仓库并 go mod tidy，可以编译。
func buildableGameDemo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	replace := regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`)
	goMod = replace.ReplaceAll(goMod, nil)
	goMod = append(goMod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "mod", "tidy")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy in the game-demo copy: %v\n%s", err, out)
	}
	return root
}

var modConfigTargets = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
}

func TestAddedConfigSectionsFollowTheFilesLineEndings(t *testing.T) {
	t.Parallel()
	t.Run("crlf files get crlf sections", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		for _, rel := range modConfigTargets {
			rewriteFile(t, root, rel, func(raw []byte) []byte {
				return bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
			})
		}
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		for _, rel := range modConfigTargets {
			got := readProjectBytes(t, root, rel)
			if !bytes.Contains(got, []byte("\r\nredis:\r\n")) {
				t.Errorf("%s: add mod redis did not append a CRLF redis: section:\n%q", rel, tailBytes(got, 200))
			}
			if bareLineFeed.Match(got) {
				t.Errorf("%s: a CRLF file now has LF-only lines after add mod (the appended section used LF):\n%q", rel, tailBytes(got, 200))
			}
		}
	})
	t.Run("lf files stay lf", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		for _, rel := range modConfigTargets {
			got := readProjectBytes(t, root, rel)
			if !bytes.Contains(got, []byte("\nredis:\n")) {
				t.Errorf("%s: add mod redis did not append a redis: section", rel)
			}
			if bytes.Contains(got, []byte("\r")) {
				t.Errorf("%s: an LF file gained a carriage return", rel)
			}
		}
	})
	t.Run("an empty dev config gets lf sections", func(t *testing.T) {
		t.Parallel()
		root := copyOfNewProject(t, "saga")
		rewriteFile(t, root, modConfigTargets[0], func([]byte) []byte { return nil })
		if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
			t.Fatalf("add mod redis: %v", err)
		}
		got := readProjectBytes(t, root, modConfigTargets[0])
		if !bytes.HasPrefix(got, []byte("redis:\n")) || bytes.Contains(got, []byte("\r")) {
			t.Errorf("empty dev config after add mod redis = %q, want an LF redis: section", got)
		}
	})
}

func rewriteFile(t *testing.T, root, rel string, edit func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edit(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectBytes(t *testing.T, root, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func tailBytes(raw []byte, n int) []byte {
	if len(raw) <= n {
		return raw
	}
	return raw[len(raw)-n:]
}

// 生成的 remote_entity 段写出 kit 的快照键（cached_max_staleness、snapshot_interest_per_consumer、
// snapshot_l2_tombstone_wait_*、mirror.shutdown_timeout），开发配置、生产示例、k8s Secret 示例三份一致；
// 生产化只改独占一行的流副本数（不把墓碑 WAIT 的 replicas 一起改成 3）。取值与 core DefaultConfig 的比对、
// 两个 Mod 的 Init 在 generated_contract_test.go 里对生成工程的真实文件做。

func TestGeneratedRemoteEntitySectionCarriesTheSnapshotKeys(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"remote_entity"}, []string{"config"})
	for _, production := range []bool{false, true} {
		config := renderServiceConfig(m, "game", production)
		for _, want := range []string{
			"\n  cached_max_staleness: 30s\n",
			"\n  snapshot_interest_per_consumer: 0\n",
			"\n  snapshot_l2_tombstone_wait_replicas: 1\n",
			"\n  snapshot_l2_tombstone_wait_timeout: 50ms\n",
			"\n    shutdown_timeout: 5s\n",
		} {
			if !strings.Contains(config, want) {
				t.Errorf("production=%v: config lacks %q:\n%s", production, strings.TrimSpace(want), config)
			}
		}
		if production && !strings.Contains(config, "\n  replicas: 3\n") {
			t.Errorf("production config no longer raises the stream replicas:\n%s", config)
		}
	}
}

// The etcd Discovery key is service_prefix + server_type + "/" + sid with no separator of its own
// (etcd/driver/discovery.go registerOnceWithEtcd; the core default is "/service/"). A generated
// service_prefix without the trailing slash registered "/roost/servicesgame/1300" (2026-10-05
// real-process drill, APP-SINGLETON-LOCK §13 第 5 笔).
func TestGeneratedEtcdServicePrefixSeparatesTheServerType(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: []string{"configdata", "etcd"}}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml"} {
		prefix := configLeaves(t, rel, readProjectFile(t, root, rel))["etcd.service_prefix"]
		if prefix == "" {
			t.Fatalf("%s: no etcd.service_prefix", rel)
		}
		key := fmt.Sprintf("%s%s/%d", prefix, "game", 1300)
		if !strings.HasSuffix(prefix, "/") || !strings.Contains(key, "/game/1300") {
			t.Errorf("%s: etcd.service_prefix %q registers the key %q, want the server type as its own path segment", rel, prefix, key)
		}
	}
}

// 所有进程读同一个 time.logic_offset（维护者决定 D-L3）：偏移不同，窗口 id、关窗截止、票据时间就错开一个偏移量。
// roost doctor 检查项目里每个服务的配置（dev、prod example、k8s secret example）是否一致，不一致时点名服务、文件和值；
// 三类文件各是一套部署，分别比较（dev 配了 +24h 测试偏移而 prod example 是 0 是正常的）。

func setLogicOffset(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	if strings.HasPrefix(rel, "deploy/") {
		// The secret example carries the config under stringData.config.yaml, indented four spaces.
		marker := "config.yaml: |\n"
		at := strings.Index(body, marker)
		if at < 0 {
			t.Fatalf("%s: no stringData config.yaml block", rel)
		}
		at += len(marker)
		body = body[:at] + "    time:\n      logic_offset: " + value + "\n" + body[at:]
	} else {
		body += "time:\n  logic_offset: " + value + "\n"
	}
	writeProjectFile(t, root, rel, body)
}

func doctorItems(t *testing.T, root string) (map[string]CheckItem, error) {
	t.Helper()
	var output bytes.Buffer
	err := DoctorWithOptions(context.Background(), root, DoctorOptions{JSONOutput: true}, &output)
	var report DoctorReport
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil {
		t.Fatalf("doctor output is not a JSON report (%v):\n%s", decodeErr, output.String())
	}
	items := map[string]CheckItem{}
	for _, item := range report.Items {
		items[item.Name] = item
	}
	return items, err
}

func TestDoctorNamesTheServicesWhoseLogicOffsetDisagrees(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)

	// dev 里只有 game 前拨一天：game 与 activity / match 等不一致，必须点名。
	setLogicOffset(t, root, "configs/service/config.game.yaml", "24h")
	items, err := doctorItems(t, root)
	item, ok := items["time:logic_offset"]
	if !ok || item.Status != StatusFail {
		t.Fatalf("only the dev game config sets time.logic_offset 24h: doctor says %+v (present=%v), want FAIL", item, ok)
	}
	for _, want := range []string{"game=24h", "activity=0s", "match=0s", "configs/service/config.<service>.yaml"} {
		if !strings.Contains(item.Detail, want) {
			t.Errorf("FAIL line does not name %q: %s", want, item.Detail)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "time:logic_offset") {
		t.Errorf("doctor returned %v, want an error naming time:logic_offset", err)
	}

	// 全部未配置（= 0）：一致。
	if item, ok := mustDoctorItems(t, newGameDemo(t))["time:logic_offset"]; !ok || item.Status != StatusOK {
		t.Fatalf("no service sets time.logic_offset: doctor says %+v (present=%v), want an OK time:logic_offset line", item, ok)
	}

	// 一种部署内全部改成同一个值就一致；另一种部署（prod example）仍是 0 不算不一致。
	for _, service := range sortedServiceNames(mustManifest(t, root)) {
		if service != "game" {
			setLogicOffset(t, root, "configs/service/config."+service+".yaml", "24h")
		}
	}
	items, _ = doctorItems(t, root)
	if item := items["time:logic_offset"]; item.Status != StatusOK {
		t.Fatalf("every dev config sets 24h, the production examples 0: doctor says %s %s, want OK", item.Status, item.Detail)
	}

	// k8s secret example 里一个服务的值写错，点名文件。
	setLogicOffset(t, root, "deploy/k8s/base/secret.match.example.yaml", "1d")
	items, _ = doctorItems(t, root)
	if item := items["time:logic_offset"]; item.Status != StatusFail ||
		!strings.Contains(item.Detail, "deploy/k8s/base/secret.match.example.yaml") || !strings.Contains(item.Detail, `"1d"`) {
		t.Fatalf("secret.match.example.yaml sets logic_offset 1d (not a duration): doctor says %s %s, want FAIL naming the file and value", item.Status, item.Detail)
	}
}

func mustDoctorItems(t *testing.T, root string) map[string]CheckItem {
	t.Helper()
	items, _ := doctorItems(t, root)
	return items
}

func mustManifest(t *testing.T, root string) Manifest {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 手改 roost.yaml 给服务加 Mod（project sync 不补配置段）之后，doctor 只读报出每份配置缺的段，给出可追加的文本；
// 业务有意删掉的段同样报出（WARN，不让 doctor 失败），留空段即可消掉。维护者 2026-10-17 决定 B。
func TestDoctorNamesTheConfigSectionsAManifestAddedModLacks(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	before := mustManifest(t, root)
	raw, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	const listed = "            - configdata\n"
	if strings.Count(string(raw), listed) != 1 {
		t.Fatalf("precondition: %s does not list game's configdata mod once:\n%s", ManifestName, raw)
	}
	writeProjectFile(t, root, ManifestName, strings.Replace(string(raw), listed, listed+"            - nest\n", 1))
	syncProject(t, root)
	dev := "configs/service/config.game.yaml"
	if cfg := readProjectFile(t, root, dev); strings.Contains(cfg, "\nnest:\n") || strings.Contains(cfg, "\nmongo:\n") {
		t.Fatalf("project sync now writes config sections (its behaviour is meant to stay as it was):\n%s", cfg)
	}

	items := mustDoctorItems(t, root)
	var reported []CheckItem
	for _, item := range items {
		if item.Status != StatusOK && strings.Contains(item.Detail, dev) && strings.Contains(item.Detail, "nest:") {
			reported = append(reported, item)
		}
	}
	if len(reported) == 0 {
		var lines []string
		for _, item := range items {
			lines = append(lines, fmt.Sprintf("%s %s %s", item.Status, item.Name, item.Detail))
		}
		t.Fatalf("roost.yaml gives game the nest mod but %s has no nest: / mongo: / nats: / dataengine: block; doctor does not say so:\n%s", dev, strings.Join(lines, "\n"))
	}
	item := items["config-sections:game"]
	if item.Status != StatusWarn {
		t.Fatalf("config-sections:game is %s %s, want WARN", item.Status, item.Detail)
	}
	for _, rel := range gameConfigs {
		if !strings.Contains(item.Detail, rel+" lacks ") {
			t.Errorf("the WARN does not name %s:\n%s", rel, item.Detail)
		}
	}
	for _, want := range []string{"nest:", "dataengine:", "mongo:", "nats:", "redis:", "\nmongo:\n  uri: mongodb://127.0.0.1", "\n    mongo:\n", "CHANGE_ME", "singleton.enabled is not true"} {
		if !strings.Contains(item.Detail, want) {
			t.Errorf("the WARN lacks %q:\n%s", want, item.Detail)
		}
	}
	for _, other := range []string{"config-schema:game", "config:game"} {
		if items[other].Status == StatusFail {
			t.Errorf("%s fails on the hand-edited project: %s", other, items[other].Detail)
		}
	}
	after := mustManifest(t, root)

	// doctor 读而不写；add 追加的就是它报的段：追加之后 config-sections 为 OK。
	if cfg := readProjectFile(t, root, dev); strings.Contains(cfg, "\nnest:\n") {
		t.Fatalf("doctor wrote %s", dev)
	}
	if _, err := appendModConfigSections(root, before, after, "game", io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, item := range checkConfigSections(root, after) {
		if item.Status != StatusOK {
			t.Fatalf("after appending what add writes, %s is %s:\n%s", item.Name, item.Status, item.Detail)
		}
	}

	// 业务删掉一段：照样报出；留空段（按代码默认值）不再报。
	cfg := readProjectFile(t, root, dev)
	var kept strings.Builder
	for _, block := range topLevelConfigBlocks(cfg) {
		if block.key != "nats" {
			kept.WriteString(block.text)
		}
	}
	writeProjectFile(t, root, dev, kept.String())
	items2 := checkConfigSections(root, after)
	if len(items2) != 1 || items2[0].Status != StatusWarn || !strings.Contains(items2[0].Detail, dev+" lacks nats:") || strings.Contains(items2[0].Detail, "prod.example.yaml lacks") {
		t.Fatalf("a deleted nats: block in %s only: doctor says %+v", dev, items2)
	}
	writeProjectFile(t, root, dev, kept.String()+"nats: {}\n")
	if items2 := checkConfigSections(root, after); items2[0].Status != StatusOK {
		t.Fatalf("an empty nats: {} block should silence the check: %+v", items2)
	}
}

// 不误报：每种登记的新生成工程（含 game-demo）与覆盖全部带配置 Mod 的渲染结果，config-sections 都是 OK。
func TestFreshlyGeneratedConfigsHaveEveryModSection(t *testing.T) {
	t.Parallel()
	for fixture := range projectFixtureOptions {
		root := copyOfNewProject(t, fixture)
		for _, item := range checkConfigSections(root, mustManifest(t, root)) {
			if item.Status != StatusOK {
				t.Errorf("%s: %s is %s:\n%s", fixture, item.Name, item.Status, item.Detail)
			}
		}
	}
	m := fullConfigManifest(t)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	for rel, file := range plan {
		if strings.HasPrefix(rel, "configs/service/") || strings.HasPrefix(rel, "deploy/k8s/base/secret.") {
			writeProjectFile(t, full, rel, string(file.Body))
		}
	}
	for _, item := range checkConfigSections(full, m) {
		if item.Status != StatusOK || item.Detail == "no config file" {
			t.Errorf("full-config render: %s is %s: %s", item.Name, item.Status, item.Detail)
		}
	}
}
