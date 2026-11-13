package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entityowner"
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/servicerpc"
)

// 实体归属服务（//roost:entity service=<name>，维护者 2026-11-04 决定，见
// docs/maintenance/FEATURE-2026-11-04-alliance-service.md §2.10 第 1 项）在工程层的规则：
//
//   - 归属只在实体标记上声明一次；service= 必须是 roost.yaml 声明的业务服务，并且它带 Nest（handler 要在那里
//     注册）；remote=managed 的 kind 的归属服务还要有 remote_entity（写准入在那里）。生成前检查，报错写实体与服务名。
//   - 归属服务的 bootstrap 装 registry.NewOwnedNestHandlersMod("<service>")（internal/registry/ownership_gen.go，
//     nest 生成器写）：锁住归属 kind 的 handler 只注册在这个服务的引擎上。bootstrap 由 project sync 渲染、
//     ownership_gen.go 由 generate 写，两者从同一次扫描得到；改了 service= 而没 sync 时 generate 拒绝。
//   - add lifecycle 缺省落到归属服务，拒绝别的服务。
//   - doctor 的 entity-owner 检查：别的服务的代码构造了归属 kind 的 Sender（写必须经归属服务的 rpc）。
//   - //roost:rpc route 只能用在拥有 Remote 托管 kind 的服务的 rpc 上（checkRouteRPCOwners）；preload=startup 的
//     写法在 entityowner 扫描时校验。
//
// 未声明 service= 的 kind 不受这些约束，保持原行为：任何带 Nest 的服务都注册、都能写。

func projectEntityOwners(root string) ([]entityowner.Owned, error) {
	return entityowner.Scan(filepath.Join(root, "game"))
}

// relativeOwnerPosition 把扫描结果里的绝对 file:line 换成工程相对路径。
func relativeOwnerPosition(root string, item entityowner.Owned) string {
	if rel, err := filepath.Rel(root, item.Position); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return item.Position
}

// validateEntityOwners 检查每个 service= 的取值。
func validateEntityOwners(root string, m Manifest, owned []entityowner.Owned) error {
	declared := sortedServiceNames(m)
	for _, item := range owned {
		where := relativeOwnerPosition(root, item)
		spec, ok := m.Services[item.Service]
		if !ok {
			return fmt.Errorf("%s: entity %s declares service=%s, but roost.yaml has no such service (services: %s)",
				where, item.Entity, item.Service, strings.Join(declared, ", "))
		}
		if _, hosted := frameworkCatalog[spec.Framework]; hosted {
			return fmt.Errorf("%s: entity %s declares service=%s, which hosts a framework service; an entity belongs to a business service", where, item.Entity, item.Service)
		}
		mods := serviceConfigMods(m, item.Service)
		if !contains(mods, "nest") {
			return fmt.Errorf("%s: entity %s declares service=%s, but that service has no Nest to run its handlers; run: roost add mod nest -service %s",
				where, item.Entity, item.Service, item.Service)
		}
		if item.Remote && !contains(mods, "remote_entity") {
			return fmt.Errorf("%s: entity %s is remote=managed and declares service=%s, but that service has no remote_entity Mod to write it; run: roost add mod remote_entity -service %s",
				where, item.Entity, item.Service, item.Service)
		}
	}
	if _, err := entityowner.DirOwners(owned); err != nil {
		return err
	}
	return nil
}

// ownedHandlersModCall 是归属服务 bootstrap 里装的那一项。
func ownedHandlersModCall(service string) string {
	return fmt.Sprintf("registry.NewOwnedNestHandlersMod(%q)", service)
}

// foreignKindsOption 是 remote_entity Mod 构造里交给 RemoteEntityMod 的外来 kind（阶段 1 的
// kitremoteentity.WithForeignKinds，Init 时校验每个 kind 是已注册的 Remote 托管 kind）。
func foreignKindsOption(service string) string {
	return fmt.Sprintf("kitremoteentity.WithForeignKinds(registry.ForeignKinds(%q)...)", service)
}

// routeKindsOption / preloadKindsOption 是归属服务的 remote_entity 构造里交给 RemoteEntityMod 的两份列表（联盟服务方案
// §2.7）：本服务拥有的 Remote 托管 kind 在加载后登记 Redis 路由，其中 preload=startup 的起服预加载。
func routeKindsOption(service string) string {
	return fmt.Sprintf("kitremoteentity.WithRouteKinds(registry.RouteKinds(%q)...)", service)
}

func preloadKindsOption(service string) string {
	return fmt.Sprintf("kitremoteentity.WithPreloadKinds(registry.PreloadKinds(%q)...)", service)
}

