package roost

import (
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"gopkg.in/yaml.v3"
)

// 同一工程各服务之间的配置一致性（维护者 2026-10-19 批准）。单个进程的 --check-config 只看得到自己的配置，
// “各服务取同一个值”“同一台机器上端口不冲突”只能在这里查。规则不在这里列举，而是从配置声明的 tag 读出
// （configschema.Key.Shared / Follows / Listen）：新增共享资源名或监听地址时在 Mod 的声明上加 tag，
// config check --all 与 doctor 的 config-across-services 自动带上。
//
//   - 共享资源名：资源名（shared tag）相同的键在全部服务里必须取同一个值。取值按进程读到的算：写了取写的，
//     没写取声明的缺省值，再没有就按 follows 取另一个键的值（例如 saga.start_effect_stream 不写取
//     dataengine.effects.stream），都没有就是空串。按写法比较（只去掉首尾空白）。
//   - 监听地址：listen tag 的开关键为 true 的键是这个服务真正监听的地址。开发配置集（configs/service/config.<service>.yaml）
//     的服务在同一台机器上一起运行（deploy/dev/run.sh），端口相同且主机重叠（相同，或任一方是 0.0.0.0 / :: / 空）
//     即冲突；同一服务里的两个监听地址同样不能冲突。端口 0 由系统分配，不比较。生产示例每个服务一个容器，不比较端口。
//   - 同一主机上各不相同的值：unique tag 组名相同的键，开发配置集里各服务的取值必须两两不同（例如 etcd.advertise_addr，
//     写进服务注册、别的进程据此连接本服务的地址）。没写也没有缺省值的不比较；生产示例每个服务一个容器，不比较。
//
// 只用生成器认识的框架声明（serviceConfigSchema）：不需要编译工程，--keys-only 照样能查；业务自己声明的键不在其中。

// configFileSet 是一组一起部署的服务配置：每个服务一份文件（相对工程根），ports 表示它们在同一台机器上运行。
type configFileSet struct {
	label string
	files map[string]string // service → 相对路径
	ports bool
}

func devConfigSet(m Manifest) configFileSet {
	set := configFileSet{label: "development configs", files: map[string]string{}, ports: true}
	for _, service := range sortedServiceNames(m) {
		set.files[service] = "configs/service/config." + service + ".yaml"
	}
	return set
}

func prodExampleConfigSet(m Manifest) configFileSet {
	set := configFileSet{label: "production examples", files: map[string]string{}}
	for _, service := range sortedServiceNames(m) {
		set.files[service] = "configs/service/config." + service + ".prod.example.yaml"
	}
	return set
}

// serviceConfigDocument 是一个服务的一份配置与它的框架声明。
type serviceConfigDocument struct {
	service, rel string
	schema       configschema.Schema
	source       configschema.Source
}

// effectiveValue 是进程读到的取值与它的来源说明（错误信息里给人看）。
func (d serviceConfigDocument) effectiveValue(key configschema.Key, all configschema.Schema) (string, string) {
	if raw, ok := d.source.Get(key.Name); ok {
		value, err := configschema.ParseString(key.Name, raw)
		if err != nil {
			value = fmt.Sprint(raw)
		}
		return strings.TrimSpace(value), key.Name
	}
	if key.Default != "" {
		return key.Default, key.Name + ", default"
	}
	if key.Follows != "" {
		target, ok := d.schema.Lookup(key.Follows)
		if !ok {
			target, ok = all.Lookup(key.Follows)
		}
		if ok {
			value, from := d.effectiveValue(target, all)
			return value, key.Name + " unset, follows " + from
		}
	}
	return "", key.Name + ", not written"
}

// enabled 报告监听地址的开关是否打开（listen:"always" 总是打开）。
func (d serviceConfigDocument) enabled(key configschema.Key, all configschema.Schema) bool {
	if key.Listen == "always" {
		return true
	}
	toggle, ok := d.schema.Lookup(key.Listen)
	if !ok {
		toggle, ok = all.Lookup(key.Listen)
	}
	raw, written := d.source.Get(key.Listen)
	if !written {
		if !ok || toggle.Default == "" {
			return false
		}
		raw = toggle.Default
	}
	on, err := configschema.ParseBool(key.Listen, raw)
	return err == nil && on
}

