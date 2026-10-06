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

// 维护者决定 A4 ①：doctor 按 Mod 的配置声明检查工程里的服务配置（开发配置、生产示例、k8s Secret 示例里的
// config.yaml）。声明有两个来源：
//
//   - 框架的：kit Mod 自己的（kitconfig_gen.go 快照）与生成进工程的 player TCP 接入层的（playerTCPDeclaration）；
//     每个服务按生成 bootstrap 给它注册的 Mod 合并（serviceConfigSchema）。不用编译工程。
//   - 业务的：业务服务（app.Service 实现 ConfigSchema）与业务 Mod 的声明只有编译后的进程知道。doctor 在工程里编译
//     一次进程、对每个服务运行 `<bin> <service> --print-config-schema` 读回它的全部声明（processConfigSchemas），
//     其中框架快照没有的键就是业务声明（A4 ① 收尾，2026-10-07）。工程编译不过时（compile:go-list 已 FAIL）只做
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
	var (
		once     sync.Once
		dir      string
		binary   string
		buildErr error
	)
	build := func() {
		dir, buildErr = os.MkdirTemp("", "roost-doctor-config-")
		if buildErr != nil {
			return
		}
		binary = filepath.Join(dir, "service")
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		buildCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		var output bytes.Buffer
		if err := runCommandTree(buildCtx, root, append(os.Environ(), "GOWORK=off"), &output, &output, "go", "build", "-buildvcs=false", "-mod=readonly", "-o", binary, "."); err != nil {
			buildErr = fmt.Errorf("go build: %v: %s", err, lastBytes(output.String(), 1024))
		}
	}
	source := func(service string) (configschema.Schema, error) {
		once.Do(build)
		if buildErr != nil {
			return configschema.Schema{}, buildErr
		}
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if err := runCommandTree(runCtx, root, append(os.Environ(), "GOWORK=off"), &stdout, &stderr, binary, service, "--print-config-schema"); err != nil {
			return configschema.Schema{}, fmt.Errorf("%s --print-config-schema: %v: %s", service, err, lastBytes(stderr.String(), 1024))
		}
		var keys []configschema.Key
		if err := json.Unmarshal(stdout.Bytes(), &keys); err != nil {
			return configschema.Schema{}, fmt.Errorf("%s --print-config-schema: not a key list: %w", service, err)
		}
		return configschema.Schema{Keys: keys}, nil
	}
	cleanup := func() {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
	return source, cleanup
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
	if access, ok := m.Access["player"]; ok && access.Service == service && contains(access.Transports, "tcp") {
		schemas = append(schemas, playerTCPSchema())
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
// 声明了却没有任何代码读的字段，运维改了它什么也不发生。两者都是 FAIL（A4 ① 收尾）。
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
