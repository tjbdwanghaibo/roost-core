package roost

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// 模块的配置（维护者 2026-11-14 决定 D19 B，R5）：清单的 config: 指出模块包里一个带 config tag 的结构体，
// 写法与 Mod 的配置声明相同（framework/app 的 SchemaOf / LoadConfig）。规则只在那个结构体里声明一次：
//
//   - 安装器从模板读出它声明的键（moduleConfigKeys，只读 tag，不编译），把标了 example 的键写进模块所在进程的
//     配置（开发配置、生产示例、k8s Secret 示例；已有同名顶层段的不动）；
//   - bootstrap 把它交给 wiring/modulehost 的 Mod：托管服务上就是装模块入口的那个 Mod，业务服务上另加一个
//     只带配置的。于是 `--check-config`、`roost config check`、doctor 与 `--print-config` 按同一份声明查它，
//     进程启动时在任何 Mod Start 之前按它装载一次，经 use 交给模块。
//
// 配置跟着模块的入口走：装在模块入口所在的进程（runtime.framework 的托管服务）；没有入口的模块装在它的业务服务。

// ModuleConfig 是清单的 config:。
type ModuleConfig struct {
	// Package 是结构体所在的包（工程内路径），必须是本模块 files/ 里的包：game/components/<包>、game/modules/<包>，
	// 或入口在托管服务上的模块的 services/<框架服务>/<包>。
	Package string `yaml:"package" json:"package"`
	// Type 是带 config tag 的结构体名（导出）。它与它用到的同包类型要写在能按 Go 解析的模板里
	// （占位只出现在字符串里），安装器读它的 tag。
	Type string `yaml:"type" json:"type"`
	// Use 是同包的 func(<Type>)，进程启动时接收装载好的值（通常把它存进 modulehost.Value）。不写时只声明：
	// 模块在自己的入口里用 app.LoadConfig 读（登录入口的 portal.*）。
	Use string `yaml:"use,omitempty" json:"use,omitempty"`
}

// moduleConfigPackagePattern 是 config.package 允许的落点。
var moduleConfigPackagePattern = regexp.MustCompile(`^(game/(components|modules)|services/[a-z][a-z0-9_]*)/[a-z][a-z0-9_]*(/[a-z][a-z0-9_]*)*$`)

// validateModuleConfig 检查清单的 config:，并从模板读出它声明的键。清单加载时（module list / plan / add / verify）执行。
func validateModuleConfig(source fs.FS, spec ModuleSpec) (configschema.Schema, error) {
	config := spec.Config
	if config == nil {
		return configschema.Schema{}, nil
	}
	if !moduleConfigPackagePattern.MatchString(config.Package) {
		return configschema.Schema{}, fmt.Errorf("config.package %q must be a package under game/components/, game/modules/ or services/<framework>/", config.Package)
	}
	// 配置装在模块入口所在的进程：托管服务上的包只能配 runtime.framework 的那个服务，game/ 下的包不能装进托管服务。
	framework := ""
	if spec.Runtime != nil {
		framework = spec.Runtime.Framework
	}
	if rest, hosted := strings.CutPrefix(config.Package, frameworkServicesDir); hosted != (framework != "") || (hosted && !strings.HasPrefix(rest, framework+"/")) {
		return configschema.Schema{}, fmt.Errorf("config.package %s must live where the module's runtime runs: services/<runtime.framework>/... for a runtime on a hosted framework service, otherwise under game/", config.Package)
	}
	if !spec.shipsPackage(config.Package) {
		return configschema.Schema{}, fmt.Errorf("config.package %s is not in files/; the config struct ships with the module", config.Package)
	}
	if !modulePascalPattern.MatchString(config.Type) {
		return configschema.Schema{}, fmt.Errorf("config.type %q must name an exported struct", config.Type)
	}
	if config.Use != "" && !modulePascalPattern.MatchString(config.Use) {
		return configschema.Schema{}, fmt.Errorf("config.use %q must name an exported function", config.Use)
	}
	files := map[string][]byte{}
	for _, rel := range spec.Files {
		if path.Dir(rel) != config.Package || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		body, err := fs.ReadFile(source, path.Join(spec.Name, "files", rel+".tmpl"))
		if err != nil {
			return configschema.Schema{}, err
		}
		files[rel] = body
	}
	keys, err := moduleConfigKeys(files, config.Type, config.Use)
	if err != nil {
		return configschema.Schema{}, fmt.Errorf("config: %w", err)
	}
	return configschema.Schema{Keys: keys}, nil
}

