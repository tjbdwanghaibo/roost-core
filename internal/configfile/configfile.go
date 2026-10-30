// Package configfile 读服务进程的配置文件：入口文件与它在 config.includes 里列出的叠加层。
//
// 叠加规则只在这里实现一次（维护者 2026-10-30 决定 R5 方案 A，规则与 cube 的 app.LoadConfigFile 对齐），
// App 启动与 --check-config（framework/app）、读服务配置的工具（app.LoadConfigFile、wiring/dataengine 的
// LoadDatabaseMapFile）、roost config check 与 doctor（codegen，经 ReadYAML）都经它：
//
//  1. 入口文件的 config.includes 是一个路径列表；相对路径相对入口文件所在目录解析，绝对路径原样使用。
//  2. includes 按列出的顺序叠加，后者覆盖前者；入口文件自己的键最后叠加，覆盖全部 includes。
//  3. 子表（map）逐键深合并；标量与列表整体替换；一层里的标量替换前一层同名子表，子表也替换同名标量。
//     值为空（`~`）或空表（`{}`）的键在这一层不产生任何效果，叠加不能删除前一层写过的键。
//  4. 只支持一层：include 自己再写非空的 config.includes 时报错（因此不会出现循环）；同一文件列两次照常叠加两次。
//  5. include 读不出（缺失、格式错误）时报错，点名这个 include 与列出它的入口文件。
//  6. config.includes 本身不进入叠加后的配置（它是加载指令，不是任何 Mod 读的键）。
//  7. 每个文件按扩展名由 viper 解析（与 --config 一样），键不区分大小写（viper 统一小写）。
//
// 入口文件没有写 config.includes 时与此前完全相同：viper 直接读这一个文件，没有任何转换。
// 叠加只发生在文件层：环境变量名一类的值（例如 mongo.password_env）是普通字符串，照常叠加，由读它的 Mod
// 在 Init 时解析；App 之后按命令行写入的 server_type / --sid 仍覆盖叠加结果。
package configfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// IncludesKey 是入口配置里列出叠加层的键。
const IncludesKey = "config.includes"

// Layers 说明一份配置由哪些文件叠成：Includes 按叠加顺序（后者覆盖前者），入口文件 Entry 在它们之后。
// 入口没有写 config.includes 时 Includes 为空。
type Layers struct {
	Entry    string
	Includes []string
	// origins 是叠加结果里每个叶子键（小写、点分）最后取值的文件；只在有 includes 时记录。
	origins map[string]string
}

// Layered 报告这份配置是否经过叠加（入口写了 config.includes）。
func (l Layers) Layered() bool { return l.origins != nil }

// Origins 返回 key 在叠加结果里取值的文件：key 是叶子时是一个文件；是子表时是其下各叶子来自的文件（按叠加顺序）。
// 没有叠加或叠加结果里没有这个键时返回 nil。
func (l Layers) Origins(key string) []string {
	if l.origins == nil {
		return nil
	}
	key = strings.ToLower(key)
	if file, ok := l.origins[key]; ok {
		return []string{file}
	}
	seen := map[string]bool{}
	for leaf, file := range l.origins {
		if strings.HasPrefix(leaf, key+".") {
			seen[file] = true
		}
	}
	var out []string
	for _, file := range append(append([]string(nil), l.Includes...), l.Entry) {
		if seen[file] {
			out = append(out, file)
			delete(seen, file)
		}
	}
	return out
}

// errorKey 取出配置错误里点名的键：声明检查的错误都以 "config: <键>" 开头（可能带 "mod x: " 一类前缀）。
var errorKey = regexp.MustCompile(`config: ([A-Za-z0-9_.*-]+)`)

