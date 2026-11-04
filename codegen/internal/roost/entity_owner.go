package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/entityowner"
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

// withEntityOwners 记下拥有 kind 的服务（渲染 bootstrap 用）。扫描失败时不记：生成前检查会报同一个错误。
func (m Manifest) withEntityOwners(root string) Manifest {
	owned, err := projectEntityOwners(root)
	if err != nil || len(owned) == 0 {
		m.ownerServices = nil
		return m
	}
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
	raw, err := os.ReadFile(filepath.Join(root, "internal", "bootstrap", "generated.go"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	owners := map[string]bool{}
	for _, service := range entityowner.Services(owned) {
		owners[service] = true
	}
	for _, service := range sortedServiceNames(m) {
		if owners[service] != strings.Contains(string(raw), ownedHandlersModCall(service)) {
			return fmt.Errorf("entity owners (//roost:entity service=) changed and internal/bootstrap/generated.go does not match for service %s; "+
				"run: roost project sync (then roost generate)", service)
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
