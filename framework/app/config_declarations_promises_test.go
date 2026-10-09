package app

// 维护者决定 A4 ①（docs/feature/A4-1-MOD-CONFIG-SCHEMA-2026-10-07.md）：每个 Mod 声明自己的配置键，校验、生成器、
// doctor 共用这一份声明。这里守住“声明与读取一致”的两半：
//
//   - 读了没声明：框架代码（app、kit 与生成进工程的两份模板）读配置只经 LoadConfig；直接调 viper 的读方法、
//     app.ConfigBool 这类单键读取，读到的键不在任何声明里，App 启动检查、生成器与 doctor 都看不到它。
//   - 声明了没读：带 config tag 的字段必须在本包代码里被读。声明了不读的键会被生成器写进配置、被 doctor 当成合法，
//     运维改了它却什么也不发生。
//
// 旧形态（A4 ②，3e3350d5）：框架键登记在 frameworkBoolKeys / frameworkDurationKeys / frameworkIntKeys 三张手写清单里，
// 守卫用正则扫读取点核对清单；生成器的配置段、doctor 各写各的，新键漏进任何一处都没有东西报错。

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

func TestConfigIntAcceptsWholeNumbersOnly(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
		ok   bool
	}{
		{"8", 8, true}, {"\"8\"", 8, true}, {"-3", -3, true}, {"1e3", 1000, true}, {"0", 0, true},
		{"1.5", 0, false}, {"8k", 0, false}, {"10s", 0, false}, {"true", 0, false}, {"[1]", 0, false},
	} {
		cfg := yamlConfig(t, "n: "+tc.body+"\n")
		got, err := ConfigInt64(cfg, "n")
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("n: %s: ConfigInt64 = %d, %v; want %d", tc.body, got, err, tc.want)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "n must be a whole number")) {
			t.Errorf("n: %s: ConfigInt64 = %d, %v; want an error naming n", tc.body, got, err)
		}
	}
	if got, err := ConfigInt64(yamlConfig(t, ""), "missing"); got != 0 || err != nil {
		t.Errorf("unset key: ConfigInt64 = %d, %v; want 0, nil", got, err)
	}
}

// --- App 启动检查：本服务全部 Mod 的声明一起检查 ---

type declaredMod struct {
	name   ModName
	schema ConfigSchema
}

func (m declaredMod) Name() ModName              { return m.name }
func (m declaredMod) Init(*viper.Viper) error    { return nil }
func (m declaredMod) Provide(*Registry) error    { return nil }
func (m declaredMod) Start() error               { return nil }
func (m declaredMod) Stop()                      {}
func (m declaredMod) ConfigSchema() ConfigSchema { return m.schema }

type shopConfigForTest struct {
	MaxItems int    `config:"shop.max_items" default:"100" min:"1" max:"10000"`
	Region   string `config:"shop.region" enum:"cn|us|eu" required:"true"`
}

type bankConfigForTest struct {
	Retries int `config:"bank.retries" min:"0"`
}

type conflictingShopConfigForTest struct {
	MaxItems int `config:"shop.max_items" default:"50"`
}

func TestCheckConfigReportsEveryModsErrorsAtOnce(t *testing.T) {
	cfg := yamlConfig(t, "shop:\n  max_items: 20000\n  region: mars\nbank:\n  retries: -1\nlog:\n  json: on\n")
	err := CheckConfig(cfg,
		declaredMod{name: "shop", schema: SchemaOf(shopConfigForTest{})},
		declaredMod{name: "bank", schema: SchemaOf(bankConfigForTest{})})
	for _, want := range []string{
		"shop.max_items must be at most 10000", "shop.region must be one of cn, us, eu", "bank.retries must not be negative", "log.json must be true or false",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckConfig = %v\nwant it to contain %q", err, want)
		}
	}
	if err := CheckConfig(yamlConfig(t, "shop:\n  region: CN\n"), declaredMod{name: "shop", schema: SchemaOf(shopConfigForTest{})}); err != nil {
		t.Errorf("valid config: CheckConfig = %v", err)
	}
	conflict := CheckConfig(yamlConfig(t, "shop:\n  region: cn\n"),
		declaredMod{name: "shop", schema: SchemaOf(shopConfigForTest{})},
		declaredMod{name: "shop2", schema: SchemaOf(conflictingShopConfigForTest{})})
	if conflict == nil || !strings.Contains(conflict.Error(), "declared differently by two mods: shop.max_items") {
		t.Errorf("two mods declaring shop.max_items differently: CheckConfig = %v", conflict)
	}
}

