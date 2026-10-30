package configfile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func mustLoad(t *testing.T, path string) (*viper.Viper, Layers) {
	t.Helper()
	cfg, layers, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	return cfg, layers
}

// 规则 2：includes 按列出的顺序叠加、后者覆盖前者；入口文件自己的键最后叠加，覆盖全部 includes。
func TestIncludesLayerInListedOrderAndTheEntryWins(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "x: a\ny: a\nz: a\n",
		"b.yaml":     "y: b\nz: b\n",
		"entry.yaml": "config:\n  includes: [a.yaml, b.yaml]\nz: entry\n",
	})
	cfg, layers := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	for key, want := range map[string]string{"x": "a", "y": "b", "z": "entry"} {
		if got := cfg.GetString(key); got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}
	if want := []string{filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")}; !reflect.DeepEqual(layers.Includes, want) {
		t.Errorf("Includes = %v; want %v in layering order", layers.Includes, want)
	}
}

// 规则 1：相对路径相对入口文件所在目录（不是进程工作目录），绝对路径原样使用。
func TestIncludePathsResolveAgainstTheEntryDirectory(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "abs.yaml")
	writeFiles(t, root, map[string]string{
		"service/defaults.yaml": "from_parent: yes-parent\n",
		"eks/local.yaml":        "from_sibling: yes-sibling\n",
		"eks/config.game.yaml":  "config:\n  includes: [../service/defaults.yaml, local.yaml, " + abs + "]\n",
	})
	if err := os.WriteFile(abs, []byte("from_absolute: yes-absolute\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // 工作目录里没有这些文件
	cfg, layers := mustLoad(t, filepath.Join(root, "eks", "config.game.yaml"))
	for key, want := range map[string]string{"from_parent": "yes-parent", "from_sibling": "yes-sibling", "from_absolute": "yes-absolute"} {
		if got := cfg.GetString(key); got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}
	if layers.Includes[0] != filepath.Join(root, "service", "defaults.yaml") {
		t.Errorf("Includes[0] = %s; want the cleaned path next to the entry's parent", layers.Includes[0])
	}
}

// 规则 3：子表逐键深合并；标量与列表整体替换（列表不拼接、不按下标合并）；标量替换子表、子表替换标量。
func TestMapsMergeDeeplyListsAndScalarsReplace(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml": `
game:
  web:
    addr: ":7780"
    route_prefix: /api/v2
  origins: [a, b, c]
  peers:
    - {name: p1, port: 1}
    - {name: p2, port: 2}
  limits:
    max: 10
scalar_then_map: 5
`,
		"entry.yaml": `
config:
  includes: [base.yaml]
game:
  web:
    addr: ":9000"
  origins: [z]
  peers:
    - {name: p3}
  limits: false
scalar_then_map:
  inner: 1
`,
	})
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	if got := cfg.GetString("game.web.addr"); got != ":9000" {
		t.Errorf("game.web.addr = %q; want the override", got)
	}
	if got := cfg.GetString("game.web.route_prefix"); got != "/api/v2" {
		t.Errorf("game.web.route_prefix = %q; want the earlier layer's sibling key kept", got)
	}
	if got := cfg.GetStringSlice("game.origins"); !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("game.origins = %v; want the list replaced as a whole", got)
	}
	peers, _ := cfg.Get("game.peers").([]any)
	if len(peers) != 1 || !reflect.DeepEqual(peers[0], map[string]any{"name": "p3"}) {
		t.Errorf("game.peers = %#v; want the list of maps replaced as a whole", cfg.Get("game.peers"))
	}
	if got := cfg.Get("game.limits"); got != false || cfg.IsSet("game.limits.max") {
		t.Errorf("game.limits = %#v (limits.max set: %v); want the scalar to replace the earlier map", got, cfg.IsSet("game.limits.max"))
	}
	if got := cfg.GetInt("scalar_then_map.inner"); got != 1 {
		t.Errorf("scalar_then_map.inner = %d; want the map to replace the earlier scalar", got)
	}
}

// 规则 3 的边界：后一层写 `~` 或 `{}` 不清掉前一层的值（viper 的 AllSettings 丢掉空值与空表，与 cube 相同）。
func TestNullAndEmptyMapDoNotClearEarlierLayers(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"base.yaml":  "a: 1\nb:\n  c: 2\n",
		"entry.yaml": "config:\n  includes: [base.yaml]\na: ~\nb: {}\n",
	})
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	if cfg.GetInt("a") != 1 || cfg.GetInt("b.c") != 2 {
		t.Errorf("a = %v, b.c = %v; want the earlier layer's 1 and 2 kept", cfg.Get("a"), cfg.Get("b.c"))
	}
}

// 规则 4：只支持一层。include 自己再写非空 config.includes（包括列回入口自己）时报错，点名两个文件；
// include 写空列表照常叠加。同一文件列两次照常叠加。
func TestNestedIncludesAreRejected(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"nested.yaml":  "config:\n  includes: [other.yaml]\n",
		"other.yaml":   "k: v\n",
		"empty.yaml":   "config:\n  includes: []\nk: empty\n",
		"entry.yaml":   "config:\n  includes: [nested.yaml]\n",
		"self.yaml":    "config:\n  includes: [self.yaml]\n",
		"twice.yaml":   "config:\n  includes: [other.yaml, empty.yaml, other.yaml]\n",
		"emptyok.yaml": "config:\n  includes: [empty.yaml]\n",
	})
	for entry, include := range map[string]string{"entry.yaml": "nested.yaml", "self.yaml": "self.yaml"} {
		_, _, err := Load(filepath.Join(dir, entry))
		if err == nil || !strings.Contains(err.Error(), "nested includes are not supported") ||
			!strings.Contains(err.Error(), filepath.Join(dir, include)) || !strings.Contains(err.Error(), "listed in "+filepath.Join(dir, entry)) {
			t.Errorf("%s: Load = %v; want a nested-includes refusal naming %s and the entry", entry, err, include)
		}
	}
	cfg, _ := mustLoad(t, filepath.Join(dir, "emptyok.yaml"))
	if cfg.GetString("k") != "empty" || cfg.IsSet(IncludesKey) {
		t.Errorf("include with an empty list: k = %v, config.includes set = %v", cfg.Get("k"), cfg.IsSet(IncludesKey))
	}
	cfg, layers := mustLoad(t, filepath.Join(dir, "twice.yaml"))
	if cfg.GetString("k") != "v" || len(layers.Includes) != 3 {
		t.Errorf("a file listed twice: k = %v, includes = %v; want the last listing to win", cfg.Get("k"), layers.Includes)
	}
}

// 规则 5：include 缺失或格式错误时报错，点名这个 include 与列出它的入口；缺失仍是 os.ErrNotExist。
// 入口文件自己缺失时返回 viper 的原始错误（不包装），App 据此判断走开发缺省。
func TestMissingOrMalformedIncludeNamesTheFileAndTheEntry(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"broken.yaml":  "ai_fusion: [\n",
		"missing.yaml": "config:\n  includes: [gone.yaml]\n",
		"bad.yaml":     "config:\n  includes: [broken.yaml]\n",
	})
	_, _, err := Load(filepath.Join(dir, "missing.yaml"))
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), filepath.Join(dir, "gone.yaml")) || !strings.Contains(err.Error(), "listed in "+filepath.Join(dir, "missing.yaml")) {
		t.Errorf("missing include: %v", err)
	}
	_, _, err = Load(filepath.Join(dir, "bad.yaml"))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "broken.yaml")) || !strings.Contains(err.Error(), "listed in "+filepath.Join(dir, "bad.yaml")) {
		t.Errorf("malformed include: %v", err)
	}
	_, _, err = Load(filepath.Join(dir, "no-entry.yaml"))
	if !os.IsNotExist(err) {
		t.Errorf("missing entry: %v; want the unwrapped not-exist error", err)
	}
}

// 规则 6：config.includes 不进入叠加结果；config 段里别的键照常保留。
func TestIncludesKeyIsNotInTheResult(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "k: v\n",
		"entry.yaml": "config:\n  includes: [a.yaml]\n  note: kept\n",
		"bare.yaml":  "config:\n  includes: [a.yaml]\n",
	})
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	if cfg.IsSet(IncludesKey) || cfg.GetString("config.note") != "kept" {
		t.Errorf("config.includes set = %v, config.note = %v", cfg.IsSet(IncludesKey), cfg.Get("config.note"))
	}
	cfg, _ = mustLoad(t, filepath.Join(dir, "bare.yaml"))
	if cfg.IsSet("config") {
		t.Errorf("an entry with only includes left a config section: %v", cfg.Get("config"))
	}
}

// config.includes 必须是非空路径字符串的列表（与 cube 的差异：cube 把单个字符串按空白拆开，这里拒绝）。
func TestIncludesMustBeAListOfPaths(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"string.yaml": "config:\n  includes: a.yaml\n",
		"blank.yaml":  "config:\n  includes: [\"  \"]\n",
		"number.yaml": "config:\n  includes: [1]\n",
	} {
		writeFiles(t, dir, map[string]string{name: body})
		_, _, err := Load(filepath.Join(dir, name))
		if err == nil || !strings.Contains(err.Error(), IncludesKey) || !strings.Contains(err.Error(), filepath.Join(dir, name)) {
			t.Errorf("%s: Load = %v; want a refusal naming config.includes in the entry", name, err)
		}
	}
}

// 入口没有写 config.includes 时与引入叠加之前相同：viper 直接读这一个文件，空表与空值照旧保留；ReadYAML 原样返回字节。
func TestEntryWithoutIncludesIsReadUnchanged(t *testing.T) {
	dir := t.TempDir()
	body := "# comment\nMixed: 1\nempty: {}\nnull_value: ~\n"
	writeFiles(t, dir, map[string]string{"plain.yaml": body})
	path := filepath.Join(dir, "plain.yaml")
	cfg, layers := mustLoad(t, path)
	direct := viper.New()
	direct.SetConfigFile(path)
	if err := direct.ReadInConfig(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.AllSettings(), direct.AllSettings()) || cfg.IsSet("empty") != direct.IsSet("empty") || layers.Layered() || len(layers.Includes) != 0 {
		t.Errorf("plain entry: %v (empty set %v), layered %v; want exactly what viper reads directly: %v", cfg.AllSettings(), cfg.IsSet("empty"), layers.Layered(), direct.AllSettings())
	}
	raw, _, err := ReadYAML(path)
	if err != nil || string(raw) != body {
		t.Errorf("ReadYAML(plain) = %q, %v; want the file's bytes", raw, err)
	}
}

// 规则 7：键不区分大小写，两层里只差大小写的段合并成同一段（viper 统一小写）。每个文件按扩展名解析（json include）。
func TestLayersAreCaseInsensitiveAndParsedByExtension(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.json":     `{"Mongo": {"URI": "mongodb://a", "database": "x"}}`,
		"entry.yaml": "config:\n  includes: [a.json]\nmongo:\n  uri: mongodb://entry\n",
	})
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	if cfg.GetString("mongo.uri") != "mongodb://entry" || cfg.GetString("mongo.database") != "x" {
		t.Errorf("mongo = %v; want one section: uri from the entry, database from a.json", cfg.Get("mongo"))
	}
}

// 环境变量名一类的值（*_env）是普通字符串，照常叠加，不在加载时展开；${VAR} 也不展开（与引入叠加之前相同）。
func TestEnvironmentNamedValuesLayerAsPlainStrings(t *testing.T) {
	t.Setenv("ROOST_CONFIGFILE_TEST", "secret")
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "mongo:\n  password_env: A_PASSWORD\n  uri_env: MONGO_URI\n  uri: ${ROOST_CONFIGFILE_TEST}\n",
		"entry.yaml": "config:\n  includes: [a.yaml]\nmongo:\n  password_env: ROOST_CONFIGFILE_TEST\n",
	})
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	for key, want := range map[string]string{"mongo.password_env": "ROOST_CONFIGFILE_TEST", "mongo.uri_env": "MONGO_URI", "mongo.uri": "${ROOST_CONFIGFILE_TEST}"} {
		if got := cfg.GetString(key); got != want {
			t.Errorf("%s = %q; want %q unexpanded", key, got, want)
		}
	}
}

// Read 只往 cfg 的文件层放叠加结果：调用方先设的缺省值与 Set 的覆盖值照常生效（App 的 server_type / --sid 依赖这一点）。
func TestReadKeepsTheCallersDefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "sid: 7\nlog:\n  level: debug\n",
		"entry.yaml": "config:\n  includes: [a.yaml]\n",
	})
	cfg := viper.New()
	cfg.SetDefault("log.dir", "log")
	cfg.SetDefault("sid", 1000)
	cfg.Set("log.level", "warn")
	if _, err := Read(cfg, filepath.Join(dir, "entry.yaml")); err != nil {
		t.Fatal(err)
	}
	if cfg.GetInt("sid") != 7 || cfg.GetString("log.dir") != "log" || cfg.GetString("log.level") != "warn" || cfg.ConfigFileUsed() != filepath.Join(dir, "entry.yaml") {
		t.Errorf("sid=%v log.dir=%v log.level=%v file=%s; want 7 from the include, the default dir, the Set override, the entry path",
			cfg.Get("sid"), cfg.Get("log.dir"), cfg.Get("log.level"), cfg.ConfigFileUsed())
	}
}

// 来源：每个键取值的文件是最后写它的那一层；Annotate / Describe 给点名键的问题补上它。
func TestOriginsNameTheLastLayerThatSetAKey(t *testing.T) {
	dir := t.TempDir()
	a, b, entry := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml"), filepath.Join(dir, "entry.yaml")
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "shop:\n  max_items: 1\n  region: cn\nflip: {inner: 1}\nflop: 1\n",
		"b.yaml":     "shop:\n  max_items: 2\nflip: 2\nflop: {inner: 2}\n",
		"entry.yaml": "config:\n  includes: [a.yaml, b.yaml]\nlog:\n  level: debug\n",
	})
	_, layers := mustLoad(t, entry)
	for key, want := range map[string][]string{
		"shop.max_items": {b}, "shop.region": {a}, "log.level": {entry}, "shop": {a, b},
		"flip": {b}, "flip.inner": nil, "flop": {b}, "flop.inner": {b}, "missing": nil,
	} {
		if got := layers.Origins(key); !reflect.DeepEqual(got, want) {
			t.Errorf("Origins(%s) = %v; want %v", key, got, want)
		}
	}
	err := layers.Annotate(errors.Join(errors.New("mod shop: config: shop.max_items must be at most 1"), os.ErrNotExist))
	if !strings.Contains(err.Error(), "shop.max_items must be at most 1 (from "+b+")") || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "is layered from "+a+", "+b+", "+entry) {
		t.Errorf("Annotate = %v", err)
	}
	if got := layers.Describe("config: shop.region is not a known key"); got != "config: shop.region is not a known key (from "+a+")" {
		t.Errorf("Describe = %q", got)
	}
	if got := (Layers{Entry: entry}).Describe("config: shop.region x"); got != "config: shop.region x" {
		t.Errorf("Describe without layering = %q; want unchanged", got)
	}
}

// ReadYAML 写了 includes 时返回叠加结果的 YAML（与 Load 同一份结果），给按文本检查的工具用。
func TestReadYAMLRendersTheLayeredConfig(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.yaml":     "sid: 1000\nnats:\n  url: nats://a\n  prefix: p\n",
		"entry.yaml": "config:\n  includes: [a.yaml]\nnats:\n  url: nats://entry\n",
	})
	raw, layers, err := ReadYAML(filepath.Join(dir, "entry.yaml"))
	if err != nil || !layers.Layered() {
		t.Fatalf("ReadYAML = %v, layered %v", err, layers.Layered())
	}
	var got map[string]any
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	cfg, _ := mustLoad(t, filepath.Join(dir, "entry.yaml"))
	if !reflect.DeepEqual(got, cfg.AllSettings()) {
		t.Errorf("ReadYAML = %v; want what Load reads: %v", got, cfg.AllSettings())
	}
}