// Annotate 给叠加后的配置检查错误补上键的来源文件（"(from <文件>)"），并在末尾说明叠加了哪些文件。
// 没有叠加时原样返回。每条错误仍包着原错误，errors.Is / errors.As 不变。
func (l Layers) Annotate(err error) error {
	if err == nil || !l.Layered() {
		return err
	}
	var parts []error
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts = joined.Unwrap()
	} else {
		parts = []error{err}
	}
	out := make([]error, 0, len(parts)+1)
	for _, part := range parts {
		if files := l.messageOrigins(part.Error()); len(files) > 0 {
			part = fmt.Errorf("%w (from %s)", part, strings.Join(files, ", "))
		}
		out = append(out, part)
	}
	out = append(out, fmt.Errorf("config: %s is layered from %s (later files override earlier ones)", l.Entry, strings.Join(append(append([]string(nil), l.Includes...), l.Entry), ", ")))
	return errors.Join(out...)
}

// Describe 是 Annotate 的文本版（doctor / config check 的问题是字符串）：给一条点名键的问题补上 "(from <文件>)"。
func (l Layers) Describe(problem string) string {
	if files := l.messageOrigins(problem); len(files) > 0 {
		return problem + " (from " + strings.Join(files, ", ") + ")"
	}
	return problem
}

// messageOrigins 取出问题文本点名的键（"config: <键>"）在叠加结果里来自的文件；没有叠加、没点名键或键不在叠加结果里时为 nil。
func (l Layers) messageOrigins(message string) []string {
	if !l.Layered() {
		return nil
	}
	match := errorKey.FindStringSubmatch(message)
	if match == nil {
		return nil
	}
	return l.Origins(strings.TrimRight(match[1], "."))
}

// Read 把 path 读进 cfg（App 的 viper，可以已经设了缺省值与覆盖值），返回叠加层。
//
// 入口文件本身读不出时原样返回 viper 的错误（不包装，调用方用它判断“文件不存在”走开发缺省）；
// include 的错误都包装并点名文件，不会被当成入口缺失。
func Read(cfg *viper.Viper, path string) (Layers, error) {
	cfg.SetConfigFile(path) // 入口读不出（开发启动退回缺省值）时也记下路径，与引入叠加之前相同
	entry, err := readFile(path)
	if err != nil {
		return Layers{Entry: path}, err
	}
	if !declaresIncludes(entry) {
		// 与引入叠加之前相同：viper 直接读这一个文件（不经 AllSettings，空表、空值照旧保留）。
		return Layers{Entry: path}, cfg.ReadInConfig()
	}
	merged, layers, err := overlay(path, entry)
	if err != nil {
		return layers, err
	}
	// 调用方给的 cfg 还没读过文件、文件层为空，MergeConfigMap 等于把叠加结果整份放进文件层；缺省值与 Set 的覆盖值不受影响。
	return layers, cfg.MergeConfigMap(merged)
}

// Load 把 path 读进一个新的 viper（给读服务配置的工具用），规则与 App 启动时相同。
func Load(path string) (*viper.Viper, Layers, error) {
	cfg := viper.New()
	layers, err := Read(cfg, path)
	if err != nil {
		return nil, layers, err
	}
	return cfg, layers, nil
}

// ReadYAML 给按 YAML 文本检查配置的工具（roost config check、doctor）读一份服务配置：入口没有写 config.includes
// 时原样返回文件内容；写了时返回叠加结果的 YAML（键小写，config.includes 已去掉）。入口文件读不出时返回读文件的
// 错误；能读但解析不了时原样返回内容，由调用方自己的解析报告。include 的错误照常返回。
func ReadYAML(path string) ([]byte, Layers, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, Layers{Entry: path}, err
	}
	entry, err := readFileFrom(path, raw)
	if err != nil || !declaresIncludes(entry) {
		return raw, Layers{Entry: path}, nil
	}
	merged, layers, err := overlay(path, entry)
	if err != nil {
		return nil, layers, err
	}
	text, err := yaml.Marshal(merged)
	if err != nil {
		return nil, layers, fmt.Errorf("config: render the layered %s: %w", path, err)
	}
	return text, layers, nil
}

