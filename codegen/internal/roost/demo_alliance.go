package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// demoAllianceService 由 alliance 模块安装。这里只保留 demo 的开发配置与运维示例。
const demoAllianceService = "alliance"

func demoAllianceSteps(gameService string) []demoScaffoldStep {
	return []demoScaffoldStep{
		{write: "internal/service/game/alliance.go", why: "alliance module start after demo PlayerOwners is ready"},
		{run: demoAllianceRuntime, why: "demo owns alliance startup order, module owns all business logic"},
		{run: demoAllianceConfig, why: "demo alliance limits and isolated development WAL"},
		{run: enableDemoAllianceAdmin, why: "demo alliance operator endpoint"},
		{run: demoGameMirrorStaleness, why: "demo Mirror refresh bound"},
		{write: "deploy/dev/second-alliance.sh", why: "a second alliance process on the developer machine"},
	}
}

// demoGameMirrorStaleness sets remote_entity.cached_max_staleness to 5s in
// the game service's three configs (dev, production example, k8s Secret
// example). The game reads every alliance through Remote mirrors (plan §1.3):
// while pushes flow a read is fresh within tens of milliseconds; when they
// stop (an owner restarting, a reader past its push quota) a cached snapshot
// is re-confirmed once it is older than this. Measured in phase 5: the
// re-confirmations are the distinct keys being read divided by this bound —
// about 20,000 Redis HGET per second for 100,000 players online at 5s.
func demoGameMirrorStaleness(root, gameService string) error {
	const (
		before = "确认不了就不交出。不配置时等于 snapshot_cache_ttl（这里写成与它相同）；配置了必须为正。\n"
		after  = "确认不了就不交出。不配置时等于 snapshot_cache_ttl；配置了必须为正。game-demo：game 经 Mirror 读公会，\n"
		value  = "cached_max_staleness: 30s\n"
		next   = "推送中断时最长 5s 回源一次（回源量 = 在读的不同 key 数 / 5s，联盟服务方案 §1.3）。\n"
	)
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
		"deploy/k8s/base/secret." + gameService + ".example.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		text := string(raw)
		at := strings.Index(text, before)
		if at < 0 || strings.Count(text, before) != 1 {
			return fmt.Errorf("%s: expected one remote_entity.cached_max_staleness comment", path)
		}
		lineStart := strings.LastIndex(text[:at], "\n") + 1
		indent := text[lineStart:at]
		indent = indent[:len(indent)-len(strings.TrimLeft(indent, " "))]
		old := before + indent + value
		if !strings.Contains(text, old) {
			return fmt.Errorf("%s: expected cached_max_staleness: 30s after its comment", path)
		}
		text = strings.Replace(text, old, after+indent+"# "+next+indent+"cached_max_staleness: 5s\n", 1)
		if err := writeAtomic(path, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// enableDemoAllianceAdmin turns the ops admin endpoint on in the alliance
// service's dev config, as enableDemoAdmin does for the game: the alliance
// operator commands answer on its ops.addr with the dev token.
func enableDemoAllianceAdmin(root, _ string) error {
	return enableDemoAdmin(root, demoAllianceService)
}

// demoAllianceConfig 写入 demo 的联盟规则，并重新生成 activity 分组。
// 增加服务改变生产 compose 的 sid 顺序；分组必须跟随最终服务表。
// 开发 WAL 的服务隔离由模块安装器完成；第二实例脚本再单独指定实例目录。
func demoAllianceConfig(root, _ string) error {
	const block = "alliance:\n" +
		"  # No routing settings: an alliance is served by the instance its Remote\n" +
		"  # ownership record names, found through the route registration.\n" +
		"  max_members: 100\n" +
		"  reservation_ttl: 30s\n" +
		"  receipt_retention: 168h\n" +
		"  rejoin_cooldown: 0s\n" +
		"  # The alliance reward (prototype defaults): reward_count of reward_item_id\n" +
		"  # per member per reward_period; the game hands it over in its reward saga.\n" +
		"  reward_item_id: 1002\n" +
		"  reward_count: 5\n" +
		"  reward_period: 24h\n"
	if err := appendDemoGameConfig(root, demoAllianceService, "alliance", block, block); err != nil {
		return err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	if !hostsActivityCoordinator(m) {
		return nil
	}
	return writeAtomic(filepath.Join(root, filepath.FromSlash(activityGroupsFile)), []byte(renderActivityGroups(m)), 0o644)
}

// demo 在 Service.Init 中先发布自有驻留表，再启动联盟消费者；关闭缺省自动启动，避免启动两份消费者。
func demoAllianceRuntime(root, _ string) error {
	before, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	record := m.Modules["alliance"]
	record.Runtime = nil
	m.Modules["alliance"] = record
	return commitManifestSync(root, before, m)
}