// loadConfigSet 读一组配置：缺的文件、读不出或不是 YAML 的文件跳过（文件本身的问题由键检查报告）。
func loadConfigSet(root string, m Manifest, set configFileSet) []serviceConfigDocument {
	var docs []serviceConfigDocument
	for _, service := range sortedServiceNames(m) {
		rel, ok := set.files[service]
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		var document map[string]any
		if err := yaml.Unmarshal(raw, &document); err != nil {
			continue
		}
		schema, err := serviceConfigSchema(m, service)
		if err != nil {
			continue
		}
		docs = append(docs, serviceConfigDocument{service: service, rel: rel, schema: schema, source: configschema.NewMapSource(document)})
	}
	return docs
}

// checkConfigAcrossServices 返回一组配置里跨服务的不一致（每条一句，点名服务、键、取值与改法）与参与比较的服务数。
// warnings 是开发配置里写的按服务端口与 roost.yaml 端口块号派生的值不一致（配置归应用、可以手改，所以只提示）。
func checkConfigAcrossServices(root string, m Manifest, set configFileSet) (problems, warnings []string, compared int) {
	docs := loadConfigSet(root, m, set)
	all := allFrameworkConfigSchema()
	problems = append(problems, sharedValueProblems(docs, all)...)
	if set.ports {
		problems = append(problems, listenAddrProblems(docs, all)...)
		problems = append(problems, uniqueValueProblems(docs, all)...)
		warnings = portBlockWarnings(m, docs)
	}
	return problems, warnings, len(docs)
}

// portBlockWarnings 对照开发配置里写了的 ops.addr / etcd.advertise_addr 端口与端口块号派生的值（port_blocks.go）。
// 不一致不一定是错（应用可以有意换端口），但 deploy/dev/run.sh 按块号探测 ops 端口，second-game.sh 按块号改写，
// 所以提示改回或知情保留。
func portBlockWarnings(m Manifest, docs []serviceConfigDocument) []string {
	var warnings []string
	for _, doc := range docs {
		ports := portsOf(m, doc.service)
		for _, check := range []struct {
			key  string
			want int
		}{{"ops.addr", ports.Ops}, {"etcd.advertise_addr", ports.Advertise}} {
			raw, written := doc.source.Get(check.key)
			if !written {
				continue
			}
			_, port, err := net.SplitHostPort(strings.TrimSpace(fmt.Sprint(raw)))
			if err != nil || port == fmt.Sprint(check.want) {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("%s: %s port %s is not %d, the port services.%s.port_block %d gives it (deploy/dev/run.sh and second-game.sh follow the block)",
				doc.rel, check.key, port, check.want, doc.service, ports.Block))
		}
	}
	return warnings
}

type sharedValue struct {
	service, rel, value, from string
}

func sharedValueProblems(docs []serviceConfigDocument, all configschema.Schema) []string {
	groups := map[string][]sharedValue{}
	for _, doc := range docs {
		for _, key := range doc.schema.Keys {
			if key.Shared == "" {
				continue
			}
			value, from := doc.effectiveValue(key, all)
			groups[key.Shared] = append(groups[key.Shared], sharedValue{service: doc.service, rel: doc.rel, value: value, from: from})
		}
	}
	var problems []string
	for _, resource := range slices.Sorted(maps.Keys(groups)) {
		values := groups[resource]
		distinct := map[string]bool{}
		for _, v := range values {
			distinct[v.value] = true
		}
		if len(distinct) < 2 {
			continue
		}
		var parts []string
		for _, v := range values {
			parts = append(parts, fmt.Sprintf("%s %q (%s)", v.service, v.value, v.from))
		}
		problems = append(problems, fmt.Sprintf("%s differs between services: %s; every service of the project reads one shared %s, otherwise they would not see each other's messages / data (or would split business days at different moments): write the same value in each service's config",
			resource, strings.Join(parts, ", "), resource))
	}
	return problems
}

