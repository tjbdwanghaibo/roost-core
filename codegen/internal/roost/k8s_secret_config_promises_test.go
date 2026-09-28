package roost

// RR-20260928-07：k8s Secret 示例内嵌的 config.yaml 与同服务的生产示例同源——项目创建之后再追加进生产
// 示例的段（add mod / add saga 的 Mod 配置段、add transport tcp 的 player_access 段）也要进 Secret。
// 旧行为：Secret 示例只在建工程时按 Mod 目录渲染一次，事后追加的段只写开发配置与生产示例；game-demo
// 按 Secret 示例部署时没有 saga 段（saga 按代码默认 8 GiB 建 ROOST_SAGA，本机实跑报
// "insufficient storage resources available"，且没有可改的配置段），也没有 player_access 段（玩家 TCP
// 默认关闭）。
// 断言：每个服务的 Secret 示例内嵌 config.yaml 与生产示例键集合一致、取值一致（Secret 独有的敏感值
// 占位目前没有；将来有了要在 secretOnlyConfigKeys 里显式列出）。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// secretOnlyConfigKeys are keys a Secret example may carry that the
// production example does not. There are none today.
var secretOnlyConfigKeys = map[string]bool{}

// checkSecretMirrorsProductionExample compares one service's k8s Secret
// example against its production example and reports every difference.
func checkSecretMirrorsProductionExample(t *testing.T, root, service string) {
	t.Helper()
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	prodRel := "configs/service/config." + service + ".prod.example.yaml"
	secretRel := "deploy/k8s/base/secret." + service + ".example.yaml"
	prod := configLeaves(t, prodRel, read(prodRel))
	secret := configLeaves(t, secretRel, secretExampleConfig(t, secretRel, read(secretRel)))
	var missing, extra, differ []string
	for _, key := range sortedKeys(prod) {
		value, ok := secret[key]
		switch {
		case !ok:
			missing = append(missing, key)
		case value != prod[key]:
			differ = append(differ, key+": secret="+value+" prod="+prod[key])
		}
	}
	for _, key := range sortedKeys(secret) {
		if _, ok := prod[key]; !ok && !secretOnlyConfigKeys[key] {
			extra = append(extra, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s lacks keys %s has: %s", secretRel, prodRel, summarizeConfigKeys(missing))
	}
	if len(extra) > 0 {
		t.Errorf("%s has keys %s does not: %s", secretRel, prodRel, summarizeConfigKeys(extra))
	}
	if len(differ) > 0 {
		t.Errorf("%s and %s disagree: %v", secretRel, prodRel, differ)
	}
}

// summarizeConfigKeys groups dotted keys by their top-level section so a
// missing section reads as one line ("saga (42 keys)") instead of forty.
func summarizeConfigKeys(keys []string) string {
	count := map[string]int{}
	var order []string
	for _, key := range keys {
		section, _, _ := strings.Cut(key, ".")
		if count[section] == 0 {
			order = append(order, section)
		}
		count[section]++
	}
	parts := make([]string, 0, len(order))
	for _, section := range order {
		parts = append(parts, section+" ("+strconv.Itoa(count[section])+" keys)")
	}
	return strings.Join(parts, ", ")
}

// The game-demo gets saga (add saga) and player_access (add transport tcp)
// after its configs are rendered; every service's Secret must still match.
func TestGameDemoKubernetesSecretExamplesMirrorTheProductionExamples(t *testing.T) {
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range sortedServiceNames(m) {
		checkSecretMirrorsProductionExample(t, root, service)
	}
}

// The same promise on a plain project, one appender at a time: add mod,
// add saga, add access + add transport tcp. A repeat changes nothing.
func TestAddedConfigSectionsReachTheKubernetesSecretExample(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "saga")
	secretRel := filepath.Join(root, "deploy", "k8s", "base", "secret.game.example.yaml")
	steps := []AddOptions{
		{Kind: "mod", Name: "redis", Service: "game"},
		{Kind: "saga", Name: "AllianceRally", Service: "game", Steps: []string{"Reserve", "March"}},
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "transport", Name: "tcp"},
	}
	for _, step := range steps {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
		if _, err := SyncProject(root); err != nil {
			t.Fatalf("sync after add %s: %v", step.Kind, err)
		}
		t.Run("after add "+step.Kind, func(t *testing.T) { checkSecretMirrorsProductionExample(t, root, "game") })
	}
	before, err := os.ReadFile(secretRel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(secretRel); string(after) != string(before) {
		t.Errorf("re-adding a mod rewrote the Secret example:\n%s", after)
	}
}
