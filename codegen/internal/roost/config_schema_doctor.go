package roost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"gopkg.in/yaml.v3"
)

// 维护者决定 A4 ①：doctor 按 Mod 的配置声明检查工程里的服务配置（开发配置、生产示例、k8s Secret 示例里的
// config.yaml）。声明是 kit Mod 自己的（kitconfig_gen.go 快照）与生成进工程的 player TCP 接入层的
// （playerTCPDeclaration）；每个服务按生成 bootstrap 给它注册的 Mod 合并（serviceConfigSchema）。
//
//   - 失败：值的类型 / 范围 / 枚举不对、必填的键没写、生产示例里的密钥为空或 dev- 开头；框架段（任一声明用到的
//     顶层段）里出现没有任何声明的键——多半是拼错了，写了也没有任何代码读。
//   - 警告：键有声明，但这个服务没有注册读它的 Mod（写给别的服务的段，或业务代码自己读的框架键）。
//
// 业务 Mod 的声明只有编译后的进程知道：真实的生产配置用 `<bin> <service> --check-config` 检查（App 启动前的同一套检查）。

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

// checkConfigDeclarations 是 doctor 的 config-schema:<service> 检查。
func checkConfigDeclarations(root string, m Manifest) []CheckItem {
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
			bad, unread, err := checkServiceConfigText(raw, file.secret, file.production, schema, all)
			if err != nil {
				failures = append(failures, file.rel+": "+err.Error())
				continue
			}
			checked = append(checked, file.rel)
			for _, problem := range bad {
				failures = append(failures, file.rel+": "+problem)
			}
			if len(unread) > 0 {
				warnings = append(warnings, fmt.Sprintf("%s: no mod of %s declares %s (another service's keys, or read by business code)", file.rel, service, strings.Join(unread, ", ")))
			}
		}
		switch {
		case len(failures) > 0:
			item.Status, item.Detail = StatusFail, strings.Join(failures, "; ")
		case len(warnings) > 0:
			item.Status, item.Detail = StatusWarn, strings.Join(warnings, "; ")
		case len(checked) == 0:
			item.Detail = "no config file"
		default:
			item.Detail = fmt.Sprintf("%d file(s) match the mods' declarations", len(checked))
		}
		items = append(items, item)
	}
	return items
}

// checkServiceConfigText 检查一份配置：bad 是失败项（值不合声明、框架段里没有声明的键），unread 是有声明但本服务
// 不读的键。secret 为 true 时 raw 是 k8s Secret 示例，检查它 stringData 里的 config.yaml。
func checkServiceConfigText(raw []byte, secret, production bool, schema, all configschema.Schema) (bad, unread []string, err error) {
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
	sections := all.Sections()
	undeclared := map[string]bool{}
	for _, key := range all.Undeclared(source, sections) {
		undeclared[key] = true
		bad = append(bad, fmt.Sprintf("config: %s is not a key any framework mod declares (misspelled? nothing would read it)", key))
	}
	for _, key := range schema.Undeclared(source, sections) {
		if !undeclared[key] {
			unread = append(unread, key)
		}
	}
	return bad, unread, nil
}