// uniqueValueProblems 报告 unique 组里取值相同的两个服务（同一主机上的配置集才比较）。
func uniqueValueProblems(docs []serviceConfigDocument, all configschema.Schema) []string {
	type owner struct{ service, key, rel string }
	seen := map[string]map[string]owner{} // 组名 → 取值 → 第一个写它的服务
	var problems []string
	for _, doc := range docs {
		for _, key := range doc.schema.Keys {
			if key.Unique == "" {
				continue
			}
			value, _ := doc.effectiveValue(key, all)
			if value == "" {
				continue
			}
			if seen[key.Unique] == nil {
				seen[key.Unique] = map[string]owner{}
			}
			if first, taken := seen[key.Unique][value]; taken {
				problems = append(problems, fmt.Sprintf("%s %q is used by two services on one host: %s %s (%s) and %s %s (%s); each service must have its own value (other processes would reach the wrong service): give one of them another port",
					key.Unique, value, first.service, first.key, first.rel, doc.service, key.Name, doc.rel))
				continue
			}
			seen[key.Unique][value] = owner{doc.service, key.Name, doc.rel}
		}
	}
	return problems
}

type listenAddr struct {
	service, rel, key, addr, host, port string
}

func listenAddrProblems(docs []serviceConfigDocument, all configschema.Schema) []string {
	var addrs []listenAddr
	var problems []string
	for _, doc := range docs {
		for _, key := range doc.schema.Keys {
			if key.Listen == "" || !doc.enabled(key, all) {
				continue
			}
			addr, _ := doc.effectiveValue(key, all)
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s %q is not host:port: %v", doc.rel, key.Name, addr, err))
				continue
			}
			if port == "0" {
				continue // 系统分配
			}
			addrs = append(addrs, listenAddr{service: doc.service, rel: doc.rel, key: key.Name, addr: addr, host: normalizeListenHost(host), port: port})
		}
	}
	for i := range addrs {
		for j := i + 1; j < len(addrs); j++ {
			a, b := addrs[i], addrs[j]
			if a.port != b.port || !listenHostsOverlap(a.host, b.host) {
				continue
			}
			problems = append(problems, fmt.Sprintf("port %s is listened on twice on one host: %s %s %q (%s) and %s %s %q (%s); the second process fails to start with \"address already in use\": give one of them another port",
				a.port, a.service, a.key, a.addr, a.rel, b.service, b.key, b.addr, b.rel))
		}
	}
	return problems
}

func normalizeListenHost(host string) string {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	switch {
	case strings.EqualFold(host, "localhost"):
		return "127.0.0.1"
	case host == "0.0.0.0" || host == "::":
		return ""
	}
	return host
}

func listenHostsOverlap(a, b string) bool { return a == "" || b == "" || a == b }

// checkConfigAcrossServicesItem 是 doctor 的 config-across-services 项：开发配置集查共享资源名与端口，生产示例只查共享资源名。
func checkConfigAcrossServicesItem(root string, m Manifest) CheckItem {
	item := CheckItem{Name: "config-across-services", Status: StatusOK}
	var problems, warnings []string
	var summary []string
	for _, set := range []configFileSet{devConfigSet(m), prodExampleConfigSet(m)} {
		found, warned, compared := checkConfigAcrossServices(root, m, set)
		for _, problem := range found {
			problems = append(problems, set.label+": "+problem)
		}
		warnings = append(warnings, warned...)
		summary = append(summary, fmt.Sprintf("%s: %d service(s)", set.label, compared))
	}
	switch {
	case len(problems) > 0:
		item.Status, item.Detail = StatusFail, strings.Join(append(problems, warnings...), "\n")
		return item
	case len(warnings) > 0:
		item.Status, item.Detail = StatusWarn, strings.Join(warnings, "\n")
		return item
	}
	item.Detail = "shared resource names agree; development listeners and per-service addresses do not collide (" + strings.Join(summary, "; ") +
		"); port blocks: " + strings.Join(sortedPortBlocks(m), ", ")
	return item
}