// moduleConfigKeys 从一个包的源码（文件名 -> 内容）读出 typeName 声明的配置键，规则与 configschema.Of 相同
// （module_config_test 用反射的结果对照）：字段的类型决定键的类型，tag 写键名、缺省值、范围、说明与 example。
// 不能按 Go 解析的文件跳过（模板里有不在字符串里的占位）；use 非空时同包必须有 func use(typeName)。
func moduleConfigKeys(files map[string][]byte, typeName, use string) ([]configschema.Key, error) {
	types := map[string]ast.Expr{}
	receivers := map[string]string{} // 函数名 -> 唯一参数的类型名
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						types[typeSpec.Name.Name] = typeSpec.Type
					}
				}
			case *ast.FuncDecl:
				if decl.Recv == nil && len(decl.Type.Params.List) == 1 && len(decl.Type.Params.List[0].Names) <= 1 {
					if ident, ok := decl.Type.Params.List[0].Type.(*ast.Ident); ok {
						receivers[decl.Name.Name] = ident.Name
					}
				}
			}
		}
	}
	root, ok := types[typeName]
	if !ok {
		return nil, fmt.Errorf("type %s is not declared in a file of the package that parses as Go (keep template placeholders inside strings in that file)", typeName)
	}
	if _, isStruct := root.(*ast.StructType); !isStruct {
		return nil, fmt.Errorf("type %s is not a struct", typeName)
	}
	if use != "" && receivers[use] != typeName {
		return nil, fmt.Errorf("func %s(%s) is not declared in a file of the package that parses as Go", use, typeName)
	}
	collector := configKeyCollector{types: types}
	if err := collector.collectKeys(root, ""); err != nil {
		return nil, fmt.Errorf("type %s: %w", typeName, err)
	}
	return collector.keys, nil
}

// configKeyCollector 是 configschema.collectKeys 按语法树的对应：同一套 tag、同一种拼键名的规则。
type configKeyCollector struct {
	types map[string]ast.Expr
	keys  []configschema.Key
	depth int
}

// resolve 把同包的类型名换成它的定义；标准库与别的包的类型原样返回。
func (c *configKeyCollector) resolve(expr ast.Expr) ast.Expr {
	for range 8 {
		ident, ok := expr.(*ast.Ident)
		if !ok {
			return expr
		}
		next, declared := c.types[ident.Name]
		if !declared {
			return expr
		}
		expr = next
	}
	return expr
}

func (c *configKeyCollector) collectKeys(expr ast.Expr, prefix string) error {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > 16 {
		return fmt.Errorf("config struct nests too deep (a type refers to itself?)")
	}
	structType, ok := c.resolve(expr).(*ast.StructType)
	if !ok {
		return fmt.Errorf("%s is not a struct", prefix)
	}
	for _, field := range structType.Fields.List {
		tag := reflect.StructTag("")
		if field.Tag != nil {
			raw, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return err
			}
			tag = reflect.StructTag(raw)
		}
		name, hasTag := tag.Lookup("config")
		name, options, _ := strings.Cut(name, ",")
		fieldNames := field.Names
		if len(fieldNames) == 0 {
			// 匿名嵌入：没有 tag 时不加前缀，有 tag 时按字段处理（字段名是类型名）。
			if !hasTag {
				if _, isStruct := c.resolve(field.Type).(*ast.StructType); isStruct {
					if err := c.collectKeys(field.Type, prefix); err != nil {
						return err
					}
					continue
				}
			}
			fieldNames = []*ast.Ident{embeddedName(field.Type)}
		}
		for _, ident := range fieldNames {
			if ident == nil {
				return fmt.Errorf("an embedded field of an unsupported type")
			}
			if !hasTag {
				if ident.IsExported() {
					return fmt.Errorf("field %s has no config tag", ident.Name)
				}
				continue
			}
			if !ident.IsExported() {
				return fmt.Errorf("field %s has a config tag but is not exported", ident.Name)
			}
			if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, " *") {
				return fmt.Errorf("field %s: config key %q must be a non-empty lowercase dotted name", ident.Name, name)
			}
			full := name
			if prefix != "" && !strings.HasSuffix(prefix, "_") {
				full = prefix + "." + name
			} else if prefix != "" {
				full = prefix + name
			}
			if err := c.collectField(field.Type, full, tag, options); err != nil {
				return fmt.Errorf("field %s: %w", ident.Name, err)
			}
		}
	}
	return nil
}

