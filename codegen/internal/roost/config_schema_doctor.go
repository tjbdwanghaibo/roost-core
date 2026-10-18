package roost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"gopkg.in/yaml.v3"
)

// 维护者决定：doctor 按 Mod 的配置声明检查工程里的服务配置（开发配置、生产示例、k8s Secret 示例里的
// config.yaml）。声明有两个来源：
//
//   - 框架的：wiring Mod 自己的（kitconfig_gen.go 快照）与生成进工程的 player TCP 接入层的（playerTCPDeclaration）；
//     每个服务按生成 bootstrap 给它注册的 Mod 合并（serviceConfigSchema）。不用编译工程。
//   - 业务的：业务服务（app.Service 实现 ConfigSchema）与业务 Mod 的声明只有编译后的进程知道。doctor 在工程里编译
//     一次进程、对每个服务运行 `<bin> <service> --print-config-schema` 读回它的全部声明（processConfigSchemas），
//     其中框架快照没有的键就是业务声明（维护者决定，2026-10-07）。工程编译不过时（compile:go-list 已 FAIL）只做
//     框架那一半。
//
//   - 失败：值的类型 / 范围 / 枚举不对、必填的键没写、生产示例里的密钥为空或 dev- 开头（框架与业务声明都查）；
//     框架段（任一框架声明用到的顶层段）或业务段里出现没有任何声明的键——多半是拼错了，写了也没有任何代码读。
//   - 警告：键有框架声明，但这个服务的进程里没有任何 Mod 或服务本身声明它（写给别的服务的段）。业务代码读的键
//     由业务服务声明之后不再警告；以前新生成的 game-demo 的 game 因 activity.* / platform.* 有一行这样的警告。

// serviceSchemaSource 返回一个服务的进程的全部声明（App、全部 Mod 与服务本身），doctor 用它拿到业务声明。
type serviceSchemaSource func(service string) (configschema.Schema, error)

// processConfigSchemas 在 root 里编译一次工程的进程（go build，与 compile:go-list 同样 -mod=readonly），之后每次
// 调用对一个服务运行 `--print-config-schema` 并解析它打印的 JSON 键表。返回的 cleanup 删掉临时二进制。
func processConfigSchemas(ctx context.Context, root string) (serviceSchemaSource, func()) {
	process, cleanup := newProjectProcess(ctx, root)
	return process.schema, cleanup
}

// projectProcess 是 doctor 在工程里编译的进程：第一次使用时编译一次，之后读声明（--print-config-schema）与
// 按全部 Mod 的声明和跨 Mod 规则检查配置（--check-config）共用这一个二进制。
type projectProcess struct {
	ctx      context.Context
	root     string
	once     sync.Once
	dir      string
	binary   string
	buildErr error
}

func newProjectProcess(ctx context.Context, root string) (*projectProcess, func()) {
	process := &projectProcess{ctx: ctx, root: root}
	cleanup := func() {
		if process.dir != "" {
			_ = os.RemoveAll(process.dir)
		}
	}
	return process, cleanup
}

func (p *projectProcess) build() error {
	p.once.Do(func() {
		p.dir, p.buildErr = os.MkdirTemp("", "roost-doctor-config-")
		if p.buildErr != nil {
			return
		}
		p.binary = filepath.Join(p.dir, "service")
		if runtime.GOOS == "windows" {
			p.binary += ".exe"
		}
		buildCtx, cancel := context.WithTimeout(p.ctx, 5*time.Minute)
		defer cancel()
		var output bytes.Buffer
		if err := runCommandTree(buildCtx, p.root, append(os.Environ(), "GOWORK=off"), &output, &output, "go", "build", "-buildvcs=false", "-mod=readonly", "-o", p.binary, "."); err != nil {
			p.buildErr = fmt.Errorf("go build: %v: %s", err, lastBytes(output.String(), 1024))
		}
	})
	return p.buildErr
}

