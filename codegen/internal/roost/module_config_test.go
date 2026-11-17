package roost

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"github.com/tjbdwanghaibo/roost-core/modules"
)

// 下面几个类型是 moduleConfigKeys 的对照样本：同一份源码，安装器按语法树读 tag，运行时按反射读（configschema.Of），
// 两边得出的键必须一字不差。覆盖配置声明支持的每一种写法：标量与具名标量、time.Duration、[]string、匿名嵌入、
// 带说明与 closed 的段、`_` 结尾的平铺前缀、map 的标量 / 结构体 / 两层元素、enum、required、secret、example。
type configShapesEmbedded struct {
	Flag bool `config:"shape.flag" default:"true" example:"true"`
}

type configShapesInner struct {
	Timeout time.Duration `config:"timeout" default:"3s" min:"1s" help:"超时"`
	Names   []string      `config:"names" example:"a,b"`
}

type configShapeLevel int64

type configShapes struct {
	configShapesEmbedded
	Count  int32             `config:"shape.count" default:"3" min:"1" max:"9" example:"3" help:"计数"`
	Ratio  float64           `config:"shape.ratio" default:"0.5"`
	Mode   string            `config:"shape.mode" default:"a" enum:"a|B" required:"true"`
	Secret string            `config:"shape.secret" secret:"optional"`
	Inner  configShapesInner `config:"shape.inner" help:"一段"`
	Closed struct {
		X int `config:"x"`
	} `config:"shape.closed,closed"`
	Flat    configShapesInner            `config:"shape.flat_"`
	Servers map[string]string            `config:"shape.servers" example:"{}" help:"映射"`
	Nested  map[string]configShapesInner `config:"shape.nested"`
	Deep    map[string]map[string]int64  `config:"shape.deep"`
	Level   configShapeLevel             `config:"shape.level" default:"2"`
	hidden  int
}

// UseConfigShapes 让样本也带一个接收函数（config.use）。
func UseConfigShapes(configShapes) {}

func TestModuleConfigKeysReadFromTheSourceMatchTheRuntimeDeclaration(t *testing.T) {
	source, err := os.ReadFile("module_config_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"module_config_test.go": source, "broken.go": []byte("package roost\nconst x = {{ID:component:X}}\n")}
	got, err := moduleConfigKeys(files, "configShapes", "UseConfigShapes")
	if err != nil {
		t.Fatal(err)
	}
	want := configschema.MustOf(configShapes{}).Keys
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keys read from the source differ from the runtime declaration:\n got %+v\nwant %+v", got, want)
	}
	_ = configShapes{}.hidden
	for label, c := range map[string]struct{ typeName, use, want string }{
		"missing type":   {"noSuchConfig", "", "type noSuchConfig is not declared"},
		"not a struct":   {"configShapeLevel", "", "is not a struct"},
		"missing use":    {"configShapes", "UseNothing", "func UseNothing(configShapes) is not declared"},
		"wrong use type": {"configShapesInner", "UseConfigShapes", "func UseConfigShapes(configShapesInner) is not declared"},
	} {
		if _, err := moduleConfigKeys(files, c.typeName, c.use); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", label, err, c.want)
		}
	}
	for label, c := range map[string]struct{ body, want string }{
		"untagged exported field": {"type C struct{ A int }", "field A has no config tag"},
		"pointer field":           {"type C struct{ A *int `config:\"a\"` }", "unsupported config field type *int"},
		"upper-case key":          {"type C struct{ A int `config:\"A\"` }", "must be a non-empty lowercase dotted name"},
		"int map key":             {"type C struct{ A map[int]string `config:\"a\"` }", "map keys must be strings"},
	} {
		_, err := moduleConfigKeys(map[string][]byte{"c.go": []byte("package c\n" + c.body + "\n")}, "C", "")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", label, err, c.want)
		}
	}
}