func TestLoadConfigFillsDefaultsAndRefusesOutOfRangeValues(t *testing.T) {
	var shop shopConfigForTest
	if err := LoadConfig(yamlConfig(t, "shop:\n  region: eu\n"), &shop); err != nil || shop.MaxItems != 100 || shop.Region != "eu" {
		t.Fatalf("LoadConfig = %+v, %v; want the default 100 and region eu", shop, err)
	}
	if err := LoadConfig(yamlConfig(t, "shop:\n  region: eu\n  max_items: 0\n"), &shop); err == nil || !strings.Contains(err.Error(), "shop.max_items must be positive") {
		t.Fatalf("max_items: 0: LoadConfig = %v; want a refusal (0 is outside the declared range, leave the key out for the default)", err)
	}
}

// --- 业务服务的声明（A4 ① 收尾，2026-10-07） ---

// 业务服务（RegisterServer 注册的 Service）与 Mod 一样实现 ConfigSchema：App 启动检查、CheckServiceConfig、
// ServiceConfigSchema（--print-config）与 --print-config-schema 都带上它。修前只看 Mod：game-demo 的业务代码读的
// activity.* / platform.* 没有任何声明，doctor 对 game 报 WARN，写错的值要到业务代码第一次读才暴露。
type declaredService struct{ schema ConfigSchema }

func (declaredService) Name() ServiceName               { return "shop" }
func (declaredService) Init(*Registry) error            { return nil }
func (declaredService) Serve(ctx context.Context) error { <-ctx.Done(); return nil }
func (declaredService) Shutdown(context.Context) error  { return nil }
func (s declaredService) ConfigSchema() ConfigSchema    { return s.schema }

func TestServiceDeclarationsAreCheckedAndPrintedWithTheMods(t *testing.T) {
	a := New("planet", "test").RegisterServer("shop", declaredService{schema: SchemaOf(shopConfigForTest{})},
		declaredMod{name: "bank", schema: SchemaOf(bankConfigForTest{})})
	err := a.CheckServiceConfig("shop", yamlConfig(t, "shop:\n  max_items: 20000\n  region: mars\nbank:\n  retries: -1\n"))
	for _, want := range []string{"service shop: config: shop.max_items must be at most 10000", "service shop: config: shop.region must be one of cn, us, eu", "mod bank: config: bank.retries must not be negative"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckServiceConfig = %v\nwant it to contain %q", err, want)
		}
	}
	if err := a.CheckServiceConfig("shop", yamlConfig(t, "shop:\n  region: cn\n")); err != nil {
		t.Errorf("valid config: CheckServiceConfig = %v", err)
	}
	var out bytes.Buffer
	if err := a.printConfigSchema("shop", &out); err != nil {
		t.Fatal(err)
	}
	var keys []configschema.Key
	if err := json.Unmarshal(out.Bytes(), &keys); err != nil {
		t.Fatalf("--print-config-schema output is not a key list: %v\n%s", err, out.String())
	}
	names := map[string]bool{}
	for _, key := range keys {
		names[key.Name] = true
	}
	for _, want := range []string{"shop.region", "shop.max_items", "bank.retries", "sid", "log.level"} {
		if !names[want] {
			t.Errorf("--print-config-schema lacks %s: %v", want, keys)
		}
	}
	if at := slices.IndexFunc(keys, func(k configschema.Key) bool { return k.Name == "shop.region" }); at >= 0 && (!keys[at].Required || !slices.Equal(keys[at].Enum, []string{"cn", "us", "eu"})) {
		t.Errorf("shop.region printed as %+v; want the declaration (required, enum cn|us|eu)", keys[at])
	}
}