func (p *projectProcess) schema(service string) (configschema.Schema, error) {
	if err := p.build(); err != nil {
		return configschema.Schema{}, err
	}
	runCtx, cancel := context.WithTimeout(p.ctx, time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if err := runCommandTree(runCtx, p.root, append(os.Environ(), "GOWORK=off"), &stdout, &stderr, p.binary, service, "--print-config-schema"); err != nil {
		return configschema.Schema{}, fmt.Errorf("%s --print-config-schema: %v: %s", service, err, lastBytes(stderr.String(), 1024))
	}
	var keys []configschema.Key
	if err := json.Unmarshal(stdout.Bytes(), &keys); err != nil {
		return configschema.Schema{}, fmt.Errorf("%s --print-config-schema: not a key list: %w", service, err)
	}
	return configschema.Schema{Keys: keys}, nil
}

// checkConfig 运行 `<bin> <service> --check-config --config <rel>`：与进程启动前同一处检查，除了键的声明，还有
// 各 Mod 配置结构体的跨键规则（ValidateConfig）与跨 Mod 的一致性规则（app.ModConfigCrossChecker，例如 saga 的效果流
// 必须就是同进程 DataEngine 的那条）。这些规则只在编译后的进程里，快照查不到。
func (p *projectProcess) checkConfig(service, rel string) error {
	if err := p.build(); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(p.ctx, time.Minute)
	defer cancel()
	var output bytes.Buffer
	if err := runCommandTree(runCtx, p.root, append(os.Environ(), "GOWORK=off"), &output, &output, p.binary, service, "--check-config", "--config", rel); err != nil {
		return fmt.Errorf("%s", lastBytes(output.String(), 2048))
	}
	return nil
}

// checkConfigRules 是 doctor 的 config-check 项：每个服务的开发配置交给编译后的进程 --check-config。生产示例与
// Secret 示例带占位值，按生产规则检查必然失败，不在这里跑；发布前对真实生产配置运行同一条命令。工程编译不过时不报这一项
// （compile:go-list 已 FAIL）。
func checkConfigRules(root string, m Manifest, process *projectProcess) []CheckItem {
	if process == nil {
		return nil
	}
	var items []CheckItem
	for _, service := range sortedServiceNames(m) {
		rel := filepath.ToSlash(filepath.Join("configs", "service", "config."+service+".yaml"))
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			continue
		}
		item := CheckItem{Name: "config-check:" + service, Status: StatusOK, Detail: rel + " passes the process's --check-config (declarations, cross-key and cross-mod rules)"}
		if err := process.checkConfig(service, rel); err != nil {
			item.Status, item.Detail = StatusFail, fmt.Sprintf("%s: %v; fix: the process refuses to start with this config; change the keys named above (same check as: go run . %s --check-config --config %s)", rel, err, service, rel)
		}
		items = append(items, item)
	}
	return items
}

func lastBytes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) > limit {
		return text[len(text)-limit:]
	}
	return text
}

// businessDeclarations 是进程的声明里框架快照（schema）没有的键：业务服务与业务 Mod 声明的键。
func businessDeclarations(process, schema configschema.Schema) configschema.Schema {
	var business configschema.Schema
	for _, key := range process.Keys {
		if _, framework := schema.Lookup(key.Name); !framework {
			business.Keys = append(business.Keys, key)
		}
	}
	return business
}

// serviceConfigSchema 是一个服务的进程会注册的全部 Mod 的声明，与 renderBootstrap / serviceModConstructors 同一个解析。
func serviceConfigSchema(m Manifest, service string) (configschema.Schema, error) {
	groups := []string{"app"}
	shared, _ := resolveMods(m.SharedMods)
	own, _ := resolveMods(effectiveServiceMods(m, service))
	groups = append(groups, shared...)
	groups = append(groups, own...)
	spec := m.Services[service]
	if framework := strings.TrimSpace(spec.Framework); framework != "" {
		groups = append(groups, framework)
	}
	for _, target := range spec.Uses {
		groups = append(groups, m.Services[target].Framework+".client")
	}
	if serviceSingletonEnabled(m, service) {
		groups = append(groups, "redis") // 单实例锁按 redis.* 连接（kitredis.SingletonStore）
	}
	schemas := make([]configschema.Schema, 0, len(groups)+1)
	for _, group := range groups {
		if schema, ok := kitConfigSchemas[group]; ok {
			schemas = append(schemas, schema)
		}
	}
	if access, ok := m.Access["player"]; ok && access.Service == service {
		schemas = append(schemas, playerAccessSchema())
		if contains(access.Transports, "tcp") {
			schemas = append(schemas, playerTCPSchema())
		}
	}
	return configschema.Merge(schemas...)
}