// 清单的 config: 写错时，清单加载（module list / plan / add / verify）就报出来，点名是哪一项。
func TestModuleConfigSpecValidationNamesWhatIsWrong(t *testing.T) {
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]struct {
		module string
		config ModuleConfig
		want   string
	}{
		"package outside files":            {"profile", ModuleConfig{Package: "game/components/bag", Type: "Config"}, "is not in files/"},
		"game package on a hosted runtime": {"portal", ModuleConfig{Package: "game/components/profile", Type: "Config"}, "must live where the module's runtime runs"},
		"lower-case type":                  {"profile", ModuleConfig{Package: "game/components/profile", Type: "config"}, "config.type"},
		"lower-case use":                   {"profile", ModuleConfig{Package: "game/components/profile", Type: "Config", Use: "useConfig"}, "config.use"},
		"type not in package":              {"profile", ModuleConfig{Package: "game/components/profile", Type: "Missing"}, "type Missing is not declared"},
	} {
		spec := catalog[c.module]
		config := c.config
		spec.Config = &config
		if _, err := validateModuleConfig(modules.Files, spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", label, err, c.want)
		}
	}
}

// R5 迁移的五个模块：清单声明的配置结构就是它们读的键（缺省值与原来的常量相同，模块场景的数因此不变）。
func TestModuleConfigCatalogDeclaresTheMigratedRules(t *testing.T) {
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for module, want := range map[string]map[string]string{
		"profile":     {"profile.exp_per_level": "100"},
		"stamina":     {"stamina.max": "100", "stamina.hard_cap": "999", "stamina.regen_interval": "5m", "stamina.refill_daily_limit": "3", "stamina.refill_price_step": "50"},
		"leaderboard": {"leaderboard.board_id": "total", "leaderboard.season": "1", "leaderboard.min_score": "1", "leaderboard.max_score": "1000000000000", "leaderboard.default_top": "10", "leaderboard.max_top": "50"},
		"mailbox":     {"mailbox.ledger_prune_grace": "10m"},
		"portal":      {"portal.servers": "", "portal.services": ""},
	} {
		got := map[string]string{}
		for _, key := range catalog[module].ConfigSchema.Keys {
			if key.Kind != configschema.KindSection && !strings.Contains(key.Name, "*") {
				got[key.Name] = key.Default
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("module %s declares %v, want %v", module, got, want)
		}
	}
}

// 在全新的 game 工程上装 profile、stamina、leaderboard、mailbox 与 portal：
//   - 安装器把清单声明里标了 example 的键写进模块所在进程的配置（业务服务 game；portal 的键都没有 example，不写）；
//   - bootstrap 把声明接进 App：game 多一个只带配置的 modulehost Mod，account 上装 portal 入口的 Mod 带上它的声明；
//   - 编译出的进程打印的声明（--print-config-schema）与安装器从模板读出的键一致；
//   - `roost config check` 查得出写错的值：与业务服务自己声明的键一样，由编译出的进程的 --check-config 按声明拒绝
//     （退出码 3，键的范围与模块的跨键规则都在这一步），错误点名键；
//   - 模块运行时读的是配置的值（UseConfig 换上的值），不是常量。
func TestModuleConfigIsWrittenCheckedAndLoaded(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project, compiles it and runs go test in it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "tunables")
	roost("module", "add", "profile", "stamina", "leaderboard", "mailbox")
	roost("module", "add", "portal")
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	// 1. 安装器写进工程配置的段。
	game := read("configs/service/config.game.yaml")
	for _, want := range []string{
		"\nprofile:\n", "  exp_per_level: 100\n",
		"\nstamina:\n", "  max: 100\n", "  hard_cap: 999\n", "  regen_interval: 5m\n", "  refill_daily_limit: 3\n", "  refill_price_step: 50\n",
		"\nleaderboard:\n", "  board_id: total\n", "  season: 1\n", "  min_score: 1\n", "  max_score: 1000000000000\n", "  default_top: 10\n", "  max_top: 50\n",
		"\nmailbox:\n", "  ledger_prune_grace: 10m\n",
	} {
		if !strings.Contains(game, want) {
			t.Fatalf("config.game.yaml lacks %q after installing the modules:\n%s", want, game)
		}
	}
	for _, rel := range []string{"configs/service/config.game.prod.example.yaml", "deploy/k8s/base/secret.game.example.yaml"} {
		if body := read(rel); !strings.Contains(body, "profile:\n") || !strings.Contains(body, "exp_per_level: 100\n") {
			t.Fatalf("%s lacks the profile section", rel)
		}
	}
	if account := read("configs/service/config.account.yaml"); strings.Contains(account, "\nportal:\n") {
		t.Fatalf("config.account.yaml got a portal section although no portal key has an example:\n%s", account)
	}

	// 2. bootstrap 的接线。
	bootstrap := read("internal/bootstrap/generated.go")
	for _, want := range []string{
		`kitmodulehost.NewMod("game", ""`,
		`kitmodulehost.Module{Name: "profile", Config: kitmodulehost.ConfigOf[profileConfig.Config](profileConfig.UseConfig)}`,
		`kitmodulehost.Module{Name: "stamina", Config: kitmodulehost.ConfigOf[staminaConfig.Config](staminaConfig.UseConfig)}`,
		`kitmodulehost.Module{Name: "leaderboard", Config: kitmodulehost.ConfigOf[leaderboardConfig.Config](leaderboardConfig.UseConfig)}`,
		`kitmodulehost.Module{Name: "mailbox", Config: kitmodulehost.ConfigOf[mailboxModule.Config](mailboxModule.UseConfig)}`,
		`Start: portalModule.Start, Config: kitmodulehost.ConfigOf[portalModule.Config](nil)}`,
	} {
		if !strings.Contains(bootstrap, want) {
			t.Fatalf("internal/bootstrap/generated.go lacks %s:\n%s", want, bootstrap)
		}
	}

	// 3. 进程打印的声明与安装器读出的键一致。
	process, cleanup := newProjectProcess(context.Background(), root)
	defer cleanup()
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for service, names := range map[string][]string{"game": {"profile", "stamina", "leaderboard", "mailbox"}, "account": {"portal"}} {
		declared, err := process.schema(service)
		if err != nil {
			t.Fatalf("--print-config-schema of %s: %v", service, err)
		}
		index := map[string]configschema.Key{}
		for _, key := range declared.Keys {
			index[key.Name] = key
		}
		for _, name := range names {
			for _, key := range catalog[name].ConfigSchema.Keys {
				if got, ok := index[key.Name]; !ok || got.Kind != key.Kind || got.Default != key.Default || got.Min != key.Min || got.Max != key.Max {
					t.Fatalf("service %s: the process declares %s as %+v, the installer read %+v", service, key.Name, got, key)
				}
			}
		}
	}

	// 4. roost config check 按声明查出写错的值。
	check := func(service string) error {
		return Run([]string{"config", "check", "--root", root, "--service", service}, &strings.Builder{}, &strings.Builder{})
	}
	if err := check("game"); err != nil {
		t.Fatalf("config check on the fresh project: %v", err)
	}
	if err := check("account"); err != nil {
		t.Fatalf("config check of account on the fresh project: %v", err)
	}
	for _, c := range []struct {
		service, file, from, to string
		code                    int
		want                    string
	}{
		{"game", "config.game.yaml", "  exp_per_level: 100\n", "  exp_per_level: 0\n", configExitCrossModErrors, "profile.exp_per_level must be positive"},
		{"game", "config.game.yaml", "  ledger_prune_grace: 10m\n", "  ledger_prune_grace: 30s\n", configExitCrossModErrors, "mailbox.ledger_prune_grace must be at least 2m"},
		{"game", "config.game.yaml", "  hard_cap: 999\n", "  hard_cap: 50\n", configExitCrossModErrors, "stamina.hard_cap must not be below stamina.max"},
		{"game", "config.game.yaml", "  board_id: total\n", "  board_id: \"no:colons\"\n", configExitCrossModErrors, "leaderboard.board_id"},
		{"game", "config.game.yaml", "  max_top: 50\n", "  max_top: 5\n", configExitCrossModErrors, "leaderboard.default_top must not exceed leaderboard.max_top"},
		{"account", "config.account.yaml", "", "portal:\n  servers:\n    \"007\": 203.0.113.7:7000\n", configExitCrossModErrors, "is not a server id"},
	} {
		path := filepath.Join(root, "configs", "service", c.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mutated := string(raw) + c.to
		if c.from != "" {
			mutated = strings.Replace(string(raw), c.from, c.to, 1)
		}
		if mutated == string(raw) {
			t.Fatalf("%s has no %q", c.file, c.from)
		}
		if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
			t.Fatal(err)
		}
		err = check(c.service)
		if code := configCheckExitCode(err); code != c.code || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("config check with %q in %s = exit %d %v; want exit %d naming %q", strings.TrimSpace(c.to), c.file, code, err, c.code, c.want)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 5. 模块运行时读配置的值。
	scenarios := map[string]string{
		"game/handler/r5_config_runtime_test.go": `package handler

import (
	"testing"

	gameprofile "example.com/studio/tunables/game/components/profile"
)

func TestR5ConfigProfileExpPerLevelComesFromTheConfig(t *testing.T) {
	previous := gameprofile.CurrentConfig()
	gameprofile.UseConfig(gameprofile.Config{ExpPerLevel: 7})
	t.Cleanup(func() { gameprofile.UseConfig(previous) })
	f := newProfileFixture(t)
	f.mustAddExp(t, 15, 2)
	if got := f.facts(t); got.level != 2 || got.exp != 1 {
		t.Fatalf("15 exp at 7 per level: %+v, want level 2 with 1 exp left", got)
	}
}
`,
		"game/components/stamina/r5_config_runtime_test.go": `package stamina

import (
	"testing"
	"time"
)

func TestR5ConfigStaminaRulesComeFromTheConfig(t *testing.T) {
	previous := CurrentConfig()
	UseConfig(Config{Max: 10, HardCap: 20, RegenInterval: 1500 * time.Millisecond, RefillDailyLimit: 1, RefillPriceStep: 7})
	t.Cleanup(func() { UseConfig(previous) })
	component := &Component{}
	if got := component.staminaRules(); got != (StaminaRules{Max: 10, HardCap: 20, RegenMillis: 1500, RefillDailyLimit: 1}) {
		t.Fatalf("rules = %+v", got)
	}
	if price := component.staminaRefillPrice(2); price != 14 {
		t.Fatalf("the second refill costs %d, want 14", price)
	}
}
`,
		"game/modules/leaderboard/r5_config_runtime_test.go": `package leaderboard

import (
	"testing"

	svcrank "github.com/tjbdwanghaibo/roost-core/service/rank"
)

func TestR5ConfigLeaderboardRulesComeFromTheConfig(t *testing.T) {
	previous := CurrentConfig()
	UseConfig(Config{BoardID: "clears", Season: 3, MinScore: 5, MaxScore: 9, DefaultTop: 2, MaxTop: 4})
	t.Cleanup(func() { UseConfig(previous) })
	if got := CurrentBoard(); got != (svcrank.Board{ID: "clears", Scope: svcrank.ScopeGlobal, Season: 3}) {
		t.Fatalf("board = %+v", got)
	}
	if ValidateScore(4) == nil || ValidateScore(5) != nil || ValidateScore(9) != nil || ValidateScore(10) == nil {
		t.Fatal("the score range is not [5, 9]")
	}
	if TopLimit(0) != 2 || TopLimit(3) != 3 || TopLimit(100) != 4 {
		t.Fatalf("top limits = %d %d %d, want 2 3 4", TopLimit(0), TopLimit(3), TopLimit(100))
	}
}
`,
		"game/modules/mailbox/r5_config_runtime_test.go": `package mailbox

import (
	"testing"
	"time"
)

func TestR5ConfigMailboxLedgerGraceComesFromTheConfig(t *testing.T) {
	previous := CurrentConfig()
	UseConfig(Config{LedgerPruneGrace: 2 * time.Minute})
	t.Cleanup(func() { UseConfig(previous) })
	const expires = 1_800_000_060
	if LedgerMayForget(expires, expires+119) || !LedgerMayForget(expires, expires+120) {
		t.Fatal("the ledger does not forget exactly 120s after the expiry")
	}
}
`,
	}
	for rel, body := range scenarios {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := runGeneratedGo(t, root, "test", "-count=1", "-v", "-run", "^TestR5Config", "./game/handler", "./game/components/stamina", "./game/modules/leaderboard", "./game/modules/mailbox")
	if got := strings.Count(out, "--- PASS: TestR5Config"); got != 4 {
		t.Fatalf("%d of 4 runtime config scenarios passed:\n%s", got, out)
	}
}
