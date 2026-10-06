package roost

// 维护者决定 A4 ①：生成器写的配置段从 Mod 的配置声明来，生成的配置与声明不一致时这里变红。
//
// 旧形态：catalog.go 的 Config 字符串、framework_services.go 的 ConfigFunc、player_tcp_config.go 的缺省表都是手写的
// 键名与示例值，与 kit Mod 实际读的键没有任何东西对照。A8、A4 两批都出过“模板缺键”；这次接上声明时还发现生成的
// shutdown 段一直写着 serve_wait_timeout——没有任何代码读它（RR-20261006-38）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// 快照与 kit 当前的声明一致：kit Mod 改了声明而没有 go generate，这里变红。
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
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "run", "../../../kit/internal/configschemagen", "-out", "kitconfig_gen.go", "-check")
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