// allFrameworkConfigSchema 是生成器认识的全部声明的并集：判断一个键是“拼错了 / 没有读取方”还是“属于别的服务”。
func allFrameworkConfigSchema() configschema.Schema {
	var all configschema.Schema
	seen := map[string]bool{}
	names := make([]string, 0, len(kitConfigSchemas))
	for name := range kitConfigSchemas {
		names = append(names, name)
	}
	sort.Strings(names)
	add := func(schema configschema.Schema) {
		for _, key := range schema.Keys {
			if !seen[key.Name] {
				seen[key.Name] = true
				all.Keys = append(all.Keys, key)
			}
		}
	}
	for _, name := range names {
		add(kitConfigSchemas[name])
	}
	add(playerTCPSchema())
	add(playerAccessSchema())
	return all
}

// serviceConfigFiles 是一个服务的三份配置：路径、是否生产、是否 k8s Secret 示例。
func serviceConfigFiles(service string) []struct {
	rel                string
	production, secret bool
} {
	return []struct {
		rel                string
		production, secret bool
	}{
		{"configs/service/config." + service + ".yaml", false, false},
		{"configs/service/config." + service + ".prod.example.yaml", true, false},
		{"deploy/k8s/base/secret." + service + ".example.yaml", true, true},
	}
}

// checkConfigDeclarations 是 doctor 的 config-schema:<service> 检查。process 为 nil 时只按框架声明检查
// （工程编译不过、或调用方只要静态检查）。
func checkConfigDeclarations(root string, m Manifest, process serviceSchemaSource) []CheckItem {
	all := allFrameworkConfigSchema()
	var items []CheckItem
	for _, service := range sortedServiceNames(m) {
		item := CheckItem{Name: "config-schema:" + service, Status: StatusOK}
		schema, err := serviceConfigSchema(m, service)
		if err != nil {
			item.Status, item.Detail = StatusFail, err.Error()
			items = append(items, item)
			continue
		}
		var business configschema.Schema
		if process != nil {
			declared, err := process(service)
			if err != nil {
				item.Status, item.Detail = StatusFail, "cannot read the process's declarations, so the keys business code declares are not checked: "+err.Error()
				items = append(items, item)
				continue
			}
			business = businessDeclarations(declared, schema)
		}
		var failures, warnings, checked []string
		for _, file := range serviceConfigFiles(service) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.rel)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				failures = append(failures, file.rel+": "+err.Error())
				continue
			}
			bad, unread, err := checkServiceConfigText(raw, file.secret, file.production, schema, business, all)
			if err != nil {
				failures = append(failures, file.rel+": "+err.Error())
				continue
			}
			checked = append(checked, file.rel)
			for _, problem := range bad {
				failures = append(failures, file.rel+": "+problem)
			}
			if len(unread) > 0 {
				warnings = append(warnings, fmt.Sprintf("%s: no mod of %s declares %s (another service's keys; keys this service's own code reads are declared by the service, see app.ModConfigSchema)", file.rel, service, strings.Join(unread, ", ")))
			}
		}
		switch {
		case len(failures) > 0:
			item.Status, item.Detail = StatusFail, strings.Join(failures, "; ")
		case len(warnings) > 0:
			item.Status, item.Detail = StatusWarn, strings.Join(warnings, "; ")
		case len(checked) == 0:
			item.Detail = "no config file"
		case process == nil:
			item.Detail = fmt.Sprintf("%d file(s) match the framework mods' declarations (business declarations not read: the project does not compile)", len(checked))
		default:
			item.Detail = fmt.Sprintf("%d file(s) match the declarations of every mod and the service (%d business key(s))", len(checked), len(business.Keys))
		}
		items = append(items, item)
	}
	return items
}