// --- 守卫：读了没声明 / 声明了没读（实现在 internal/configschema/guard.go，codegen 对生成工程用同一份） ---

// declarationLoaderFiles 是唯一允许直接读 viper 的地方：声明读取的 viper 适配器与单键读取本身。
var declarationLoaderFiles = map[string]bool{"config_schema.go": true, "config_values.go": true}

// generatedModTemplates 是生成进工程、读配置的 Mod 模板（player TCP 接入层、RPC 客户端 Mod）。它们是字符串里的
// Go 源码，按文本检查。
var generatedModTemplates = []string{"../../codegen/internal/roost/render_player_tcp.go", "../../codegen/internal/servicerpc/template.go"}

var templateConfigRead = regexp.MustCompile(`\b(?:cfg|config)\.(?:Get\w*|IsSet|AllKeys|UnmarshalKey|Sub)\(|\bapp\.(?:Config(?:Bool|Duration|Int|Int64)|NewConfigReader)\(`)

func TestFrameworkModsReadConfigOnlyThroughDeclarations(t *testing.T) {
	violations := undeclaredConfigReads(t, frameworkPackages(t))
	for _, path := range generatedModTemplates {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if templateConfigRead.MatchString(line) {
				violations = append(violations, path+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	for _, violation := range violations {
		t.Errorf("%s\n\treads config outside a declaration; declare the key on the mod's config struct and read it with app.LoadConfig", violation)
	}
}

// undeclaredConfigReads 找出直接读 viper（参数、变量、字段或 Registry.Config() 的返回值）与单键读取的调用。
func undeclaredConfigReads(t *testing.T, packages map[string][]string) []string {
	t.Helper()
	reads, err := configschema.UndeclaredReads(packages, func(path string) bool {
		return filepath.Dir(path) == "." && declarationLoaderFiles[filepath.Base(path)]
	})
	if err != nil {
		t.Fatal(err)
	}
	return reads
}

func TestEveryDeclaredConfigFieldIsRead(t *testing.T) {
	for _, unread := range unreadConfigFields(t, frameworkPackages(t)) {
		t.Errorf("%s\n\tdeclares a config key that nothing in its package reads; read it or delete the declaration (the generator would write it and doctor would accept it while it does nothing)", unread)
	}
}

func unreadConfigFields(t *testing.T, packages map[string][]string) []string {
	t.Helper()
	unread, err := configschema.UnreadFields(packages)
	if err != nil {
		t.Fatal(err)
	}
	return unread
}

// frameworkPackages 返回 app 与 kit 的非测试源文件，按目录（包）分组。
func frameworkPackages(t *testing.T) map[string][]string {
	t.Helper()
	packages, err := configschema.GoPackages(".", "../../wiring")
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) < 20 {
		t.Fatalf("found only %d framework packages; the walk no longer reaches kit", len(packages))
	}
	return packages
}

// 守卫自己必须能红：在临时包里放一处直接读 viper、一个没人读的声明，两个守卫都要点名它们。
func TestConfigDeclarationGuardsCatchTheirDrift(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mod := write("mod.go", `package shop

import "github.com/spf13/viper"

type shopConfig struct {
	MaxItems  int `+"`config:\"shop.max_items\"`"+`
	Forgotten int `+"`config:\"shop.forgotten\"`"+`
}

type Mod struct{ cfg shopConfig; raw *viper.Viper }

func (m *Mod) Init(cfg *viper.Viper) error {
	_ = m.cfg.MaxItems
	_ = cfg.GetInt("shop.secret_knob")
	_ = m.raw.IsSet("shop.other")
	return nil
}
`)
	packages := map[string][]string{dir: {mod}}
	reads := undeclaredConfigReads(t, packages)
	if len(reads) != 2 {
		t.Errorf("undeclaredConfigReads = %v; want the GetInt on the parameter and the IsSet on the field", reads)
	}
	unread := unreadConfigFields(t, packages)
	if len(unread) != 1 || !strings.Contains(unread[0], "Forgotten") {
		t.Errorf("unreadConfigFields = %v; want only Forgotten", unread)
	}
}
