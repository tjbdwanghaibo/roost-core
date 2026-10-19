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
// doctor 的 config-check 项把每个服务的开发配置交给编译后的进程 --check-config：跨键与跨 Mod 规则（例如 saga 的效果流
// 必须就是同进程 DataEngine 的那条）只在进程里，键快照查不到。新生成的 game-demo 全部通过；把 saga 的效果流写成与
// dataengine.effects 不同之后，game 一项 FAIL 并点名两个键（修前 doctor 全部通过，启动才报 "stream not found"）；把效果流的
// 保留期写得比 saga 完成回执长，同样 FAIL（D12 顺带项，修前这条在 saga Init 里，doctor 放过）。
func TestDoctorChecksCrossModConfigRulesThroughTheProcess(t *testing.T) {
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
	process, cleanup := newProjectProcess(ctx, root)
	defer cleanup()
	items := checkConfigRules(root, m, process)
	if len(items) < 10 {
		t.Fatalf("config-check items = %d; the game-demo no longer has its framework services", len(items))
	}
	for _, item := range items {
		if item.Status != StatusOK {
			t.Errorf("fresh game-demo %s: %s %s", item.Name, item.Status, item.Detail)
		}
	}
	configPath := filepath.Join(root, "configs", "service", "config.game.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(raw), "\nsaga:\n", "\nsaga:\n  start_effect_stream: ROOST_EFFECTS\n", 1)
	if mutated == string(raw) {
		t.Fatal("config.game.yaml has no saga: section; update this test")
	}
	if err := os.WriteFile(configPath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, item := range checkConfigRules(root, m, process) {
		if item.Name != "config-check:game" {
			continue
		}
		if item.Status != StatusFail || !strings.Contains(item.Detail, "saga.start_effect_stream") || !strings.Contains(item.Detail, "dataengine.effects.stream") {
			t.Errorf("mismatched saga effect stream: config-check:game = %s %q; want a FAIL naming both keys", item.Status, item.Detail)
		}
	}
	// 效果流比完成回执活得久（D12 顺带项）：saga 的完成回执 TTL 与 dataengine.effects.max_age 的比较是跨 Mod 规则，
	// 现在由 saga Mod 的 CheckConfigAcrossMods 做，doctor 经进程的 --check-config 报出；修前它在 saga Init 里，doctor 全部通过。
	retention := strings.Replace(string(raw), "\n    max_age: 168h\n", "\n    max_age: 800h\n", 1)
	if retention == string(raw) {
		t.Fatal("config.game.yaml has no dataengine.effects max_age: 168h; update this test")
	}
	if err := os.WriteFile(configPath, []byte(retention), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, item := range checkConfigRules(root, m, process) {
		if item.Name != "config-check:game" {
			continue
		}
		if item.Status != StatusFail || !strings.Contains(item.Detail, "saga.completion_receipt_ttl") || !strings.Contains(item.Detail, "dataengine.effects.max_age") {
			t.Errorf("effect stream outliving the completion receipts: config-check:game = %s %q; want a FAIL naming both keys", item.Status, item.Detail)
		}
	}
}

// TestConfigCheckRunsCrossModRulesThroughTheProcess：`roost config check` 与进程 --check-config、doctor config-check
// 对同一份配置给同一个结论。跨键与跨 Mod 规则（app.ModConfigCrossChecker，例如 saga 的效果流必须就是同进程 DataEngine
// 的那条）只在编译后的进程里，config check 经同一个 projectProcess.checkConfig 运行它，不复制规则。
func TestConfigCheckRunsCrossModRulesThroughTheProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the generated game-demo")
	}
	t.Parallel()
	root := buildableGameDemo(t)
	var out, errOut bytes.Buffer
	if err := Run([]string{"config", "check", "--root", root, "--service", "game"}, &out, &errOut); err != nil {
		t.Fatalf("fresh game-demo config check: %v\n%s%s", err, out.String(), errOut.String())
	}
	configPath := filepath.Join(root, "configs", "service", "config.game.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(raw), "\nsaga:\n", "\nsaga:\n  start_effect_stream: ROOST_EFFECTS\n", 1)
	if mutated == string(raw) {
		t.Fatal("config.game.yaml has no saga: section; update this test")
	}
	if err := os.WriteFile(configPath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	err = Run([]string{"config", "check", "--root", root, "--service", "game"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "saga.start_effect_stream") || !strings.Contains(err.Error(), "dataengine.effects.stream") {
		t.Fatalf("mismatched saga effect stream: config check = %v (stdout %q); want an error naming both keys, like --check-config and doctor", err, out.String())
	}
	if code := configCheckExitCode(err); code != configExitCrossModErrors {
		t.Errorf("cross-mod rule failure: exit %d, want %d", code, configExitCrossModErrors)
	}
	if err := os.WriteFile(configPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// 新生成的 game-demo（十个服务）整体检查：键、跨 Mod 规则、跨服务一致性全部通过，没有误报，退出码 0。
	out.Reset()
	if err := Run([]string{"config", "check", "--root", root, "--all", "--json"}, &out, &errOut); err != nil {
		t.Fatalf("fresh game-demo config check --all: %v\n%s", err, out.String())
	}
	var report configCheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.ExitCode != 0 || !report.CrossService.Checked || report.CrossService.Services < 10 {
		t.Errorf("fresh game-demo --all --json = %+v", report)
	}
	for _, f := range report.Files {
		if f.Keys != checkOK || f.CrossMod != checkOK {
			t.Errorf("fresh game-demo %s: %+v", f.Path, f)
		}
	}
}

func buildableGameDemo(t *testing.T) string {
	t.Helper()
	root := copyOfNewProject(t, "game-demo")
	useLocalRoostCore(t, root)
	return root
}

// useLocalRoostCore 让生成工程编译当前工作树的 roost-core（replace 到仓库根）并 go mod tidy。
func useLocalRoostCore(t *testing.T, root string) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("go mod tidy in the generated project copy: %v\n%s", err, out)
	}
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

// 生成配置里决定“与谁共享”的名字（总线主题前缀、Data Engine 库、effects / saga / sync 的主题与流、saga 与 Remote 的库、
// etcd 服务注册前缀）按工程名派生：两个工程共用一套开发用 Mongo / NATS / etcd 时不撞库撞流。库名与 DAO 骨架同一规则
// （daoDatabaseName）。新建时写进的段与之后 roost add mod 追加的段派生规则相同，saga 读的 effects 流与 Data Engine
// 写的那条一致：saga 不另写效果流，取 dataengine.effects 的值。
func TestGeneratedSharedResourceNamesFollowTheProject(t *testing.T) {
	t.Parallel()
	want := func(project string) map[string]string {
		upper, db := strings.ToUpper(project), daoDatabaseName(project)
		return map[string]string{
			"nats.prefix":                       project,
			"syncbus.prefix":                    project + ".sync",
			"dataengine.database":               db,
			"dataengine.effects.subject_prefix": project + ".effect",
			"dataengine.effects.stream":         upper + "_EFFECTS",
			"saga.database":                     db + "_saga",
			"saga.subject_prefix":               project + ".saga",
			"saga.stream":                       upper + "_SAGA",
			"remote_entity.mongo.database":      db + "_remote",
			"etcd.service_prefix":               "/" + project + "/services/",
		}
	}
	mods := []string{"configdata", "etcd", "syncbus", "nest", "saga", "remote_entity"}
	for _, tc := range []struct {
		project string
		later   bool // 先建只有 configdata 的工程，再逐个 roost add mod
	}{{"alpha", false}, {"beta", true}} {
		root := filepath.Join(t.TempDir(), tc.project)
		initial := mods
		if tc.later {
			initial = []string{"configdata"}
		}
		if _, _, err := NewProject(NewOptions{Name: tc.project, Module: "example.com/" + tc.project, Out: root, Mods: initial}); err != nil {
			t.Fatal(err)
		}
		if tc.later {
			for _, mod := range mods[1:] {
				if _, err := Add(root, AddOptions{Kind: "mod", Name: mod, Service: "game", Warnings: io.Discard}); err != nil {
					t.Fatalf("add mod %s: %v", mod, err)
				}
			}
		}
		for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml"} {
			body := readProjectFile(t, root, rel)
			leaves := configLeaves(t, rel, body)
			for key, value := range want(tc.project) {
				if leaves[key] != value {
					t.Errorf("%s %s: %s = %q, want %q (derived from the project name)", tc.project, rel, key, leaves[key], value)
				}
			}
			// saga 的效果流不另写：不写时取 dataengine.effects（上面已按工程名派生），只在一处配置。
			for _, key := range []string{"saga.start_effect_stream", "saga.start_effect_prefix"} {
				if value, written := leaves[key]; written {
					t.Errorf("%s %s: %s = %q is written again; the saga follows dataengine.effects when it is unset", tc.project, rel, key, value)
				}
			}
			if strings.Contains(body, "{project") || strings.Contains(body, "{PROJECT") {
				t.Errorf("%s %s: an unreplaced name placeholder is left:\n%s", tc.project, rel, body)
			}
		}
	}
}

// 同一工程各服务之间的配置一致性（维护者 2026-10-19 批准）：跨进程共享的资源名（声明里带 shared 的键，例如
// nats.prefix）在各服务必须相同；开发配置集里的服务在同一台机器上一起跑，监听地址（声明里带 listen 的键，例如
// ops.addr）的端口不得冲突。单个进程看不到别的服务，规则只能在这里查：config check --all 与 doctor 的
// config-across-services 用同一个实现（checkConfigAcrossServices）。修前两种改法都“通过”。
func TestConfigCheckAllComparesSharedNamesAndPortsAcrossServices(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata", "nats"}})
	if err != nil {
		t.Fatal(err)
	}
	gatePath := filepath.Join(root, "configs", "service", "config.gate.yaml")
	pristine, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"config", "check", "--root", root, "--all"}, io.Discard, io.Discard); err != nil && strings.Contains(err.Error(), "cross-service") {
		t.Fatalf("fresh two-service project: %v", err)
	}
	for _, tc := range []struct {
		name, from, to string
		want           []string
	}{
		{"nats prefix", "\n  prefix: planet\n", "\n  prefix: other\n", []string{"nats.prefix", "game", "gate", `"planet"`, `"other"`}},
		{"ops port", "\n  addr: 127.0.0.1:9101\n", "\n  addr: 0.0.0.0:9100\n", []string{"ops.addr", "game", "gate", "9100"}},
	} {
		mutated := strings.Replace(string(pristine), tc.from, tc.to, 1)
		if mutated == string(pristine) {
			t.Fatalf("%s: config.gate.yaml has no %q; update this test", tc.name, tc.from)
		}
		if err := os.WriteFile(gatePath, []byte(mutated), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Run([]string{"config", "check", "--root", root, "--all"}, io.Discard, io.Discard)
		for _, want := range tc.want {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: config check --all = %v; want an error naming %s", tc.name, err, want)
				break
			}
		}
		var report bytes.Buffer
		_ = DoctorWithOptions(context.Background(), root, DoctorOptions{JSONOutput: true}, &report) // 夹具编译不过，doctor 本身失败
		var decoded DoctorReport
		if err := json.Unmarshal(report.Bytes(), &decoded); err != nil {
			t.Fatalf("%s: doctor JSON: %v\n%s", tc.name, err, report.String())
		}
		found := false
		for _, item := range decoded.Items {
			if item.Name == "config-across-services" {
				found = true
				if item.Status != StatusFail || !strings.Contains(item.Detail, tc.want[0]) {
					t.Errorf("%s: doctor config-across-services = %s %q; want a FAIL naming %s", tc.name, item.Status, item.Detail, tc.want[0])
				}
			}
		}
		if !found {
			t.Errorf("%s: doctor has no config-across-services item", tc.name)
		}
	}
}

// --keys-only 只做键检查：不编译工程、不跑跨 Mod 规则，并在输出里写明没查；--json 给出每份文件三步的结论与退出码。
// 编译不过的工程上，完整检查以 5（检查没有跑全）结束，--keys-only 以 0 结束；键错了两种都是 2。
func TestConfigCheckKeysOnlyAndJSONReport(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata", "nats"}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run([]string{"config", "check", "--root", root, "--all", "--keys-only"}, &out, io.Discard); err != nil {
		t.Fatalf("--keys-only on a valid project = %v (exit %d)\n%s", err, configCheckExitCode(err), out.String())
	}
	if !strings.Contains(out.String(), "cross-mod rules not checked (--keys-only)") || strings.Contains(out.String(), "does not compile") {
		t.Errorf("--keys-only output must say the cross-mod rules were not checked and must not compile the project:\n%s", out.String())
	}

	decode := func(args ...string) (configCheckReport, error) {
		t.Helper()
		var buf bytes.Buffer
		err := Run(append([]string{"config", "check", "--root", root, "--json"}, args...), &buf, io.Discard)
		var report configCheckReport
		if decodeErr := json.Unmarshal(buf.Bytes(), &report); decodeErr != nil {
			t.Fatalf("%v --json: stdout is not one JSON report: %v\n%s", args, decodeErr, buf.String())
		}
		return report, err
	}
	report, err := decode("--all")
	if code := configCheckExitCode(err); code != configExitNotFullyChecked || report.ExitCode != code || report.Status != "not_fully_checked" {
		t.Fatalf("uncompilable project --all --json: exit %d, report %+v; want exit %d not_fully_checked", code, report, configExitNotFullyChecked)
	}
	if len(report.Files) != 2 || !report.CrossService.Checked || len(report.CrossService.Errors) != 0 {
		t.Errorf("report files / cross_service = %+v / %+v", report.Files, report.CrossService)
	}
	for _, f := range report.Files {
		if f.Keys != checkOK || f.CrossMod != checkNotChecked || !strings.HasPrefix(f.CrossModReason, "project_does_not_compile") {
			t.Errorf("%s: %+v; want keys ok, cross_mod not_checked because the project does not compile", f.Path, f)
		}
	}
	report, err = decode("--service", "game", "--keys-only")
	if err != nil || report.Status != "ok" || report.Files[0].CrossModReason != "keys_only" || report.CrossService.Checked {
		t.Errorf("--service game --keys-only --json = %v, %+v; want ok, cross_mod keys_only, cross_service not checked", err, report)
	}

	gamePath := filepath.Join(root, "configs", "service", "config.game.yaml")
	raw, err := os.ReadFile(gamePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gamePath, []byte(strings.Replace(string(raw), "\nnats:\n", "\nnats:\n  urll: nats://x\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = decode("--all", "--keys-only")
	if code := configCheckExitCode(err); code != configExitKeyErrors || report.Status != "key_errors" || report.Files[0].Keys != checkFailed || !strings.Contains(strings.Join(report.Files[0].KeyErrors, " "), "nats.urll") {
		t.Errorf("misspelled key --keys-only --json: exit %d, %+v; want exit %d with nats.urll in game's key_errors", code, report.Files, configExitKeyErrors)
	}
}

// etcd.advertise_addr 是写进服务注册、别的进程据此连接本服务的地址（维护者 2026-10-19 决定）：同一台机器上一起跑的
// 服务（开发配置集）必须各不相同。生成器按服务序号给每个带 etcd 的服务不同的端口（与 ops 端口同一套序号，9000 + 序号），
// 新建工程与之后 add mod 追加的段一样；两个服务写成同一个地址时 config check --all（退出码 4）与 doctor
// config-across-services 报出（声明上的 unique tag，规则只在声明里一处）。修前两个服务都是 127.0.0.1:9000，检查通过。
func TestEtcdAdvertiseAddrDiffersPerServiceAndIsChecked(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata", "etcd"}})
	if err != nil {
		t.Fatal(err)
	}
	advertise := func(service, suffix string) string {
		leaves := configLeaves(t, service, readProjectFile(t, root, "configs/service/config."+service+suffix+".yaml"))
		return leaves["etcd.advertise_addr"]
	}
	if game, gate := advertise("game", ""), advertise("gate", ""); game == "" || game == gate {
		t.Errorf("generated etcd.advertise_addr: game %q, gate %q; want two different addresses", game, gate)
	} else if game != "127.0.0.1:9000" || gate != "127.0.0.1:9001" {
		t.Errorf("generated etcd.advertise_addr: game %q, gate %q; want 127.0.0.1:9000 / 127.0.0.1:9001 (9000 + the ops port's service index)", game, gate)
	}
	if prod := advertise("gate", ".prod.example"); prod != "CHANGE_ME:9000" {
		t.Errorf("production example etcd.advertise_addr = %q; want the CHANGE_ME:9000 placeholder (one container per service)", prod)
	}
	// 之后 add mod etcd 追加的段走同一规则。
	later := filepath.Join(t.TempDir(), "comet")
	_, laterRoot, err := NewProject(NewOptions{Name: "comet", Module: "example.com/comet", Out: later, Services: []string{"game", "gate"}, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(laterRoot, AddOptions{Kind: "mod", Name: "etcd", Service: "gate", Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if got := configLeaves(t, "gate", readProjectFile(t, laterRoot, "configs/service/config.gate.yaml"))["etcd.advertise_addr"]; got != "127.0.0.1:9001" {
		t.Errorf("add mod etcd -service gate: etcd.advertise_addr = %q, want 127.0.0.1:9001", got)
	}

	gatePath := filepath.Join(root, "configs", "service", "config.gate.yaml")
	raw, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	same := regexp.MustCompile(`(?m)^(  advertise_addr: ).*$`).ReplaceAllString(string(raw), "${1}"+advertise("game", ""))
	if err := os.WriteFile(gatePath, []byte(same), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Run([]string{"config", "check", "--root", root, "--all", "--keys-only"}, io.Discard, io.Discard)
	if code := configCheckExitCode(err); code != configExitCrossServiceErrors || !strings.Contains(err.Error(), "etcd.advertise_addr") {
		t.Errorf("two services advertising one address: config check --all = %v (exit %d); want exit %d naming etcd.advertise_addr", err, code, configExitCrossServiceErrors)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if item := checkConfigAcrossServicesItem(root, m); item.Status != StatusFail || !strings.Contains(item.Detail, "etcd.advertise_addr") {
		t.Errorf("doctor config-across-services = %s %q; want a FAIL naming etcd.advertise_addr", item.Status, item.Detail)
	}
}

// 按服务区分的开发端口来自 roost.yaml 里每个服务一经分配就不变的端口块号（services.<name>.port_block，维护者
// 2026-10-19 提议）：ops 9100+N、etcd.advertise_addr 9000+N。之后 add 一个名字排在中间的服务，已有服务的块号与
// deploy/dev/run.sh 里的端口不变，新服务拿“当前最大块号 + 1”。修前端口按服务名排序现算：新服务排到最前，拿走
// game 的 9100，run.sh 里 game / gate 的端口整体后移，与它们的配置文件不再一致。
func TestPortBlocksAreStableAcrossAddService(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata", "etcd"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "service", Name: "field", Mods: []string{"configdata", "etcd"}, Warnings: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for service, want := range map[string][2]string{
		"game":  {"127.0.0.1:9100", "127.0.0.1:9000"},
		"gate":  {"127.0.0.1:9101", "127.0.0.1:9001"},
		"field": {"127.0.0.1:9102", "127.0.0.1:9002"},
	} {
		leaves := configLeaves(t, service, readProjectFile(t, root, "configs/service/config."+service+".yaml"))
		if leaves["ops.addr"] != want[0] || leaves["etcd.advertise_addr"] != want[1] {
			t.Errorf("%s: ops.addr %q, etcd.advertise_addr %q; want %s / %s", service, leaves["ops.addr"], leaves["etcd.advertise_addr"], want[0], want[1])
		}
		if port := opsPortOf(t, root, service); fmt.Sprintf("127.0.0.1:%d", port) != want[0] {
			t.Errorf("deploy/dev/run.sh: %s on ops port %d; want %s (the port in its config)", service, port, want[0])
		}
	}
	manifest := readProjectFile(t, root, ManifestName)
	for _, want := range []string{"port_block: 0", "port_block: 1", "port_block: 2"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("roost.yaml does not record %q:\n%s", want, manifest)
		}
	}
	if err := Run([]string{"config", "check", "--root", root, "--all", "--keys-only"}, io.Discard, io.Discard); err != nil {
		t.Errorf("after add service: config check --all = %v", err)
	}
}

// roost.yaml 的端口块号是生成器管的一等字段：缺了（手改 roost.yaml 加服务）普通命令报错并给出改法，project sync 分配
// “当前最大块号 + 1”并写回；块号重复或超出 0-99 校验失败；配置里的端口与块号派生值不一致只 WARN（配置归应用）。
// 不从旧配置推断（项目未上线，维护者 2026-10-19 决定不做旧工程兼容）。
func TestPortBlockIsARequiredManagedManifestField(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata", "etcd"}})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	// 手改 roost.yaml 加一个服务（没有块号）。
	handAdded := strings.Replace(string(raw), "services:\n", "services:\n    alpha:\n        mods:\n            - configdata\n", 1)
	if handAdded == string(raw) {
		t.Fatal("roost.yaml has no services: block; update this test")
	}
	if err := os.WriteFile(manifestPath, []byte(handAdded), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(root); err == nil || !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "roost project sync") || !strings.Contains(err.Error(), "services.alpha.port_block: 2") {
		t.Fatalf("roost.yaml without alpha's port_block: LoadManifest = %v; want an error naming alpha, roost project sync and the next free block 2", err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatalf("project sync: %v", err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatalf("after sync: %v", err)
	}
	for service, want := range map[string]int{"game": 0, "gate": 1, "alpha": 2} {
		if got := m.Services[service].PortBlock; got == nil || *got != want {
			t.Errorf("after sync: services.%s.port_block = %v, want %d (existing blocks unchanged, the new service takes max+1)", service, got, want)
		}
	}
	if got := opsPortOf(t, root, "alpha"); got != 9102 {
		t.Errorf("deploy/dev/run.sh: alpha on ops port %d, want 9102", got)
	}

	for _, tc := range []struct {
		name  string
		block int
		want  string
	}{{"duplicate", 0, "also services.game"}, {"out of range", 100, "outside 0-99"}, {"negative", -1, "outside 0-99"}} {
		bad := m.withPortBlocks()
		services := map[string]ServiceSpec{}
		for name, spec := range bad.Services {
			services[name] = spec
		}
		gate := services["gate"]
		block := tc.block
		gate.PortBlock = &block
		services["gate"] = gate
		bad.Services = services
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s port_block: Validate = %v, want %q", tc.name, err, tc.want)
		}
	}

	// 四段按服务端口互不重叠：块号 0-99 时 ops / advertise / 第二个 game 的两个端口各占一段。
	seen := map[int]string{}
	for block := 0; block <= maxPortBlock; block++ {
		p := portsFor(block)
		for _, port := range []int{p.Ops, p.Advertise, p.SecondOps, p.SecondAdvertise} {
			if owner, taken := seen[port]; taken {
				t.Fatalf("port %d of block %d collides with %s", port, block, owner)
			}
			seen[port] = fmt.Sprintf("block %d", block)
		}
	}

	// 配置里手改了 ops 端口：与块号不一致只提示，不影响退出码。
	gatePath := filepath.Join(root, "configs", "service", "config.gate.yaml")
	gateRaw, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatePath, []byte(strings.Replace(string(gateRaw), "addr: 127.0.0.1:9101", "addr: 127.0.0.1:9150", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run([]string{"config", "check", "--root", root, "--all", "--keys-only"}, &out, io.Discard); err != nil {
		t.Fatalf("hand-edited ops port: config check = %v; a mismatch with the block is a warning, not a failure", err)
	}
	if !strings.Contains(out.String(), "cross-service warnings") || !strings.Contains(out.String(), "port_block 1") {
		t.Errorf("hand-edited ops port: output lacks the port block warning:\n%s", out.String())
	}
	if item := checkConfigAcrossServicesItem(root, m); item.Status != StatusWarn || !strings.Contains(item.Detail, "9150") {
		t.Errorf("doctor config-across-services = %s %q; want a WARN naming 9150", item.Status, item.Detail)
	}
}