// checkConfigSections 是 doctor 的 config-sections:<service> 检查（维护者 2026-10-17 决定 B）：只读，按服务当前
// 实际注册的 Mod（serviceConfigMods，与生成器同一份计算）找三份配置（开发配置、生产示例、k8s Secret 示例的
// config.yaml，缺的文件跳过）里缺的配置段，给出可直接追加的文本。
//
// 为什么需要：服务配置在建工程时生成一次，之后归应用所有；只有 add mod / add saga / add access / add rpc 在
// “这条命令让服务多出 Mod”的那一刻追加配置段（appendModConfigSections）。手改 roost.yaml 加 Mod、或修复前
// 走过 add 的工程，project sync 不补段，Mod 就按代码默认值运行（默认地址、默认流大小），config-schema 检查
// 只查写了的键与必填键，看不出整段没写。
//
//   - 缺段是 WARN，不让 doctor 失败：业务可能有意删掉一段、按代码默认值运行。要消掉提示，留下空段（例如
//     `saga: {}`），Mod 照样用代码默认值。没有另外的豁免机制。
//   - 段“有没有”只看行首的 `<key>:`，与 add 追加时的判断相同（missingConfigBlocks）；段里的内容不比较。
//   - 带 DataEngine 的服务的 singleton.enabled 不是 true 也是 WARN：add 让服务带上 DataEngine 时会打开它，
//     手改 roost.yaml 时没人打开，单实例锁静默不生效。
//
// 不改写任何文件；project sync 的行为不变。
func checkConfigSections(root string, m Manifest) []CheckItem {
	var items []CheckItem
	for _, service := range sortedServiceNames(m) {
		item := CheckItem{Name: "config-sections:" + service, Status: StatusOK}
		sections := serviceConfigSections(m, service)
		singleton := serviceSingletonEnabled(m, service)
		var problems []string
		checked := 0
		for _, file := range serviceConfigFiles(service) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.rel)))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				problems = append(problems, file.rel+": "+err.Error())
				continue
			}
			body, _ := lfText(raw)
			where := file.rel
			if file.secret {
				config, _, _, ok := kubernetesSecretEmbeddedConfig(body)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s: no single `config.yaml: |` block under stringData, not checked (it mirrors configs/service/config.%s.prod.example.yaml)", file.rel, service))
					continue
				}
				body, where = config, file.rel+" (stringData config.yaml)"
			}
			checked++
			if body != "" && !strings.HasSuffix(body, "\n") {
				body += "\n"
			}
			keys, text := missingConfigBlocks(body, sections, file.production)
			if len(keys) > 0 {
				if file.secret {
					text = indentText(text, "    ")
				}
				problems = append(problems, fmt.Sprintf("%s lacks %s, generated for the mods of %s (without it they run on their code defaults). Unless removed on purpose, append this to the end of %s; to stay on the defaults and silence this, keep an empty block such as `%s: {}`:\n%s",
					file.rel, strings.Join(keys, ":, ")+":", service, where, keys[0], strings.TrimSuffix(text, "\n")))
			}
			if singleton && !slices.Contains(keys, "singleton") {
				if _, generatedOff, edited := turnGeneratedSingletonOn(body, m.Project.Name, file.production); generatedOff || edited {
					problems = append(problems, fmt.Sprintf("%s: service %s runs the Data Engine (one WAL writer per sid) but singleton.enabled is not true; set it to true in %s", file.rel, service, where))
				}
			}
		}
		switch {
		case len(problems) > 0:
			item.Status, item.Detail = StatusWarn, strings.Join(problems, "\n")
		case checked == 0:
			item.Detail = "no config file"
		default:
			item.Detail = fmt.Sprintf("%d file(s) have a block for every mod of %s", checked, service)
		}
		items = append(items, item)
	}
	return items
}

// checkConfigFileDeclarations 是 `roost config check` 的声明检查，与 doctor 的 config-schema 用同一份框架声明：
// 值不合声明、框架段里没有任何声明的键（多半拼错）都是失败。先只按框架声明静态检查，不编译工程；只有发现
// 框架段里的未声明键时才向 process 要这个服务的进程声明复核——业务服务自己声明、写在框架段里的键不算拼错。
// process 失败（工程编译不过等）时照样报出静态发现，并写明业务声明没读到。业务独有段（框架没有的顶层段）里的
// 拼写只有读到进程声明才查得出，那是 doctor 的范围。
func checkConfigFileDeclarations(path string, production bool, m Manifest, service string, process serviceSchemaSource) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	schema, err := serviceConfigSchema(m, service)
	if err != nil {
		return nil, err
	}
	all := allFrameworkConfigSchema()
	bad, _, err := checkServiceConfigText(raw, false, production, schema, configschema.Schema{}, all)
	if err != nil || len(bad) == 0 || process == nil {
		return bad, err
	}
	declared, err := process(service)
	if err != nil {
		return append(bad, "the service's own declarations were not read, so keys it declares may be reported above: "+err.Error()), nil
	}
	bad, _, err = checkServiceConfigText(raw, false, production, schema, businessDeclarations(declared, schema), all)
	return bad, err
}

