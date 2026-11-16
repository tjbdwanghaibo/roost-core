package roost

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// checkSingletonEtcd 是 doctor 的 singleton:etcd-consistent 检查（2026-11-16 单实例锁改存 etcd）。
//
// 单实例锁与业务时间高水位存在 etcd（kitetcd.SingletonStore），而 Live、Gate 与 cellworldhost 的 IdentityChecker 要读
// 别的进程的锁键，只会去自己配置的 etcd 与前缀下找。所以一套部署里所有开锁的服务（singleton.enabled: true），以及
// 为业务时间高水位打开同一存储的服务（time.logic_offset 非 0），必须：
//
//   - 都写了 etcd.endpoints 或 etcd.endpoints_env（opener 不沿用 localhost:2379 缺省，缺了起不来）；
//   - etcd.endpoints 相同（按地址集合比较，顺序不论）；用 etcd.endpoints_env 的按变量名比较——变量的值只在部署时才有，
//     这里看不到，两种写法混用就无法证明是同一个集群，同样报 FAIL；
//   - singleton.key_prefix 相同（config-across-services 也按共享资源名查它，这里只在开锁的服务之间点名）。
//
// 开发配置集与生产示例各算一套部署，分别比较。只读配置，不改文件；参与比较的服务少于两个且都写了地址时为 OK。
func checkSingletonEtcd(root string, m Manifest) CheckItem {
	item := CheckItem{Name: "singleton:etcd-consistent", Status: StatusOK}
	var problems, summary []string
	for _, set := range []configFileSet{devConfigSet(m), prodExampleConfigSet(m)} {
		found, compared := singletonEtcdProblems(loadConfigSet(root, m, set))
		for _, problem := range found {
			problems = append(problems, set.label+": "+problem)
		}
		summary = append(summary, fmt.Sprintf("%s: %d service(s) with the lock", set.label, compared))
	}
	if len(problems) > 0 {
		item.Status = StatusFail
		item.Detail = strings.Join(problems, "\n") + "\nevery service that holds or reads the singleton lock must reach the same etcd " +
			"(etcd.endpoints) under the same singleton.key_prefix; otherwise Live and the Gate's identity check look for each other's lock in the wrong place"
		return item
	}
	item.Detail = "singleton lock services agree on etcd.endpoints and singleton.key_prefix (" + strings.Join(summary, "; ") + ")"
	return item
}

// singletonEtcdView 是一个服务在一份配置里与锁存储有关的取值。
type singletonEtcdView struct {
	service, rel         string
	endpoints, keyPrefix string // 没写为空
}

// singletonEtcdProblems 返回一套配置里开锁服务之间的不一致与参与比较的服务数。
func singletonEtcdProblems(docs []serviceConfigDocument) ([]string, int) {
	var views []singletonEtcdView
	var problems []string
	for _, doc := range docs {
		if !opensSingletonStore(doc) {
			continue
		}
		view := singletonEtcdView{service: doc.service, rel: doc.rel, endpoints: singletonEndpoints(doc)}
		if raw, ok := doc.source.Get("singleton.key_prefix"); ok {
			view.keyPrefix, _ = configschema.ParseString("singleton.key_prefix", raw)
			view.keyPrefix = strings.TrimSpace(view.keyPrefix)
		}
		if view.endpoints == "" {
			problems = append(problems, fmt.Sprintf("%s: %s opens the singleton store but writes neither etcd.endpoints nor etcd.endpoints_env (the lock does not fall back to localhost:2379)", doc.rel, doc.service))
		}
		views = append(views, view)
	}
	for _, field := range []struct {
		key   string
		value func(singletonEtcdView) string
	}{
		{"etcd.endpoints", func(v singletonEtcdView) string { return v.endpoints }},
		{"singleton.key_prefix", func(v singletonEtcdView) string { return v.keyPrefix }},
	} {
		distinct := map[string]bool{}
		var parts []string
		for _, view := range views {
			if field.key == "etcd.endpoints" && view.endpoints == "" {
				continue // 上面已经点名
			}
			distinct[field.value(view)] = true
			parts = append(parts, fmt.Sprintf("%s %q", view.service, field.value(view)))
		}
		if len(distinct) > 1 {
			problems = append(problems, fmt.Sprintf("%s differs between the services with the singleton lock: %s", field.key, strings.Join(parts, ", ")))
		}
	}
	return problems, len(views)
}

// opensSingletonStore 报告这份配置的进程是否打开单实例锁的存储：开了锁，或配了非 0 的业务时间偏移
// （App 为高水位单独打开同一个存储，见 framework/app/business_time.go）。
func opensSingletonStore(doc serviceConfigDocument) bool {
	if raw, ok := doc.source.Get("singleton.enabled"); ok {
		if on, err := configschema.ParseBool("singleton.enabled", raw); err == nil && on {
			return true
		}
	}
	if raw, ok := doc.source.Get("time.logic_offset"); ok {
		if offset, err := configschema.ParseDuration("time.logic_offset", raw); err == nil && offset != 0 {
			return true
		}
	}
	return false
}

// singletonEndpoints 是锁存储连接的 etcd 地址的比较形式：写了 etcd.endpoints_env 时是 "$<变量名>"（取代 etcd.endpoints，
// 与 wiring/mods 的 `_env` 规则相同），否则是排序去重后逗号连接的地址；都没写为空。
func singletonEndpoints(doc serviceConfigDocument) string {
	if raw, ok := doc.source.Get("etcd.endpoints_env"); ok {
		if name, err := configschema.ParseString("etcd.endpoints_env", raw); err == nil && strings.TrimSpace(name) != "" {
			return "$" + strings.TrimSpace(name)
		}
	}
	raw, ok := doc.source.Get("etcd.endpoints")
	if !ok {
		return ""
	}
	endpoints, err := configschema.ParseStrings("etcd.endpoints", raw)
	if err != nil {
		return fmt.Sprint(raw)
	}
	slices.Sort(endpoints)
	return strings.Join(slices.Compact(endpoints), ",")
}
