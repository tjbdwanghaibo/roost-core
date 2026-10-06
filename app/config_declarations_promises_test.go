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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/viper"
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

// --- 守卫：读了没声明 ---

// viperReadMethods 是 *viper.Viper 读配置的方法。写入（Set、SetDefault）与读文件（ReadInConfig）不算读键。
var viperReadMethods = map[string]bool{
	"Get": true, "GetBool": true, "GetDuration": true, "GetFloat64": true, "GetInt": true, "GetInt32": true,
	"GetInt64": true, "GetIntSlice": true, "GetSizeInBytes": true, "GetString": true, "GetStringMap": true,
	"GetStringMapString": true, "GetStringMapStringSlice": true, "GetStringSlice": true, "GetTime": true,
	"GetUint": true, "GetUint8": true, "GetUint16": true, "GetUint32": true, "GetUint64": true,
	"IsSet": true, "AllKeys": true, "AllSettings": true, "InConfig": true, "Sub": true,
	"Unmarshal": true, "UnmarshalKey": true, "UnmarshalExact": true,
}

// singleKeyReads 是 app 的单键读取：读到的键不在任何声明里。
var singleKeyReads = map[string]bool{"ConfigBool": true, "ConfigDuration": true, "ConfigInt": true, "ConfigInt64": true}

// declarationLoaderFiles 是唯一允许直接读 viper 的地方：声明读取的 viper 适配器与单键读取本身。
var declarationLoaderFiles = map[string]bool{"config_schema.go": true, "config_values.go": true}

// generatedModTemplates 是生成进工程、读配置的 Mod 模板（player TCP 接入层、RPC 客户端 Mod）。它们是字符串里的
// Go 源码，按文本检查。
var generatedModTemplates = []string{"../codegen/internal/roost/render_player_tcp.go", "../codegen/internal/servicerpc/template.go"}

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
	var out []string
	for _, files := range packages {
		fset := token.NewFileSet()
		parsed := parseFiles(t, fset, files)
		viperFields := map[string]bool{}
		for _, file := range parsed {
			ast.Inspect(file, func(node ast.Node) bool {
				if field, ok := node.(*ast.Field); ok && isViperType(field.Type) {
					for _, name := range field.Names {
						viperFields[name.Name] = true
					}
				}
				return true
			})
		}
		for path, file := range parsed {
			if filepath.Dir(path) == "." && declarationLoaderFiles[filepath.Base(path)] {
				continue
			}
			viperNames := map[string]bool{}
			for name := range viperFields { // 参数与字段：同一个包里叫这个名字的 *viper.Viper
				viperNames[name] = true
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.ValueSpec:
					if isViperType(n.Type) {
						for _, name := range n.Names {
							viperNames[name.Name] = true
						}
					}
				case *ast.AssignStmt:
					for i, rhs := range n.Rhs {
						if call, ok := rhs.(*ast.CallExpr); ok && i < len(n.Lhs) && returnsViper(call) {
							if ident, ok := n.Lhs[i].(*ast.Ident); ok {
								viperNames[ident.Name] = true
							}
						}
					}
				}
				return true
			})
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				var bad bool
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					switch {
					case viperReadMethods[fun.Sel.Name] && isViperValue(fun.X, viperNames, viperFields):
						bad = true
					case singleKeyReads[fun.Sel.Name] || fun.Sel.Name == "NewConfigReader":
						if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "app" {
							bad = true
						}
					}
				case *ast.Ident:
					bad = singleKeyReads[fun.Name] && filepath.Dir(path) == "."
				}
				if bad {
					out = append(out, fset.Position(call.Pos()).String())
				}
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}

func isViperType(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Viper"
}

func returnsViper(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "viper" && sel.Sel.Name == "New" {
		return true
	}
	return sel.Sel.Name == "Config" && len(call.Args) == 0
}

func isViperValue(expr ast.Expr, names, fields map[string]bool) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		return names[x.Name]
	case *ast.SelectorExpr:
		return fields[x.Sel.Name]
	case *ast.CallExpr:
		return returnsViper(x)
	case *ast.ParenExpr:
		return isViperValue(x.X, names, fields)
	}
	return false
}

// --- 守卫：声明了没读 ---

func TestEveryDeclaredConfigFieldIsRead(t *testing.T) {
	for _, unread := range unreadConfigFields(t, frameworkPackages(t)) {
		t.Errorf("%s\n\tdeclares a config key that nothing in its package reads; read it or delete the declaration (the generator would write it and doctor would accept it while it does nothing)", unread)
	}
}

func unreadConfigFields(t *testing.T, packages map[string][]string) []string {
	t.Helper()
	var out []string
	for _, files := range packages {
		fset := token.NewFileSet()
		parsed := parseFiles(t, fset, files)
		selected := map[string]bool{}
		type declared struct {
			name, key string
			pos       token.Pos
		}
		var fields []declared
		for _, file := range parsed {
			ast.Inspect(file, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.SelectorExpr:
					selected[n.Sel.Name] = true
				case *ast.StructType:
					for _, field := range n.Fields.List {
						if field.Tag == nil || len(field.Names) == 0 {
							continue
						}
						tag, _ := strconv.Unquote(field.Tag.Value)
						key, ok := reflect.StructTag(tag).Lookup("config")
						if !ok {
							continue
						}
						for _, name := range field.Names {
							fields = append(fields, declared{name.Name, key, name.Pos()})
						}
					}
				}
				return true
			})
		}
		for _, field := range fields {
			if !selected[field.name] {
				out = append(out, fset.Position(field.pos).String()+": "+field.name+" (config:\""+field.key+"\")")
			}
		}
	}
	sort.Strings(out)
	return out
}

// frameworkPackages 返回 app 与 kit 的非测试源文件，按目录（包）分组。
func frameworkPackages(t *testing.T) map[string][]string {
	t.Helper()
	packages := map[string][]string{}
	for _, root := range []string{".", "../kit"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			dir := filepath.Dir(path)
			packages[dir] = append(packages[dir], path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(packages) < 20 {
		t.Fatalf("found only %d framework packages; the walk no longer reaches kit", len(packages))
	}
	return packages
}

func parseFiles(t *testing.T, fset *token.FileSet, files []string) map[string]*ast.File {
	t.Helper()
	parsed := make(map[string]*ast.File, len(files))
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed[path] = file
	}
	return parsed
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
