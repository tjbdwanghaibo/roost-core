package roost

// RR-20260928-06：game-demo 的生产示例配置与 k8s Secret 示例必须能让 game 通过 Init 的配置校验。
// demo 的 game 在 Init 里要求 platform.payment_secret 非空、App 单实例锁打开（activity 从它的 Live
// 查询取 expected 集合）、activity.key_prefix 非空（按这个顺序失败），activity.groups_file 指向活动组文件（C4，取代 activity.game_sids），
// platform.key_prefix 按 kit/mods.KeyPrefix 的规则非空且无空白。旧行为：demo 只把追加段写进开发配置，
// 按生产示例起 game（本机 compose 实测）依次报 "platform.payment_secret is empty" →
// "game_route.key_prefix is empty" → "activity.key_prefix is empty"。
// 这里同时守“与开发配置同源”：每个服务的生产示例与开发配置的键集合一致；Secret 示例里 demo 追加的
// 这三段与生产示例一致；前缀取值与开发配置、以及所属服务自己的配置相同；密钥不沿用开发值（生产里
// 是 CHANGE_ME，由运维填）。
// Secret 示例这里只比这三段；Secret 与生产示例整体同源（含 add saga / player TCP 事后追加的 saga、
// player_access 段）由 RR-20260928-07 的 k8s_secret_config_promises_test.go 守。
// App 单实例锁（APP-SINGLETON-LOCK-2026-10-05 §6.3）：game 带 dataengine，三份配置都要
// singleton.enabled: true、key_prefix 为 roost:<project>:singleton，取值满足 App 启动校验的三条关系。
// 静态绑定方案第 3 笔删除了按玩家的 Redis 表，game_route 段随之删除：三份配置里都不能再有它。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// configLeaves flattens a YAML document into dotted key -> scalar text.
func configLeaves(t *testing.T, name, body string) map[string]string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	out := map[string]string{}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		if m, ok := value.(map[string]any); ok && len(m) > 0 {
			for key, child := range m {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, child)
			}
			return
		}
		out[prefix] = fmt.Sprint(value)
	}
	walk("", doc)
	return out
}

// secretExampleConfig returns the config.yaml a k8s Secret example embeds.
func secretExampleConfig(t *testing.T, name, body string) string {
	t.Helper()
	var secret struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(body), &secret); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	config, ok := secret.StringData["config.yaml"]
	if !ok {
		t.Fatalf("%s has no stringData.config.yaml", name)
	}
	return config
}

func sortedKeys(set map[string]string) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// checkDemoGameInitConfig applies the checks the demo's game code makes on its
// configuration at Init, in the order it makes them, and returns the first
// refusal with the game's own wording.
func checkDemoGameInitConfig(body string) error {
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(body)); err != nil {
		return err
	}
	// game/controllers/player/controller.go
	if cfg.GetString("platform.payment_secret") == "" {
		return fmt.Errorf("player controller: platform.payment_secret is empty")
	}
	// internal/service/<game>/activity.go: app.ModSingleton is published only
	// when the App singleton lock is on.
	if !cfg.GetBool("singleton.enabled") {
		return fmt.Errorf("activity: capability %q not found; needs singleton.enabled=true", "singleton")
	}
	if cfg.GetString("activity.key_prefix") == "" {
		return fmt.Errorf("activity: activity.key_prefix is empty")
	}
	if cfg.GetString("activity.groups_file") == "" {
		return fmt.Errorf("activity: activity.groups_file is empty")
	}
	if cfg.IsSet("activity.game_sids") {
		return fmt.Errorf("activity: activity.game_sids is the removed per-service list; the group comes from activity.groups_file (C4)")
	}
	// internal/service/<game>/purchase_drain.go: mods.KeyPrefix(cfg, "platform")
	if prefix := strings.TrimSpace(cfg.GetString("platform.key_prefix")); prefix == "" || strings.ContainsAny(prefix, " \t\n") {
		return fmt.Errorf("purchase drain: platform.key_prefix %q", prefix)
	}
	return nil
}

// demoGameSection reports whether a dotted key belongs to one of the sections
// the demo appends to the game service's configs.
func demoGameSection(key string) bool {
	for _, section := range []string{"activity", "platform", "singleton"} {
		if key == section || strings.HasPrefix(key, section+".") {
			return true
		}
	}
	return false
}

func TestGameDemoProductionConfigsPassTheGameInitChecks(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	gameService := m.Access["player"].Service
	if gameService == "" {
		t.Fatal("game-demo declares no player access service")
	}
	for _, service := range sortedServiceNames(m) {
		devRel := "configs/service/config." + service + ".yaml"
		prodRel := "configs/service/config." + service + ".prod.example.yaml"
		secretRel := "deploy/k8s/base/secret." + service + ".example.yaml"
		configs := map[string]string{
			prodRel:   read(prodRel),
			secretRel: secretExampleConfig(t, secretRel, read(secretRel)),
		}
		dev := configLeaves(t, devRel, read(devRel))
		for rel, body := range configs {
			leaves := configLeaves(t, rel, body)
			var missing []string
			for _, key := range sortedKeys(dev) {
				if rel == secretRel && !demoGameSection(key) {
					continue
				}
				if _, ok := leaves[key]; !ok {
					missing = append(missing, key)
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s lacks keys the dev config %s has: %v", rel, devRel, missing)
			}
			for _, key := range sortedKeys(leaves) {
				if strings.HasSuffix(key, "key_prefix") && dev[key] != "" && leaves[key] != dev[key] {
					t.Errorf("%s: %s = %q, dev config has %q; the two come from one source", rel, key, leaves[key], dev[key])
				}
				if strings.Contains(strings.ToLower(leaves[key]), "dev-") {
					t.Errorf("%s: %s carries the dev value %q", rel, key, leaves[key])
				}
			}
			if service != gameService {
				continue
			}
			if err := checkDemoGameInitConfig(body); err != nil {
				t.Errorf("%s: the game refuses it at Init: %v", rel, err)
			}
			assertSingletonOn(t, rel, leaves, "roost:planet:singleton")
			for key := range leaves {
				if key == "game_route" || strings.HasPrefix(key, "game_route.") {
					t.Errorf("%s still has %s; the per-player route table and its keyspace were removed with static binding", rel, key)
				}
			}
			// The prefixes the game borrows are the owning services' own.
			for owner, key := range map[string]string{"activity": "activity.key_prefix", "platform": "platform.key_prefix"} {
				ownerRel := "configs/service/config." + owner + ".prod.example.yaml"
				want := configLeaves(t, ownerRel, read(ownerRel))[key]
				if got := leaves[key]; got != want {
					t.Errorf("%s: %s = %q, but the %s service keeps its keys under %q", rel, key, got, owner, want)
				}
			}
		}
	}
}