// preloadModCall 是有预加载 kind 的服务 bootstrap 里装的预加载 Mod：排在 remote_entity、Nest（与 DataEngine 恢复）之后，
// 路由 rpc 的 owner Mod 经 OptionalDependsOn 排在它之后，预加载完成前不在总线上接请求。
const preloadModCall = "kitremoteentity.NewRemotePreloadMod()"

// localPreloadModCall 是起服预加载本地实体的服务 bootstrap 里装的那一项（wiring/dataengine.LocalPreloadMod）。
func localPreloadModCall(service string) string {
	return fmt.Sprintf("kitdataengine.NewLocalPreloadMod(registry.LocalPreloadKinds(%q)...)", service)
}

// localPreloadServices 决定哪些服务起服预加载本地实体（//roost:entity preload=startup，非 remote=managed）：
//
//   - 写了 service= 的实体由那个服务预加载；它要有 nest 与 dataengine（加载经 DataEngine、发布经 Nest）。
//   - 没写 service= 的实体由工程里唯一一个带 nest 与 dataengine 的业务服务预加载（-template game 只有 game）。
//     有几个这样的服务时说不清由谁加载：同一种服务类型内 sid 唯一，不同服务类型的进程可以用同一个 sid、读同一个
//     <库>_<sid>，让每个都加载会把别的服务的实体读进来——要求在实体标记上写 service=<name>。
//
// 规则不满足时返回错误（生成前检查报出，文件与行来自扫描）。
func localPreloadServices(root string, m Manifest, preloads []entityowner.Owned) (map[string]bool, error) {
	out := map[string]bool{}
	if len(preloads) == 0 {
		return out, nil
	}
	hosts := func(service string) bool {
		if _, hosted := frameworkCatalog[m.Services[service].Framework]; hosted {
			return false
		}
		mods, err := resolveMods(effectiveServiceMods(m, service))
		return err == nil && contains(mods, "nest") && contains(mods, "dataengine")
	}
	var candidates []string
	for _, service := range sortedServiceNames(m) {
		if hosts(service) {
			candidates = append(candidates, service)
		}
	}
	for _, item := range preloads {
		where := relativeOwnerPosition(root, item)
		if item.Service != "" {
			if _, declared := m.Services[item.Service]; !declared {
				return nil, fmt.Errorf("%s: entity %s declares service=%s, but roost.yaml has no such service", where, item.Entity, item.Service)
			}
			if !hosts(item.Service) {
				return nil, fmt.Errorf("%s: entity %s is preload=startup and declares service=%s, but that service lacks nest or dataengine, which the startup preload "+
					"loads through; run: roost add mod dataengine -service %s (and nest)", where, item.Entity, item.Service, item.Service)
			}
			out[item.Service] = true
			continue
		}
		switch len(candidates) {
		case 1:
			out[candidates[0]] = true
		case 0:
			return nil, fmt.Errorf("%s: entity %s is preload=startup, but no business service has both nest and dataengine to load it at startup", where, item.Entity)
		default:
			return nil, fmt.Errorf("%s: entity %s is preload=startup, but several services could load it (%s): services of different types may share a sid and so the "+
				"same <db>_<sid> database; declare which one with service=<name> on the //roost:entity marker", where, item.Entity, strings.Join(candidates, ", "))
		}
	}
	return out, nil
}

// routeAndPreloadServices 返回拥有 Remote 托管 kind 的服务与其中有预加载 kind 的服务。
func routeAndPreloadServices(owned []entityowner.Owned) (route, preload map[string]bool) {
	route, preload = map[string]bool{}, map[string]bool{}
	for _, item := range owned {
		if !item.Remote {
			continue
		}
		route[item.Service] = true
		if item.Preload {
			preload[item.Service] = true
		}
	}
	return route, preload
}

// withEntityOwners 记下拥有 kind 的服务（渲染 bootstrap 用）。扫描失败时不记：生成前检查会报同一个错误。
func (m Manifest) withEntityOwners(root string) Manifest {
	m.localPreloadServices = nil
	if preloads, err := entityowner.LocalPreloads(filepath.Join(root, "game")); err == nil {
		m.localPreloadServices, _ = localPreloadServices(root, m, preloads)
	}
	owned, err := projectEntityOwners(root)
	m.ownedRemote = false
	m.routeServices, m.preloadServices = nil, nil
	if err != nil || len(owned) == 0 {
		m.ownerServices = nil
		return m
	}
	for _, item := range owned {
		m.ownedRemote = m.ownedRemote || item.Remote
	}
	m.routeServices, m.preloadServices = routeAndPreloadServices(owned)
	services := map[string]bool{}
	for _, service := range entityowner.Services(owned) {
		services[service] = true
	}
	m.ownerServices = services
	return m
}