func embeddedName(expr ast.Expr) *ast.Ident {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr
	case *ast.SelectorExpr:
		return expr.Sel
	}
	return nil
}

func (c *configKeyCollector) collectField(expr ast.Expr, full string, tag reflect.StructTag, options string) error {
	resolved := c.resolve(expr)
	if _, isStruct := resolved.(*ast.StructType); isStruct {
		if help := tag.Get("help"); help != "" || options == "closed" {
			c.keys = append(c.keys, configschema.Key{Name: full, Kind: configschema.KindSection, Help: help, Closed: options == "closed"})
		}
		return c.collectKeys(resolved, full)
	}
	if mapType, isMap := resolved.(*ast.MapType); isMap {
		if kind, err := c.kindOf(mapType.Key); err != nil || kind != configschema.KindString {
			return fmt.Errorf("map keys must be strings")
		}
		key := configschema.Key{Name: full, Kind: configschema.KindMap, Help: tag.Get("help")}
		key.Example, key.Starter = tag.Lookup("example")
		c.keys = append(c.keys, key)
		return c.collectElem(mapType.Value, full+".*")
	}
	kind, err := c.kindOf(expr)
	if err != nil {
		return err
	}
	key := configschema.Key{
		Name: full, Kind: kind, Default: tag.Get("default"),
		Min: tag.Get("min"), Max: tag.Get("max"),
		Required: tag.Get("required") == "true", Secret: tag.Get("secret") == "true" || tag.Get("secret") == "optional",
		SecretOptional: tag.Get("secret") == "optional",
		Help:           tag.Get("help"),
		Shared:         tag.Get("shared"),
		Follows:        tag.Get("follows"),
		Listen:         tag.Get("listen"),
		Unique:         tag.Get("unique"),
	}
	key.Example, key.Starter = tag.Lookup("example")
	if enum := tag.Get("enum"); enum != "" {
		if kind != configschema.KindString {
			return fmt.Errorf("enum is only for strings")
		}
		for _, item := range strings.Split(enum, "|") {
			key.Enum = append(key.Enum, strings.ToLower(strings.TrimSpace(item)))
		}
	}
	c.keys = append(c.keys, key)
	return nil
}

func (c *configKeyCollector) collectElem(expr ast.Expr, full string) error {
	resolved := c.resolve(expr)
	switch resolved := resolved.(type) {
	case *ast.StructType:
		c.keys = append(c.keys, configschema.Key{Name: full, Kind: configschema.KindSection, Closed: true})
		return c.collectKeys(resolved, full)
	case *ast.MapType:
		if kind, err := c.kindOf(resolved.Key); err != nil || kind != configschema.KindString {
			return fmt.Errorf("map keys must be strings")
		}
		return c.collectElem(resolved.Value, full+".*")
	}
	kind, err := c.kindOf(expr)
	if err != nil {
		return err
	}
	c.keys = append(c.keys, configschema.Key{Name: full, Kind: kind})
	return nil
}

// kindOf 与 configschema 的 kindOf 相同：time.Duration、[]string 与各种标量（同包按它们定义的具名类型也算）。
func (c *configKeyCollector) kindOf(expr ast.Expr) (configschema.Kind, error) {
	switch expr := expr.(type) {
	case *ast.SelectorExpr:
		if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Name == "time" && expr.Sel.Name == "Duration" {
			return configschema.KindDuration, nil
		}
	case *ast.ArrayType:
		if elem, ok := expr.Elt.(*ast.Ident); ok && expr.Len == nil && elem.Name == "string" {
			return configschema.KindStrings, nil
		}
	case *ast.Ident:
		switch expr.Name {
		case "bool":
			return configschema.KindBool, nil
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
			return configschema.KindInt, nil
		case "float32", "float64":
			return configschema.KindFloat, nil
		case "string":
			return configschema.KindString, nil
		}
		if declared, ok := c.types[expr.Name]; ok {
			return c.kindOf(declared)
		}
	}
	return "", fmt.Errorf("unsupported config field type %s", typeText(expr))
}

func typeText(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return typeText(expr.X) + "." + expr.Sel.Name
	case *ast.StarExpr:
		return "*" + typeText(expr.X)
	case *ast.ArrayType:
		return "[]" + typeText(expr.Elt)
	case *ast.MapType:
		return "map[" + typeText(expr.Key) + "]" + typeText(expr.Value)
	}
	return fmt.Sprintf("%T", expr)
}