// checkServiceConfigText 检查一份配置：bad 是失败项（值不合框架或业务声明、框架段或业务段里没有声明的键），unread 是
// 有框架声明、但本服务的进程里没有任何声明的键。business 是本服务的业务声明（可以为空）。secret 为 true 时 raw 是
// k8s Secret 示例，检查它 stringData 里的 config.yaml。
func checkServiceConfigText(raw []byte, secret, production bool, schema, business, all configschema.Schema) (bad, unread []string, err error) {
	if secret {
		var document struct {
			StringData map[string]string `yaml:"stringData"`
		}
		if err := yaml.Unmarshal(raw, &document); err != nil {
			return nil, nil, fmt.Errorf("does not parse: %w", err)
		}
		raw = []byte(document.StringData["config.yaml"])
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, nil, fmt.Errorf("does not parse: %w", err)
	}
	source := configschema.NewMapSource(document)
	for _, problem := range schema.Check(source, production) {
		bad = append(bad, problem.Error())
	}
	for _, problem := range business.Check(source, production) {
		bad = append(bad, problem.Error())
	}
	sections := all.Sections()
	notBusiness := business.Undeclared(source, sections) // 框架段里业务也没有声明的键
	undeclared := map[string]bool{}
	for _, key := range all.Undeclared(source, sections) {
		if !slices.Contains(notBusiness, key) {
			continue // 框架段里由业务声明的键
		}
		undeclared[key] = true
		bad = append(bad, fmt.Sprintf("config: %s is not a key any framework mod declares (misspelled? nothing would read it)", key))
	}
	// 只有业务声明用到的段（框架没有的顶层段）：同样不许有没声明的键。
	var businessOnly []string
	for _, section := range business.Sections() {
		if !slices.Contains(sections, section) {
			businessOnly = append(businessOnly, section)
		}
	}
	for _, key := range business.Undeclared(source, businessOnly) {
		bad = append(bad, fmt.Sprintf("config: %s is not a key this service declares (misspelled? nothing would read it)", key))
	}
	for _, key := range schema.Undeclared(source, sections) {
		if !undeclared[key] && slices.Contains(notBusiness, key) {
			unread = append(unread, key)
		}
	}
	return bad, unread, nil
}

// checkConfigReads 是 doctor 的 config-reads 检查：工程里的代码读配置只经声明（与框架的守卫同一份实现，
// internal/configschema/guard.go）。直接读 viper 的键不在任何声明里，App 启动检查与上面的 config-schema 都看不到它；
// 声明了却没有任何代码读的字段，运维改了它什么也不发生。两者都是 FAIL。
func checkConfigReads(root string) CheckItem {
	item := CheckItem{Name: "config-reads", Status: StatusOK}
	packages, err := configschema.GoPackages(root)
	if err != nil {
		item.Status, item.Detail = StatusFail, err.Error()
		return item
	}
	reads, err := configschema.UndeclaredReads(packages, nil)
	if err != nil {
		item.Status, item.Detail = StatusFail, err.Error()
		return item
	}
	// 声明常集中在一个包、由别的包读（game-demo 的 game/settings），所以“没读”按整个工程算。
	var files []string
	for _, group := range packages {
		files = append(files, group...)
	}
	unread, err := configschema.UnreadFields(map[string][]string{root: files})
	if err != nil {
		item.Status, item.Detail = StatusFail, err.Error()
		return item
	}
	var problems []string
	for _, read := range reads {
		problems = append(problems, relativeTo(root, read)+": reads config outside a declaration; declare the key on the service or mod (ConfigSchema) and read it with app.LoadConfig")
	}
	for _, field := range unread {
		problems = append(problems, relativeTo(root, field)+": declares a config key nothing in the project reads")
	}
	if len(problems) > 0 {
		item.Status, item.Detail = StatusFail, strings.Join(problems, "; ")
		return item
	}
	item.Detail = fmt.Sprintf("%d package(s) read config only through declarations", len(packages))
	return item
}

func relativeTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}