// checkEntityOwnersBeforeGenerate 在生成之前校验 service= 并确认 bootstrap 与归属一致。
func checkEntityOwnersBeforeGenerate(root string, m Manifest) error {
	owned, err := projectEntityOwners(root)
	if err != nil {
		return fmt.Errorf("scan entity owners: %w", err)
	}
	if err := validateEntityOwners(root, m, owned); err != nil {
		return err
	}
	if err := checkRouteRPCOwners(root, m, owned); err != nil {
		return err
	}
	localItems, err := entityowner.LocalPreloads(filepath.Join(root, "game"))
	if err != nil {
		return fmt.Errorf("scan local preload entities: %w", err)
	}
	localPreloads, err := localPreloadServices(root, m, localItems)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(root, "internal", "bootstrap", "generated.go"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, service := range sortedServiceNames(m) {
		if localPreloads[service] != strings.Contains(string(raw), localPreloadModCall(service)) {
			return fmt.Errorf("local preload entities (//roost:entity preload=startup) changed and internal/bootstrap/generated.go does not match for service %s "+
				"(want kitdataengine.NewLocalPreloadMod: %v); run: roost project sync (then roost generate)", service, localPreloads[service])
		}
	}
	owners := map[string]bool{}
	for _, service := range entityowner.Services(owned) {
		owners[service] = true
	}
	ownedRemote := false
	for _, item := range owned {
		ownedRemote = ownedRemote || item.Remote
	}
	routes, preloads := routeAndPreloadServices(owned)
	for _, service := range sortedServiceNames(m) {
		// 只看服务自己的 Mod：shared_mods 里的 remote_entity 不知道是哪个服务，不带 ForeignKinds（renderModConstructor）。
		own, _ := resolveMods(effectiveServiceMods(m, service))
		hasRemote := contains(own, "remote_entity")
		wantForeign := ownedRemote && hasRemote
		wantRoute := routes[service] && hasRemote
		wantPreload := preloads[service] && hasRemote
		if owners[service] != strings.Contains(string(raw), ownedHandlersModCall(service)) ||
			wantForeign != strings.Contains(string(raw), foreignKindsOption(service)) ||
			wantRoute != strings.Contains(string(raw), routeKindsOption(service)) ||
			wantPreload != strings.Contains(string(raw), preloadKindsOption(service)) {
			return fmt.Errorf("entity owners (//roost:entity service=) changed and internal/bootstrap/generated.go does not match for service %s; "+
				"run: roost project sync (then roost generate)", service)
		}
	}
	return nil
}

// checkRouteRPCOwners 是路由 rpc（//roost:rpc route，联盟服务方案 §2.10 第 2 项）在工程层的三条规则。路由是方法级策略，
// 只查标了 route 的方法，不标 route 的 rpc（例如 mail）不受影响：
//
//   - 拥有者必须托管 Remote 实体 kind（//roost:entity remote=managed service=<owner>）：路由客户端按实体的 Redis 路由
//     登记、再按持久所有权记录找 owner（§2.7），只有这类 kind 有；没有时每次调用都只会得到“实体不存在”。
//     add rpc -entity-service 只是骨架形状、不进 roost.yaml，规则按拥有者的实体判断。
//   - 调用方（uses_rpcs）要有发布路由查找的 Mod：remote_entity（自己也写 Remote 实体）或 remote_mirror（只读，带 Mongo）。
//     生成的 ClientMod 在 Provide 时从注册表取它（kitremoteentity.RouteSource）。
//   - 拥有者有 preload=startup kind 时，owner Mod（mod.go，业务文件）要把 mods.ModRemoteEntityPreload 写进
//     OptionalDependsOn：预加载完成前不在总线上注册 handler（§2.7“时机”）。按源码文本查。
//
// 解析失败的 rpc 包跳过：同一个错误由随后的 servicerpc 生成报出，带文件与行。
func checkRouteRPCOwners(root string, m Manifest, owned []entityowner.Owned) error {
	routeServices, preloadServices := routeAndPreloadServices(owned)
	for _, dir := range projectRPCDirs(root, m) {
		services, err := servicerpc.ParseDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			continue
		}
		rpc := filepath.Base(dir)
		owner := ""
		for _, name := range sortedServiceNames(m) {
			if contains(m.Services[name].Rpcs, rpc) {
				owner = name
				break
			}
		}
		routed := ""
		for _, service := range services {
			for _, method := range service.Methods {
				if method.Route == "" {
					continue
				}
				if routed == "" {
					routed = service.Interface + "." + method.Name
				}
				if routeServices[owner] {
					continue
				}
				return fmt.Errorf("%s: method %s.%s is a route method (//roost:rpc route, key %s), but its owner service %s owns no Remote managed entity kind; "+
					"a route call finds the entity's owner through the route registration and the durable ownership record, which only kinds declared "+
					"//roost:entity remote=managed service=%s have. Declare the entity it acts on that way, or drop the route marker (a create method routes to any instance)",
					dir, service.Interface, method.Name, method.Route, owner, owner)
			}
		}
		if routed == "" {
			continue
		}
		for _, caller := range sortedServiceNames(m) {
			if !contains(m.Services[caller].UsesRpcs, rpc) {
				continue
			}
			mods, err := resolveMods(effectiveServiceMods(m, caller))
			if err != nil {
				return err
			}
			if !contains(mods, "remote_entity") && !contains(mods, "remote_mirror") {
				return fmt.Errorf("service %s calls %s, a route method of rpc %s, but has neither remote_entity nor remote_mirror to look up the entity's owner; "+
					"run: roost add mod remote_mirror -service %s", caller, routed, rpc, caller)
			}
		}
		if preloadServices[owner] {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), "mod.go"))
			if err == nil && !strings.Contains(string(raw), "ModRemoteEntityPreload") {
				return fmt.Errorf("%s/mod.go: rpc %s has route methods and its owner %s preloads entities (preload=startup), but the owner Mod does not order itself after "+
					"the preload: add mods.ModRemoteEntityPreload to its OptionalDependsOn, so no request is served before the preload finishes", dir, rpc, owner)
			}
		}
	}
	return nil
}