// overlay 按包注释的规则叠加 entry（已读出的入口文件）列出的 includes 与它自己，返回叠加结果（小写键的嵌套表）。
func overlay(path string, entry *viper.Viper) (map[string]any, Layers, error) {
	layers := Layers{Entry: path, origins: map[string]string{}}
	names, err := includeList(entry.Get(IncludesKey))
	if err != nil {
		return nil, layers, fmt.Errorf("config: %s in %s: %w", IncludesKey, path, err)
	}
	merged := map[string]any{}
	baseDir := filepath.Dir(path)
	for _, name := range names {
		includePath := name
		if !filepath.IsAbs(includePath) {
			includePath = filepath.Join(baseDir, includePath)
		}
		includePath = filepath.Clean(includePath)
		included, err := readFile(includePath)
		if err != nil {
			return nil, layers, fmt.Errorf("config: read include %s (listed in %s): %w", includePath, path, err)
		}
		nested, err := includeList(included.Get(IncludesKey))
		if err != nil || len(nested) > 0 {
			return nil, layers, fmt.Errorf("config: include %s (listed in %s) has its own %s; nested includes are not supported, list every file in %s", includePath, path, IncludesKey, path)
		}
		mergeLayer(merged, withoutIncludes(included.AllSettings()), includePath, "", layers.origins)
		layers.Includes = append(layers.Includes, includePath)
	}
	mergeLayer(merged, withoutIncludes(entry.AllSettings()), path, "", layers.origins)
	return merged, layers, nil
}

// declaresIncludes：入口文件写了 config.includes（值为空 `~` 视为没写）。写了空列表也算，键照样去掉。
func declaresIncludes(entry *viper.Viper) bool { return entry.InConfig(IncludesKey) }

// includeList 读 config.includes 的值：必须是路径字符串列表，每项非空（前后空白去掉）。
// 与 cube 的差异：cube 用 GetStringSlice，单个字符串会按空白拆成多个路径、非字符串项被转成字符串；
// 这里拒绝，配置写错不静默换一种解释。
func includeList(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a list of file paths, got %v", raw)
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("every entry must be a non-empty file path, got %v", item)
		}
		names = append(names, strings.TrimSpace(name))
	}
	return names, nil
}

// withoutIncludes 去掉一层设置里的 config.includes；config 段因此变空时整段去掉。
func withoutIncludes(settings map[string]any) map[string]any {
	section, ok := settings["config"].(map[string]any)
	if !ok {
		return settings
	}
	delete(section, "includes")
	if len(section) == 0 {
		delete(settings, "config")
	}
	return settings
}

// mergeLayer 把一层设置 source 深合并进 target（规则见包注释第 3 条），并记下每个叶子键来自 file。
// source 来自 viper 的 AllSettings：键已小写，叶子不是表（空表与空值已被 viper 丢掉）。
func mergeLayer(target, source map[string]any, file, prefix string, origins map[string]string) {
	for key, value := range source {
		full := key
		if prefix != "" {
			full = prefix + "." + key
		}
		if sourceMap, ok := value.(map[string]any); ok {
			targetMap, isMap := target[key].(map[string]any)
			if !isMap {
				targetMap = map[string]any{}
				target[key] = targetMap
				delete(origins, full) // 子表替换了前一层的标量
			}
			mergeLayer(targetMap, sourceMap, file, full, origins)
			continue
		}
		if _, wasMap := target[key].(map[string]any); wasMap {
			for leaf := range origins { // 标量替换了前一层的子表
				if strings.HasPrefix(leaf, full+".") {
					delete(origins, leaf)
				}
			}
		}
		target[key] = value
		origins[full] = file
	}
}

func readFile(path string) (*viper.Viper, error) {
	cfg := viper.New()
	cfg.SetConfigFile(path)
	if err := cfg.ReadInConfig(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readFileFrom 用已经读出的内容解析 path（格式按扩展名，与 readFile 相同）。
func readFileFrom(path string, raw []byte) (*viper.Viper, error) {
	cfg := viper.New()
	ext := strings.TrimPrefix(filepath.Ext(path), ".")
	if ext == "" {
		return nil, fmt.Errorf("config: %s has no extension to tell its format", path)
	}
	cfg.SetConfigType(ext)
	if err := cfg.ReadConfig(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return cfg, nil
}
