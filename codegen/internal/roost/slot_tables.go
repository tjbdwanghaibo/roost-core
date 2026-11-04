package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// 按槽位路由的工程 rpc（//roost:rpc shard=，联盟服务方案 §2.7）在 doctor 里的一致性检查（rpc-slots:<rpc>）。
//
// 生成的 ClientMod 读 <service_type>.slots / .shards（框架的 servicerpc.SlotTableConfig，必填，加载时校验区间表），
// 拥有者的服务骨架读同一个键判断新建实体的槽位。槽位数编码在 ID 里、全部进程必须相同；区间表不同的两个进程会把
// 同一个槽位路由到不同实例。slots 是整数，用不了配置声明的 shared tag，所以在这里按配置文件核对：
//   - 调用方（services.<name>.uses_rpcs 列了它）的配置必须写这两个键，没写时进程启动即被拒，这里先报出并给出写法；
//   - 拥有者可以不写（骨架用到时再写）；写了的服务之间两个键必须逐值相同。
// 开发配置与生产示例各自成组比较（与 config-across-services 同一组文件），缺文件的服务不比较。

var shardMarkerPattern = regexp.MustCompile(`(?m)^\s*//roost:rpc\b[^\n]*\bshard=`)
var serviceTypePattern = regexp.MustCompile(`//roost:rpc\b[^\n]*\bservice_type=([A-Za-z0-9_.-]+)`)

// shardedRPCs 返回源码里有 shard= 方法的工程 rpc 及其 service_type（配置节名）。
func shardedRPCs(root string, m Manifest) map[string]string {
	out := map[string]string{}
	for _, dir := range projectRPCDirs(root, m) {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			continue
		}
		sharded, serviceType := false, ""
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_gen.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name))
			if err != nil {
				continue
			}
			sharded = sharded || shardMarkerPattern.Match(raw)
			if match := serviceTypePattern.FindSubmatch(raw); match != nil {
				serviceType = string(match[1])
			}
		}
		if sharded {
			rpc := filepath.Base(dir)
			if serviceType == "" {
				serviceType = rpc
			}
			out[rpc] = serviceType
		}
	}
	return out
}

func checkSlotTables(root string, m Manifest) []CheckItem {
	var items []CheckItem
	sharded := shardedRPCs(root, m)
	rpcs := make([]string, 0, len(sharded))
	for rpc := range sharded {
		rpcs = append(rpcs, rpc)
	}
	sort.Strings(rpcs)
	for _, rpc := range rpcs {
		section := sharded[rpc]
		var problems []string
		for _, set := range []configFileSet{devConfigSet(m), prodExampleConfigSet(m)} {
			problems = append(problems, slotTableProblems(root, m, rpc, section, set)...)
		}
		name := "rpc-slots:" + rpc
		if len(problems) > 0 {
			items = append(items, CheckItem{Name: name, Status: StatusFail, Detail: strings.Join(problems, "; ")})
			continue
		}
		items = append(items, CheckItem{Name: name, Status: StatusOK, Detail: section + ".slots / " + section + ".shards agree across the services that call or own it"})
	}
	return items
}

func slotTableProblems(root string, m Manifest, rpc, section string, set configFileSet) []string {
	type table struct{ slots, shards string }
	tables := map[string]table{}
	var problems []string
	for _, service := range sortedServiceNames(m) {
		caller := contains(m.Services[service].UsesRpcs, rpc)
		owner := contains(m.Services[service].Rpcs, rpc)
		if !caller && !owner {
			continue
		}
		rel := set.files[service]
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		node, _ := doc[section].(map[string]any)
		slots, hasSlots := node["slots"]
		shards, hasShards := node["shards"]
		if !hasSlots || !hasShards {
			if caller {
				problems = append(problems, fmt.Sprintf("%s calls %s (shard= methods) but lacks %s.slots / %s.shards; add:\n%s:\n  slots: 256\n  shards: [\"<owner sid>:0-255\"]",
					rel, rpc, section, section, section))
			}
			continue
		}
		tables[service] = table{slots: fmt.Sprint(slots), shards: fmt.Sprint(shards)}
	}
	var first string
	for _, service := range sortedServiceNames(m) {
		current, ok := tables[service]
		if !ok {
			continue
		}
		if first == "" {
			first = service
			continue
		}
		reference := tables[first]
		if current.slots != reference.slots {
			problems = append(problems, fmt.Sprintf("%s: %s.slots is %s in %s but %s in %s; every process must use the same slot count",
				set.label, section, current.slots, set.files[service], reference.slots, set.files[first]))
		}
		if current.shards != reference.shards {
			problems = append(problems, fmt.Sprintf("%s: %s.shards is %s in %s but %s in %s; a stale table routes slots to the wrong instance",
				set.label, section, current.shards, set.files[service], reference.shards, set.files[first]))
		}
	}
	return problems
}
