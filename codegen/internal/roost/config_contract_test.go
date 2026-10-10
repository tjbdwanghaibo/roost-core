package roost

import (
	"bytes"
	"context"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
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