var ownedWrapperPattern = regexp.MustCompile(`// Register\w+NestHandlersOn 在归属服务 (\w+) 的`)

// checkEntityOwnerUse 是 doctor 的 entity-owner 检查：没有 kind 声明归属时不出项。归属 kind 的 handler 只在归属服务
// 注册，别的服务构造它的 Sender 发出的消息在本进程得到“handler 未注册”——写要经归属服务的 rpc。按源码文本查
// New<X>Sender 的使用：服务目录（internal/service/<name>/）、服务拥有的 rpc 包与玩家接入代码按服务归属，
// 共用代码（game/ 下的 handler、组件）看不出由谁调用，不在检查之内。
func checkEntityOwnerUse(root string, m Manifest) []CheckItem {
	owned, err := projectEntityOwners(root)
	if err != nil {
		return []CheckItem{{Name: "entity-owner", Status: StatusFail, Detail: err.Error()}}
	}
	if len(owned) == 0 {
		return nil
	}
	if err := validateEntityOwners(root, m, owned); err != nil {
		return []CheckItem{{Name: "entity-owner", Status: StatusFail, Detail: err.Error()}}
	}
	senders := map[string]string{} // New<X>Sender → 归属服务
	_ = filepath.WalkDir(filepath.Join(root, "game"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, "_nest_gen.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if match := ownedWrapperPattern.FindSubmatch(raw); match != nil {
			senders["New"+senderStem(path)+"Sender"] = string(match[1])
		}
		return nil
	})
	sources, err := projectGoSources(root)
	if err != nil {
		return []CheckItem{{Name: "entity-owner", Status: StatusFail, Detail: err.Error()}}
	}
	var problems []string
	for rel, source := range sources {
		user := codeService(m, rel)
		if user == "" {
			continue
		}
		for constructor, owner := range senders {
			if owner != user && regexp.MustCompile(`\b`+constructor+`\b`).MatchString(source) {
				problems = append(problems, fmt.Sprintf("%s (service %s) uses %s, but its kind belongs to service %s; write through %s's rpc", rel, user, constructor, owner, owner))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return []CheckItem{{Name: "entity-owner", Status: StatusFail, Detail: strings.Join(problems, "; ")}}
	}
	return []CheckItem{{Name: "entity-owner", Status: StatusOK, Detail: fmt.Sprintf("%d owned entity kind(s); no other service sends to their handlers", len(owned))}}
}

// senderStem 与 nest 生成器的 senderTypeName 同一规则：文件名去掉 _nest_gen 与 handler 前缀后转 Pascal。
func senderStem(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), "_nest_gen.go")
	base = strings.TrimPrefix(strings.TrimPrefix(base, "handler_"), "handler")
	return toPascal(base)
}

// codeService 返回一段源码所属的服务：internal/service/<name>/、服务拥有的 internal/rpc/<rpc>/、玩家接入代码
// （internal/access/player/、game/controllers/ 归 access.player.service）。其余为 ""。
func codeService(m Manifest, rel string) string {
	if rest, ok := strings.CutPrefix(rel, "internal/service/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		if _, declared := m.Services[name]; declared {
			return name
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(rel, "internal/rpc/"); ok {
		rpc, _, _ := strings.Cut(rest, "/")
		for _, service := range sortedServiceNames(m) {
			if contains(m.Services[service].Rpcs, rpc) {
				return service
			}
		}
		return ""
	}
	if strings.HasPrefix(rel, "internal/access/player/") || strings.HasPrefix(rel, "game/controllers/") {
		if access, enabled := m.Access["player"]; enabled {
			return access.Service
		}
	}
	return ""
}