// moduleConfigSection 是安装器写进配置的段：清单的 config 标了 example 的键。没有时为空串。
func moduleConfigSection(spec ModuleSpec) string {
	return spec.ConfigSchema.StarterYAML(nil)
}

// moduleConfigHost 是一个模块的配置装进的服务：模块入口所在的进程（moduleRuntimeHost），没有入口时是它的业务服务。
// 托管服务找不到（工程没托管那个框架服务）时返回空串，generateModuleServices 报同一个错。
func moduleConfigHost(m Manifest, module InstalledModule) string {
	return moduleRuntimeHost(m, module)
}

// businessModuleConfigs 是装在业务服务 service 上、声明了配置的模块，按名字排序：bootstrap 给这个服务加一个只带配置的
// modulehost Mod。托管服务上的模块配置跟着它的入口走（hostedModuleRuntimes），不在这里。
func businessModuleConfigs(m Manifest, service string) []string {
	if m.isFrameworkService(service) {
		return nil
	}
	var names []string
	for _, name := range sortedModuleRecordNames(m) {
		module := m.Modules[name]
		if module.Config != nil && moduleConfigHost(m, module) == service {
			names = append(names, name)
		}
	}
	return names
}

// moduleConfigImportAlias 是 bootstrap 里配置包的别名：与入口同一个包时沿用入口的别名（<模块>Module），否则 <模块>Config。
func moduleConfigImportAlias(module InstalledModule, name string) string {
	if module.Runtime != nil && module.Runtime.Package == module.Config.Package {
		return name + "Module"
	}
	return name + "Config"
}

// renderModuleConfigOf 是 bootstrap 里一个模块的配置声明：kitmodulehost.ConfigOf[<包>.<Type>](<包>.<Use> 或 nil)。
func renderModuleConfigOf(module InstalledModule, name string) string {
	alias := moduleConfigImportAlias(module, name)
	use := "nil"
	if module.Config.Use != "" {
		use = alias + "." + module.Config.Use
	}
	return fmt.Sprintf("kitmodulehost.ConfigOf[%s.%s](%s)", alias, module.Config.Type, use)
}

// moduleSpecConfigHost 是还没装的模块 spec 的配置要装进的服务：入口在托管服务上时是托管它的服务，否则是业务服务 service。
// 模块没有配置、或托管服务找不到时为空串（找不到时安装的生成一步报错）。
func moduleSpecConfigHost(m Manifest, service string, spec ModuleSpec) string {
	if spec.Config == nil {
		return ""
	}
	return moduleConfigHost(m, InstalledModule{Service: service, Runtime: spec.Runtime})
}

// moduleConfigKeyNames 是 spec 的配置声明的键名（plan 输出用），不含段。
func moduleConfigKeyNames(spec ModuleSpec) []string {
	var names []string
	for _, key := range spec.ConfigSchema.Keys {
		if key.Kind != configschema.KindSection {
			names = append(names, key.Name)
		}
	}
	return names
}

// moduleConfigTouchedFiles 是安装 spec 时要写配置段的文件（工程内相对路径），供安装事务备份。
func moduleConfigTouchedFiles(host string) []string {
	return []string{moduleServiceConfig(host), "configs/service/config." + host + ".prod.example.yaml", kubernetesSecretExampleRel(host)}
}

// writeModuleConfigSection 把模块配置里标了 example 的键追加到 host 的开发配置、生产示例与 k8s Secret 示例：
// 已经有同名顶层段的文件不动（业务改过的段照样算有），文件不存在的跳过。在 AddModules 的回滚边界内调用。
func writeModuleConfigSection(root, host string, spec ModuleSpec, warn io.Writer) error {
	section := moduleConfigSection(spec)
	if section == "" || host == "" {
		return nil
	}
	for _, target := range []struct {
		rel        string
		production bool
	}{
		{moduleServiceConfig(host), false},
		{"configs/service/config." + host + ".prod.example.yaml", true},
	} {
		path := filepath.Join(root, filepath.FromSlash(target.rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		text, crlf := lfText(raw)
		body, appended := appendMissingConfigBlocks(text, []string{section}, target.production)
		if !appended {
			continue
		}
		if err := writeAtomic(path, restoreLineEndings(body, crlf), 0o644); err != nil {
			return err
		}
	}
	_, err := editKubernetesSecretExampleConfig(root, host, warn, func(config string) (string, error) {
		body, _ := appendMissingConfigBlocks(config, []string{section}, true)
		return body, nil
	})
	return err
}
