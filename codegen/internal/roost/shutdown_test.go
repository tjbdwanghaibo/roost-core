package roost

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 停机预算按服务计算：生成器从每个服务 bootstrap 实际注册的 Mod 算出
//
//	total_timeout = 声明的停机预算之和 + 3s x 未声明的 Mod + 5s（带 dataengine 的服务再加 3s 单实例锁释放）
//	grace period  = total_timeout + 5s
//
// 并把同一组数字写进服务配置（开发与生产示例）、k8s workload、生产 compose、shell 安装器写的 systemd unit 和
// dev 运行脚本。声明预算的 Mod：dataengine（dataengine.shutdown_timeout，30s）与 player TCP
// （player_access.tcp.shutdown_timeout，10s）；App 给每个未声明的 Mod 留 3s 下限，固定 60s 盖不住 game-demo 的 game 服务。

// bootstrapModCounts reads the generated bootstrap and returns, per service,
// how many Mods the process registers: the shared a.Mods(...) block plus the
// Mods passed to that service's RegisterServer. It is read from the output, not
// from the generator's own bookkeeping, so the two cannot agree by accident.
func bootstrapModCounts(t *testing.T, root string) (map[string]int, map[string]declaringMods) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "internal", "bootstrap", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	constructor := regexp.MustCompile(`(?m)^\t\t\S`)
	shared := 0
	if start := strings.Index(body, "\ta.Mods(\n"); start >= 0 {
		block := body[start : start+strings.Index(body[start:], "\n\t)\n")]
		shared = len(constructor.FindAllString(block, -1))
	}
	sharedDataEngine := strings.Contains(body[:max(strings.Index(body, "a.RegisterServer("), 0)], "kitdataengine.NewMod(")
	counts, declares := map[string]int{}, map[string]declaringMods{}
	register := regexp.MustCompile(`(?m)^\ta\.RegisterServer\(app\.ServiceName\(([^)]*)\)`)
	for _, match := range register.FindAllStringSubmatchIndex(body, -1) {
		name := strings.Trim(body[match[2]:match[3]], `"`)
		if strings.HasPrefix(name, "domain") && strings.HasSuffix(name, ".ServiceType") {
			name = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "domain"), ".ServiceType"))
		}
		rest := body[match[0]:]
		end := strings.Index(rest, "\n\ta.")
		if end < 0 {
			end = strings.Index(rest, "\n\treturn a, nil")
		}
		block := rest[:end]
		counts[name] = shared + len(constructor.FindAllString(block, -1))
		declares[name] = declaringMods{
			dataengine: sharedDataEngine || strings.Contains(block, "kitdataengine.NewMod("),
			playerTCP:  strings.Contains(block, "accessplayertcp.NewMod("),
		}
	}
	return counts, declares
}

// declaringMods is which of the Mods that declare a stop budget a service's
// bootstrap registers.
type declaringMods struct{ dataengine, playerTCP bool }

// expectedShutdown is the formula of the fix, applied to what the bootstrap
// registers: dataengine declares dataengine.shutdown_timeout (30s in the
// generated config), the player TCP Mod player_access.tcp.shutdown_timeout
// (10s, RR-20260927-05); every other Mod declares nothing. A service with
// dataengine has singleton.enabled: true in its generated config, and the App
// keeps 3s of the window back from the Mods for the release (APP-SINGLETON-LOCK §3.5).
func expectedShutdown(mods int, declares declaringMods) (totalSeconds, graceSeconds int) {
	declared, undeclared, release := 0, mods, 0
	if declares.dataengine {
		declared, undeclared, release = declared+30, undeclared-1, 3
	}
	if declares.playerTCP {
		declared, undeclared = declared+10, undeclared-1
	}
	total := declared + 3*undeclared + 5 + release
	return total, total + 5
}

func assertContains(t *testing.T, root, rel, want string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Errorf("%s does not contain %q", rel, want)
	}
}

func assertGeneratedShutdown(t *testing.T, root string, service string, total, grace int) {
	t.Helper()
	for _, rel := range []string{
		"configs/service/config." + service + ".yaml",
		"configs/service/config." + service + ".prod.example.yaml",
		"deploy/k8s/base/secret." + service + ".example.yaml",
	} {
		assertContains(t, root, rel, fmt.Sprintf("total_timeout: %ds\n", total))
		assertContains(t, root, rel, fmt.Sprintf(">= total_timeout + 5s: %ds for this service", grace))
	}
	assertContains(t, root, "deploy/k8s/base/"+service+".yaml", fmt.Sprintf("terminationGracePeriodSeconds: %d\n", grace))
	compose, err := os.ReadFile(filepath.Join(root, "deploy", "docker", "docker-compose.prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	block := string(compose)[strings.Index(string(compose), "\n  "+service+":\n"):]
	if next := regexp.MustCompile(`\n  [a-z0-9_-]+:\n`).FindStringIndex(block[1:]); next != nil {
		block = block[:next[0]+1]
	}
	if !strings.Contains(block, fmt.Sprintf("stop_grace_period: %ds\n", grace)) {
		t.Errorf("compose service %s: stop_grace_period is not %ds:\n%s", service, grace, block)
	}
	assertContains(t, root, "deploy/shell/install.sh", fmt.Sprintf("  %s) STOP_TIMEOUT=%ds ;;\n", service, grace))
	// audit8 D3: the shell README names the same per-service value, not a fixed 45s.
	readme, err := os.ReadFile(filepath.Join(root, "deploy", "shell", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(当前为 |、)`+regexp.QuoteMeta(fmt.Sprintf("%s %ds", service, grace))+`(、|；)`).Match(readme) || strings.Contains(string(readme), "45 秒") {
		t.Errorf("deploy/shell/README.md does not give %s the stop budget %ds install.sh uses", service, grace)
	}
	run, err := os.ReadFile(filepath.Join(root, "deploy", "dev", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf("%s:%d:%d", service, opsPortOf(t, root, service), grace)
	if !regexp.MustCompile(`[" ]` + regexp.QuoteMeta(entry) + `[" ]`).Match(run) {
		t.Errorf("deploy/dev/run.sh SERVICES has no %q entry", entry)
	}
}

func opsPortOf(t *testing.T, root, service string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "dev", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`[" ]` + regexp.QuoteMeta(service) + `:(\d+)`).FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatalf("deploy/dev/run.sh has no entry for %s", service)
	}
	var port int
	fmt.Sscanf(match[1], "%d", &port)
	return port
}

func TestGeneratedShutdownTimeoutFollowsEachServicesMods(t *testing.T) {
	target := newGameDemo(t)
	counts, declares := bootstrapModCounts(t, target)
	// The real game service of the demo: 22 service Mods (etcd among them, for the
	// match / activity clients that route by key, RR-20261006-59; entity_timer for the
	// World's timers, D11) + lock, ops, statslog.
	if counts["game"] != 25 || !declares["game"].dataengine || !declares["game"].playerTCP {
		t.Fatalf("game registers %d Mods (declaring: %+v), want 25 with dataengine and the player TCP Mod", counts["game"], declares["game"])
	}
	// 30s + 10s + 3s x 23 + 5s + 3s singleton release; a framework service (redis, nats, its owner Mod + 3 shared): 3s x 6 + 5s.
	pinned := map[string][2]int{"game": {117, 122}, "account": {23, 28}}
	for service, mods := range counts {
		total, grace := expectedShutdown(mods, declares[service])
		if want, ok := pinned[service]; ok && (want[0] != total || want[1] != grace) {
			t.Fatalf("%s: formula gives %ds / %ds, pinned %v", service, total, grace, want)
		}
		t.Run(service, func(t *testing.T) { assertGeneratedShutdown(t, target, service, total, grace) })
	}
	if counts["account"] >= counts["game"] {
		t.Fatalf("account registers %d Mods, game %d", counts["account"], counts["game"])
	}
	// The second game process runs the game service's Mods and waits as long.
	assertContains(t, target, "deploy/dev/second-game.sh", `[ "$i" -lt 122 ]`)
}

// A service with few Mods gets a smaller window than the demo's game service.
func TestGeneratedShutdownTimeoutIsSmallerForAServiceWithFewMods(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"gate"}, Mods: []string{"configdata"}, Features: []string{"config"}}); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, target)
	if declares["gate"] != (declaringMods{}) {
		t.Fatalf("gate unexpectedly runs a declaring Mod: %+v", declares["gate"])
	}
	total, grace := expectedShutdown(counts["gate"], declaringMods{})
	if total >= 117 {
		t.Fatalf("gate with %d Mods gets %ds, not less than the game service's 117s", counts["gate"], total)
	}
	assertGeneratedShutdown(t, target, "gate", total, grace)
}

func readProjectFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeProjectFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The configs are application-owned and rendered once, but the grace periods
// are regenerated on every sync from the current Mods. A Mod added later has
// to move an unedited generated shutdown: block with it (the game-demo is
// built exactly that way: its configs are written with 20 Mods, the scaffold
// then adds saga and the two player access Mods). A block that is not the current
// generated form (hand-edited, or written some other way) is left alone.
func TestSyncMovesAnUneditedShutdownBlockWithTheServicesMods(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	counts, declares := bootstrapModCounts(t, root)
	before, _ := expectedShutdown(counts["game"], declares["game"])
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	counts, declares = bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], declares["game"])
	if total != before+3 {
		t.Fatalf("adding redis moved the formula from %ds to %ds, want +3s", before, total)
	}
	assertGeneratedShutdown(t, root, "game", total, grace)

	// Hand-edited: kept as written, through the next Mod.
	dev := readProjectFile(t, root, "configs/service/config.game.yaml")
	writeProjectFile(t, root, "configs/service/config.game.yaml", strings.Replace(dev, fmt.Sprintf("total_timeout: %ds\n", total), "total_timeout: 200s\n", 1))
	// A block in some other hand-written form: kept as written too.
	handSet := "shutdown:\n  # Keep it >= dataengine.shutdown_timeout + 3s x the other Mods.\n  total_timeout: 60s\n  serve_wait_timeout: 5s\n"
	prod := readProjectFile(t, root, "configs/service/config.game.prod.example.yaml")
	end := shutdownBlockEnd(prod)
	writeProjectFile(t, root, "configs/service/config.game.prod.example.yaml", strings.Replace(prod, prod[strings.Index(prod, "shutdown:\n"):end], handSet, 1))
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "mongo", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "configs/service/config.game.yaml"); !strings.Contains(got, "total_timeout: 200s\n") {
		t.Errorf("a hand-edited total_timeout was rewritten:\n%s", got)
	}
	if got := readProjectFile(t, root, "configs/service/config.game.prod.example.yaml"); !strings.Contains(got, handSet) {
		t.Errorf("a hand-written shutdown block was rewritten:\n%s", got)
	}
	// The secret example was never touched by hand and keeps following.
	counts, declares = bootstrapModCounts(t, root)
	total, _ = expectedShutdown(counts["game"], declares["game"])
	assertContains(t, root, "deploy/k8s/base/secret.game.example.yaml", fmt.Sprintf("total_timeout: %ds\n", total))
	// The grace period follows the longest configured total (the hand-edited 200s), not the formula.
	assertDeployedGrace(t, root, "game", 205)
}

// deployedGraces reads a service's grace period out of every generated
// deployment template: k8s, production compose, the systemd installer and the
// dev run script.
func deployedGraces(t *testing.T, root, service string) map[string]int {
	t.Helper()
	number := func(rel, pattern string) int {
		match := regexp.MustCompile(pattern).FindStringSubmatch(readProjectFile(t, root, rel))
		if match == nil {
			t.Fatalf("%s: no grace period for %s (%s)", rel, service, pattern)
		}
		var value int
		fmt.Sscanf(match[1], "%d", &value)
		return value
	}
	compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
	block := compose[strings.Index(compose, "\n  "+service+":\n")+1:]
	if next := regexp.MustCompile(`\n  [a-z0-9_-]+:\n`).FindStringIndex(block); next != nil {
		block = block[:next[0]]
	}
	composeGrace := regexp.MustCompile(`stop_grace_period: (\d+)s`).FindStringSubmatch(block)
	if composeGrace == nil {
		t.Fatalf("compose service %s has no stop_grace_period", service)
	}
	var composeValue int
	fmt.Sscanf(composeGrace[1], "%d", &composeValue)
	return map[string]int{
		"k8s":     number("deploy/k8s/base/"+service+".yaml", `terminationGracePeriodSeconds: (\d+)`),
		"compose": composeValue,
		"systemd": number("deploy/shell/install.sh", `\n  `+regexp.QuoteMeta(service)+`\) STOP_TIMEOUT=(\d+)s ;;`),
		"dev":     number("deploy/dev/run.sh", `[" ]`+regexp.QuoteMeta(service)+`:\d+:(\d+)[" ]`),
	}
}

func assertDeployedGrace(t *testing.T, root, service string, want int) {
	t.Helper()
	for template, got := range deployedGraces(t, root, service) {
		if got != want {
			t.Errorf("%s: %s grace period = %ds, want %ds", service, template, got, want)
		}
	}
}

// sync 重新生成的宽限期不能低于进程实际会等的时间：手改过 total 的工程（sync 不改写手改的块），公式单独算出的宽限期
// 会小于配置的 total，平台会在 App 自己的窗口中途 SIGKILL。所以 grace = max(公式, 配置的 total) + 5s。
func TestSyncNeverLowersTheGracePeriodBelowTheConfiguredTotal(t *testing.T) {
	t.Parallel()
	target := newGameDemo(t)
	// account: a hand-set 60s block in a form sync does not recognise as generated.
	handSet := "shutdown:\n" +
		"  # Whole shutdown window: Service.Shutdown, then every Mod in reverse order. Every Mod without\n" +
		"  # a declared stop budget keeps a fixed 3s floor; a Mod that declares one\n" +
		"  # (dataengine.shutdown_timeout) is granted it from the rest, scaled down with a warning only\n" +
		"  # when the rest cannot cover it. Keep it >= dataengine.shutdown_timeout + 3s x the other Mods.\n" +
		"  # The deployment's termination grace period (k8s terminationGracePeriodSeconds, compose\n" +
		"  # stop_grace_period, systemd TimeoutStopSec) must be >= total_timeout + 5s: 65s by default.\n" +
		"  total_timeout: 60s\n  serve_wait_timeout: 5s\n"
	for _, rel := range []string{"configs/service/config.account.yaml", "configs/service/config.account.prod.example.yaml"} {
		body := readProjectFile(t, target, rel)
		end := shutdownBlockEnd(body)
		writeProjectFile(t, target, rel, strings.Replace(body, body[strings.Index(body, "shutdown:\n"):end], handSet, 1))
	}
	// game: a hand-edited total above the formula's 117s.
	game := readProjectFile(t, target, "configs/service/config.game.yaml")
	writeProjectFile(t, target, "configs/service/config.game.yaml", strings.Replace(game, "total_timeout: 117s\n", "total_timeout: 120s\n", 1))
	if _, err := SyncProject(target); err != nil {
		t.Fatal(err)
	}
	assertDeployedGrace(t, target, "account", 65)
	assertDeployedGrace(t, target, "game", 125)
	// Services nobody touched keep the formula.
	assertDeployedGrace(t, target, "chat", 28)
	// Neither config was rewritten.
	assertContains(t, target, "configs/service/config.account.yaml", handSet)
	assertContains(t, target, "configs/service/config.game.yaml", "total_timeout: 120s\n")
	// The second game process waits as long as the first.
	assertContains(t, target, "deploy/dev/second-game.sh", `[ "$i" -lt 125 ]`)

	// A config with no total_timeout key runs on the App's 30s fallback.
	rel := "configs/service/config.chat.yaml"
	writeProjectFile(t, target, rel, regexp.MustCompile(`  total_timeout: \d+s\n`).ReplaceAllString(readProjectFile(t, target, rel), ""))
	if _, err := SyncProject(target); err != nil {
		t.Fatal(err)
	}
	assertDeployedGrace(t, target, "chat", 35)
}

// roost doctor reports a config the generator no longer maintains when its
// window no longer fits: one the platform would cut short (a framework
// service still on the old 60s default, now generated with a 28s grace
// period), and one too short for its Mods (the game service on 60s).
func TestDoctorChecksEachServicesShutdownWindow(t *testing.T) {
	t.Parallel()
	target := newGameDemo(t)
	m, err := LoadManifest(target)
	if err != nil {
		t.Fatal(err)
	}
	status := func() map[string]CheckItem {
		items := map[string]CheckItem{}
		for _, item := range checkShutdownBudgets(target, m) {
			items[item.Name] = item
		}
		return items
	}
	for name, item := range status() {
		if item.Status != StatusOK {
			t.Errorf("freshly generated %s: %s %s", name, item.Status, item.Detail)
		}
	}
	for _, service := range []string{"account", "game"} {
		rel := "configs/service/config." + service + ".yaml"
		body := regexp.MustCompile(`total_timeout: \d+s\n`).ReplaceAllString(readProjectFile(t, target, rel), "total_timeout: 60s\n")
		writeProjectFile(t, target, rel, body)
	}
	// Before a sync the deployment templates still carry 28s for account:
	// a process the platform would SIGKILL while the App is still stopping it.
	items := status()
	if item := items["shutdown:account"]; item.Status != StatusFail || !strings.Contains(item.Detail, "deploy/k8s/base/account.yaml grace period 28s < total_timeout 60s + 5s") {
		t.Errorf("account on 60s before sync: %s %s", item.Status, item.Detail)
	}
	if item := items["shutdown:game"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "cannot cover 25 Mods (40s declared + 3s x 23 + 3s singleton release = 112s)") || !strings.Contains(item.Detail, "Set it to 117s") {
		t.Errorf("game on 60s: %s %s", item.Status, item.Detail)
	}
	// Sync follows the configured total: the grace period is back above it.
	if _, err := SyncProject(target); err != nil {
		t.Fatal(err)
	}
	items = status()
	if item := items["shutdown:account"]; item.Status != StatusOK {
		t.Errorf("account on 60s after sync: %s %s", item.Status, item.Detail)
	}
	if item := items["shutdown:game"]; item.Status != StatusWarn {
		t.Errorf("game on 60s after sync: %s %s (the total still cannot cover its Mods)", item.Status, item.Detail)
	}
}

// shutdownBlockEnd is the end of the first shutdown: block's total_timeout
// line, the block's last line since RR-20261006-38 dropped the unread
// serve_wait_timeout.
func shutdownBlockEnd(body string) int {
	start := strings.Index(body, "shutdown:\n")
	line := strings.Index(body[start:], "  total_timeout: ") + start
	return line + strings.IndexByte(body[line:], '\n') + 1
}

const handSet60sShutdownBlock = "shutdown:\n" +
	"  # Whole shutdown window: Service.Shutdown, then every Mod in reverse order. Every Mod without\n" +
	"  # a declared stop budget keeps a fixed 3s floor; a Mod that declares one\n" +
	"  # (dataengine.shutdown_timeout) is granted it from the rest, scaled down with a warning only\n" +
	"  # when the rest cannot cover it. Keep it >= dataengine.shutdown_timeout + 3s x the other Mods.\n" +
	"  # The deployment's termination grace period (k8s terminationGracePeriodSeconds, compose\n" +
	"  # stop_grace_period, systemd TimeoutStopSec) must be >= total_timeout + 5s: 65s by default.\n" +
	"  total_timeout: 60s\n  serve_wait_timeout: 5s\n"

// replaceShutdownBlock swaps the shutdown: block of a config (or of the
// config.yaml a k8s secret example embeds, keeping its indentation) for block.
func replaceShutdownBlock(t *testing.T, root, rel, block string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	start := regexp.MustCompile(`(?m)^( *)shutdown:\n`).FindStringSubmatchIndex(body)
	if start == nil {
		t.Fatalf("%s has no shutdown: block", rel)
	}
	indent := body[start[2]:start[3]]
	totalLine := strings.Index(body[start[0]:], indent+"  total_timeout: ")
	if totalLine < 0 {
		t.Fatalf("%s: shutdown: block has no total_timeout line", rel)
	}
	end := totalLine + strings.IndexByte(body[start[0]+totalLine:], '\n') + 1
	writeProjectFile(t, root, rel, body[:start[0]]+indentText(block, indent)+body[start[0]+end:])
}

func setShutdownTotal(t *testing.T, root, rel, total string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	edited := regexp.MustCompile(`total_timeout: \d+s\n`).ReplaceAllString(body, "total_timeout: "+total+"\n")
	if edited == body {
		t.Fatalf("%s: total_timeout not replaced by %s", rel, total)
	}
	writeProjectFile(t, root, rel, edited)
}

func shutdownStatus(t *testing.T, root string) map[string]CheckItem {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	items := map[string]CheckItem{}
	for _, item := range checkShutdownBudgets(root, m) {
		items[item.Name] = item
	}
	return items
}

func assertProjectDiffEmpty(t *testing.T, root string) {
	t.Helper()
	var out bytes.Buffer
	if err := DiffProject(root, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "summary: 0 file(s) would change") {
		t.Errorf("project diff after one sync is not empty:\n%s", out.String())
	}
}

func editManifest(t *testing.T, root string, edit func(*Manifest)) {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	edit(&m)
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
}

// newGameDemo 返回一份私有的 game-demo 工程。同参数只真实 NewProject 一次，之后复制（见 project_fixture_test.go；
// 副本与新鲜生成逐字节相同）。
func newGameDemo(t *testing.T) string {
	t.Helper()
	return copyOfNewProject(t, "game-demo")
}

// (a) Every config hand-set to 60s, then only config.game.yaml raised to the
// value doctor suggests: the production example and the secret example still
// run 60s, and doctor must say so.
func TestDoctorWarnsForEachRepositoryConfigThatCannotCoverTheModFloors(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"
	for _, rel := range []string{dev, prod, secret} {
		replaceShutdownBlock(t, root, rel, handSet60sShutdownBlock)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	for _, rel := range []string{dev, prod, secret} {
		if item.Status != StatusWarn || !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 25 Mods (40s declared + 3s x 23 + 3s singleton release = 112s)") {
			t.Errorf("all three configs on 60s: %s %s (want a WARN naming %s)", item.Status, item.Detail, rel)
		}
	}

	setShutdownTotal(t, root, dev, "117s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("only %s raised to 117s, the examples still 60s: %s %s", dev, item.Status, item.Detail)
	}
	for _, rel := range []string{prod, secret} {
		if !strings.Contains(item.Detail, rel+": total_timeout 60s cannot cover 25 Mods") {
			t.Errorf("WARN does not name %s: %s", rel, item.Detail)
		}
	}
	if strings.Contains(item.Detail, dev+":") {
		t.Errorf("WARN names %s, which covers the Mods: %s", dev, item.Detail)
	}

	setShutdownTotal(t, root, prod, "117s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, secret+":") || strings.Contains(item.Detail, prod+":") {
		t.Errorf("only %s left on 60s: %s %s", secret, item.Status, item.Detail)
	}

	setShutdownTotal(t, root, secret, "117s")
	item = shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 117s, grace period 122s") {
		t.Errorf("every config on 117s: %s %s", item.Status, item.Detail)
	}
}

// (b) A service loses a Mod (the game service no longer uses session, as in
// the auditor's aud4-modrm): ONE sync must bring the configs and every
// template to the new plan, leave project diff empty and doctor all OK with
// the grace period the templates on disk actually set.
func TestOneSyncConvergesAfterAServiceLosesAMod(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Uses = removeString(spec.Uses, "session")
		m.Services["game"] = spec
	})
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], declares["game"])
	if counts["game"] != 24 || total != 114 || grace != 119 {
		t.Fatalf("game after dropping session: %d Mods, %ds / %ds; want 24 Mods, 114s / 119s", counts["game"], total, grace)
	}
	assertGeneratedShutdown(t, root, "game", total, grace)
	assertContains(t, root, "deploy/dev/second-game.sh", `[ "$i" -lt 119 ]`)
	assertProjectDiffEmpty(t, root)
	for name, item := range shutdownStatus(t, root) {
		if item.Status != StatusOK {
			t.Errorf("%s after one sync: %s %s", name, item.Status, item.Detail)
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; !strings.Contains(item.Detail, "total_timeout 114s, grace period 119s") {
		t.Errorf("shutdown:game after one sync: %s", item.Detail)
	}
}

// (c) A service gains a Mod: one sync, the same promises.
func TestOneSyncConvergesAfterAServiceGainsAMod(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], declares["game"])
	assertGeneratedShutdown(t, root, "game", total, grace)
	assertProjectDiffEmpty(t, root)
	item := shutdownStatus(t, root)["shutdown:game"]
	if want := "total_timeout " + strconv.Itoa(total) + "s, grace period " + strconv.Itoa(grace) + "s"; item.Status != StatusOK || !strings.Contains(item.Detail, want) {
		t.Errorf("shutdown:game after adding redis: %s %s (want %q)", item.Status, item.Detail, want)
	}
}

// doctor reports what the platform will wait — the templates on disk — not
// what the generator would render now. Templates raised by hand to 120s: the
// OK line says 120s.
func TestDoctorShowsTheGracePeriodTheTemplatesOnDiskSet(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	graces := deployedGraces(t, root, "game")
	generated := graces["k8s"]
	replace := func(rel, old, new string) {
		body := readProjectFile(t, root, rel)
		if !strings.Contains(body, old) {
			t.Fatalf("%s does not contain %q", rel, old)
		}
		writeProjectFile(t, root, rel, strings.Replace(body, old, new, 1))
	}
	g, raised := strconv.Itoa(generated), "120"
	replace("deploy/k8s/base/game.yaml", "terminationGracePeriodSeconds: "+g+"\n", "terminationGracePeriodSeconds: "+raised+"\n")
	compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
	at := strings.Index(compose, "\n  game:\n")
	writeProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml", compose[:at]+strings.Replace(compose[at:], "stop_grace_period: "+g+"s", "stop_grace_period: "+raised+"s", 1))
	replace("deploy/shell/install.sh", "  game) STOP_TIMEOUT="+g+"s ;;", "  game) STOP_TIMEOUT="+raised+"s ;;")
	run := readProjectFile(t, root, "deploy/dev/run.sh")
	entry := regexp.MustCompile(`([" ]game:\d+:)` + g + `([" ])`)
	if !entry.MatchString(run) {
		t.Fatalf("deploy/dev/run.sh has no game entry with %ss", g)
	}
	writeProjectFile(t, root, "deploy/dev/run.sh", entry.ReplaceAllString(run, "${1}"+raised+"${2}"))
	if second := "deploy/dev/second-game.sh"; statErr(filepath.Join(root, second)) == nil {
		replace(second, `[ "$i" -lt `+g+` ]`, `[ "$i" -lt `+raised+` ]`)
	}
	for template, value := range deployedGraces(t, root, "game") {
		if value != 120 {
			t.Fatalf("setup: %s grace period %ds, want 120s", template, value)
		}
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusOK || !strings.Contains(item.Detail, "grace period 120s") {
		t.Errorf("templates on disk at 120s (generated %ds): %s %s", generated, item.Status, item.Detail)
	}
}

func projectFileHashes(t *testing.T, root string) map[string][sha256.Size]byte {
	t.Helper()
	out := map[string][sha256.Size]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = sha256.Sum256(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertProjectUnchanged(t *testing.T, root string, before map[string][sha256.Size]byte) {
	t.Helper()
	after := projectFileHashes(t, root)
	for _, rel := range diffSnapshot(before, after) {
		t.Errorf("a failed sync left %s changed", rel)
	}
}

// blockWritesIn 让 dir 里已有的文件在本测试余下时间里无法被替换，于是向 dir 的原子写（writeAtomic：先写临时文件，再改名覆盖）
// 失败；文件内容不变、仍可读。
//
// Windows 上 os.Chmod 只切换文件的只读属性，对目录的写入与改名不起作用，所以改为持有目录里每个文件的句柄：Go 在 Windows
// 打开文件时共享模式不带 FILE_SHARE_DELETE，而 os.Rename 是 MoveFileEx(MOVEFILE_REPLACE_EXISTING)——替换一个这样被打开的
// 文件、或把它挪去备份，都会被拒绝，writeAtomic 的直接改名与“先挪到备份再改名”两条路都失败；读取不受影响。Unix 上打开的
// 句柄不妨碍改名，仍用去掉目录写权限的办法（创建临时文件即失败）。
func blockWritesIn(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		held := 0
		for _, entry := range entries {
			if !entry.Type().IsRegular() {
				continue
			}
			file, err := os.Open(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			// Registered after t.TempDir, so it runs before the directory is removed.
			t.Cleanup(func() { _ = file.Close() })
			held++
		}
		if held == 0 {
			t.Fatalf("%s has no file to hold: the injected write failure would not happen", dir)
		}
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// The config refresh is part of the sync's commit: when it cannot be written,
// nothing of the sync is (the templates already rendered for the new plan are
// not left behind with the old configs).
func TestSyncWritesNothingWhenTheShutdownRefreshCannotBeWritten(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := projectFileHashes(t, root)
	blockWritesIn(t, filepath.Join(root, "configs", "service"))
	if _, err := SyncProject(root); err == nil {
		t.Fatal("sync succeeded although no file in configs/service can be written")
	}
	assertProjectUnchanged(t, root, before)
}

// A later write failing rolls the refreshed configs back with the templates.
func TestSyncRollsTheShutdownRefreshBackWithTheTemplates(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	before := projectFileHashes(t, root)
	// internal/bootstrap/generated.go sorts after configs/ and deploy/: both
	// are written, then rolled back.
	blockWritesIn(t, filepath.Join(root, "internal", "bootstrap"))
	if _, err := SyncProject(root); err == nil {
		t.Fatal("sync succeeded although no file in internal/bootstrap can be written")
	}
	assertProjectUnchanged(t, root, before)
}

// A config edited while sync runs (after the refresh was planned, before the
// commit) stops the sync like any other project input: the developer's edit
// stays, no template is written.
func TestSyncRefusesAConfigEditedWhileItRuns(t *testing.T) {
	root := copyOfNewProject(t, "configdata")
	editManifest(t, root, func(m *Manifest) {
		spec := m.Services["game"]
		spec.Mods = append(spec.Mods, "redis")
		m.Services["game"] = spec
	})
	rel := "configs/service/config.game.yaml"
	edited := readProjectFile(t, root, rel) + "# developer edit\n"
	syncProjectBeforeCommit = func() { writeProjectFile(t, root, rel, edited) }
	t.Cleanup(func() { syncProjectBeforeCommit = nil })
	before := projectFileHashes(t, root)
	_, err := SyncProject(root)
	if err == nil || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("sync with %s edited concurrently: %v", rel, err)
	}
	if got := readProjectFile(t, root, rel); got != edited {
		t.Errorf("the concurrent edit of %s was overwritten:\n%s", rel, got)
	}
	after := projectFileHashes(t, root)
	for _, changed := range diffSnapshot(before, after) {
		if changed != rel {
			t.Errorf("a refused sync wrote %s", changed)
		}
	}
}

func statErr(path string) error {
	_, err := os.Stat(path)
	return err
}

func removeString(values []string, drop string) []string {
	out := values[:0:0]
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

func gamePlan(t *testing.T, root string) serviceShutdown {
	t.Helper()
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	return serviceShutdownPlan(m, "game")
}

func syncProject(t *testing.T, root string) {
	t.Helper()
	if _, err := SyncProject(root); err != nil {
		t.Fatal(err)
	}
}

var gameConfigs = []string{
	"configs/service/config.game.yaml",
	"configs/service/config.game.prod.example.yaml",
	"deploy/k8s/base/secret.game.example.yaml",
}

var accountConfigs = []string{
	"configs/service/config.account.yaml",
	"configs/service/config.account.prod.example.yaml",
	"deploy/k8s/base/secret.account.example.yaml",
}

// A framework service needs 3s x 6 = 18s; a total_timeout of 0s or below runs
// on the App's 30s, which covers it.
func TestDoctorJudgesANonPositiveTotalAsTheAppsFallback(t *testing.T) {
	t.Parallel()
	for _, total := range []string{"0s", "-5s"} {
		t.Run(total, func(t *testing.T) {
			root := newGameDemo(t)
			for _, rel := range accountConfigs {
				setShutdownTotal(t, root, rel, total)
			}
			// The templates follow the App's 30s + 5s (RR-66 复核); without the
			// sync the FAIL for the old 28s grace period would come first.
			syncProject(t, root)
			item := shutdownStatus(t, root)["shutdown:account"]
			if item.Status != StatusOK {
				t.Fatalf("account with total_timeout %s (the App uses 30s, the Mods need 18s): %s %s, want OK", total, item.Status, item.Detail)
			}
			if !strings.Contains(item.Detail, "the App uses 30s") {
				t.Errorf("OK line does not say the App's fallback is what runs: %s", item.Detail)
			}
		})
	}
}

// The game service needs more than 30s: the WARN says so from the App's 30s,
// not from 0s.
func TestDoctorReportsTheAppsFallbackWhenItCannotCoverTheMods(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := gameConfigs[0]
	setShutdownTotal(t, root, dev, "0s")
	syncProject(t, root)
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("game with total_timeout 0s: %s %s, want a WARN", item.Status, item.Detail)
	}
	if want := dev + ": total_timeout 0s (the App uses 30s) cannot cover"; !strings.Contains(item.Detail, want) {
		t.Errorf("WARN does not judge the App's 30s (%q): %s", want, item.Detail)
	}
}

// dataengine.shutdown_timeout 0s runs on kit/dataengine's 30s: a total that
// covers "0s declared" but not the real 30s is a WARN.
func TestDoctorJudgesANonPositiveDataEngineBudgetAsItsFallback(t *testing.T) {
	root := newGameDemo(t)
	dev := gameConfigs[0]
	plan := gamePlan(t, root)
	short := plan.total - generatedShutdownMargin - time.Second // one second short with dataengine at 30s
	setShutdownTotal(t, root, dev, seconds(short))
	setDataEngineShutdownTimeout(t, root, dev, "0s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, dev+": total_timeout "+seconds(short)+" cannot cover") {
		t.Fatalf("game with total_timeout %s and dataengine.shutdown_timeout 0s (kit/dataengine uses 30s): %s %s, want a WARN naming %s",
			seconds(short), item.Status, item.Detail, dev)
	}
}

// The advice is computed from the dataengine budget the configs set, and
// following it makes the WARN go away.
func TestDoctorAdviceFollowsTheConfiguredDataEngineBudget(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	for _, rel := range gameConfigs {
		setDataEngineShutdownTimeout(t, root, rel, "60s")
	}
	plan := gamePlan(t, root)
	want := plan.total + 30*time.Second // the formula's total with dataengine at 60s instead of 30s
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("dataengine.shutdown_timeout 60s with total_timeout %s: %s %s, want a WARN", seconds(plan.total), item.Status, item.Detail)
	}
	if advice := "Set it to " + seconds(want); !strings.Contains(item.Detail, advice) {
		t.Fatalf("WARN advice is not %q (60s dataengine budget): %s", advice, item.Detail)
	}
	for _, rel := range gameConfigs {
		setShutdownTotal(t, root, rel, seconds(want))
	}
	syncProject(t, root)
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
		t.Errorf("after following the advice (%s) and syncing: %s %s, want OK", seconds(want), item.Status, item.Detail)
	}
}

// Configs that set different dataengine budgets get the advice each needs.
func TestDoctorAdvicePerFileWhenTheConfigsDiffer(t *testing.T) {
	root := newGameDemo(t)
	prod, secret := gameConfigs[1], gameConfigs[2]
	plan := gamePlan(t, root)
	for _, rel := range []string{prod, secret} {
		setShutdownTotal(t, root, rel, "60s")
	}
	setDataEngineShutdownTimeout(t, root, secret, "45s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	want := "Set it to " + seconds(plan.total) + " in " + prod + ", " + seconds(plan.total+15*time.Second) + " in " + secret
	if !strings.Contains(item.Detail, want) {
		t.Errorf("WARN advice is not %q: %s", want, item.Detail)
	}
}

const staleShutdownAdvice = "every stop warns and budgets are cut"

// setDataEngineShutdownTimeout rewrites the dataengine: block's
// shutdown_timeout (other blocks, such as a gateway's, have their own).
func setDataEngineShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "dataengine:\n")
	if block < 0 {
		t.Fatalf("%s has no dataengine: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: dataengine: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

func TestDoctorNamesTheFileAndKeyOfAnExampleItCannotParse(t *testing.T) {
	t.Parallel()
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"

	for _, tc := range []struct {
		name    string
		rel     string
		corrupt func(t *testing.T, root string)
		want    []string // every fragment the WARN must carry
	}{
		{
			name:    "prod example total_timeout is not a duration",
			rel:     prod,
			corrupt: func(t *testing.T, root string) { setShutdownTotal(t, root, prod, "soon") },
			want:    []string{prod + ": shutdown.total_timeout", `"soon"`},
		},
		{
			name:    "secret example dataengine.shutdown_timeout is not a duration",
			rel:     secret,
			corrupt: func(t *testing.T, root string) { setDataEngineShutdownTimeout(t, root, secret, "30 seconds") },
			want:    []string{secret + ": dataengine.shutdown_timeout", `"30 seconds"`},
		},
		{
			name: "prod example does not parse",
			rel:  prod,
			corrupt: func(t *testing.T, root string) {
				writeProjectFile(t, root, prod, readProjectFile(t, root, prod)+"shutdown: [\n")
			},
			want: []string{prod + ": does not parse: yaml: "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newGameDemo(t)
			if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
				t.Fatalf("setup: a fresh project is %s %s", item.Status, item.Detail)
			}
			tc.corrupt(t, root)
			item := shutdownStatus(t, root)["shutdown:game"]
			if item.Status != StatusWarn {
				t.Fatalf("%s: %s %s, want a WARN", tc.name, item.Status, item.Detail)
			}
			for _, fragment := range tc.want {
				if !strings.Contains(item.Detail, fragment) {
					t.Errorf("%s: WARN does not say %q: %s", tc.name, fragment, item.Detail)
				}
			}
			if strings.Contains(item.Detail, staleShutdownAdvice) || strings.Contains(item.Detail, "Set it to") {
				t.Errorf("%s: a parse failure is reported as a total that cannot cover the Mods: %s", tc.name, item.Detail)
			}
			for _, other := range []string{dev, prod, secret} {
				if other != tc.rel && strings.Contains(item.Detail, other+":") {
					t.Errorf("%s: WARN names %s, which parses and covers the Mods: %s", tc.name, other, item.Detail)
				}
			}
		})
	}
}

// A real shortfall next to a parse failure keeps its advice, and the advice
// follows the shortfall rather than the file the doctor could not read.
func TestDoctorKeepsTheAdviceForARealShortfallNextToAParseFailure(t *testing.T) {
	root := newGameDemo(t)
	prod := "configs/service/config.game.prod.example.yaml"
	secret := "deploy/k8s/base/secret.game.example.yaml"
	setShutdownTotal(t, root, prod, "soon")
	setShutdownTotal(t, root, secret, "60s")

	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn {
		t.Fatalf("%s %s, want a WARN", item.Status, item.Detail)
	}
	shortfall := secret + ": total_timeout 60s cannot cover 25 Mods (40s declared + 3s x 23 + 3s singleton release = 112s); " + staleShutdownAdvice + ". Set it to 117s"
	if !strings.Contains(item.Detail, shortfall) {
		t.Errorf("WARN does not carry the shortfall with its advice %q: %s", shortfall, item.Detail)
	}
	if !strings.Contains(item.Detail, prod+": shutdown.total_timeout") {
		t.Errorf("WARN does not name the file and key it cannot parse: %s", item.Detail)
	}
	if strings.Contains(item.Detail, prod+": total_timeout") {
		t.Errorf("WARN judges the unparsable %s as a shortfall: %s", prod, item.Detail)
	}
}

// The dev config is what deploy/dev runs directly; an invalid duration there
// stays a FAIL (unchanged by this fix).
func TestDoctorStillFailsOnAnInvalidDurationInTheDevConfig(t *testing.T) {
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	setShutdownTotal(t, root, dev, "soon")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusFail || !strings.Contains(item.Detail, dev+": shutdown.total_timeout") {
		t.Errorf("dev config with an invalid total_timeout: %s %s, want a FAIL naming the file and key", item.Status, item.Detail)
	}
}

func setPlayerTCPShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "player_access:\n")
	if block < 0 {
		t.Fatalf("%s has no player_access: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: player_access: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

// The generated game service's window counts the player TCP Mod's 10s.
func TestGeneratedShutdownCountsThePlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	if plan.playerTCP != 1 || plan.dataEngine != 1 || plan.declared != generatedDataEngineShutdownTimeout+generatedPlayerTCPShutdownTimeout {
		t.Fatalf("game plan declares %+v, want dataengine 30s + player tcp 10s", plan)
	}
	for _, rel := range gameConfigs {
		body := readProjectFile(t, root, rel)
		wants := []string{"40s declared + 3s x 23\n", "3s for the singleton release = 117s", "total_timeout: 117s\n"}
		if rel != gameConfigs[2] { // the k8s secret example carries no player_access block: the Mod's default 10s
			wants = append(wants, "shutdown_timeout: 10s\n")
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", rel, want)
			}
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 117s, grace period 122s") {
		t.Errorf("fresh game-demo: %s %s", item.Status, item.Detail)
	}
}

// playerTCPConfigs are the game configs that carry a player_access block (the
// k8s secret example does not: the Mod runs on its default there).
var playerTCPConfigs = gameConfigs[:2]

// doctor reads the listener's budget from the config: raised to 30s, the
// generated total no longer covers it, and the advice counts the 30s.
func TestDoctorCountsTheConfiguredPlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "30s")
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	want := plan.total + 20*time.Second
	if item.Status != StatusWarn || !strings.Contains(item.Detail, "(60s declared + 3s x 23 + 3s singleton release = ") || !strings.Contains(item.Detail, "Set it to "+seconds(want)) {
		t.Fatalf("player_access.tcp.shutdown_timeout 30s with total_timeout %s: %s %s, want a WARN with 60s declared and advice %s",
			seconds(plan.total), item.Status, item.Detail, seconds(want))
	}
	// 显式 0s 与缺失不同：运行时拒绝，doctor 必须同样拒绝。
	root = newGameDemo(t)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "0s")
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusFail {
		t.Errorf("player_access.tcp.shutdown_timeout 0s: %s %s, want FAIL", item.Status, item.Detail)
	}
}

// A negative value stops the Mod from starting: a FAIL in the dev config,
// a WARN naming the file and key in an example (like any value the doctor
// cannot use), never a budget.
func TestDoctorRejectsANegativePlayerTCPShutdownTimeout(t *testing.T) {
	root := newGameDemo(t)
	dev, prod := gameConfigs[0], gameConfigs[1]
	setPlayerTCPShutdownTimeout(t, root, prod, "-1s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, prod+": player_access.tcp.shutdown_timeout: -1s is negative") || strings.Contains(item.Detail, "Set it to") {
		t.Errorf("prod example with a negative player tcp shutdown_timeout: %s %s", item.Status, item.Detail)
	}
	setPlayerTCPShutdownTimeout(t, root, dev, "-1s")
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusFail || !strings.Contains(item.Detail, dev+": player_access.tcp.shutdown_timeout") {
		t.Errorf("dev config with a negative player tcp shutdown_timeout: %s %s, want a FAIL", item.Status, item.Detail)
	}
}

func TestExplicitZeroTCPShutdownBudgetIsRefused(t *testing.T) {
	var s shutdownSettings
	s.PlayerAccess.TCP.ShutdownTimeout = "0s"
	if got, err := configuredDeclaredBudgets(serviceShutdown{playerTCP: 1}, s); err == nil {
		t.Fatalf("explicit zero accepted with budget %s", got)
	}
	s.PlayerAccess.TCP.ShutdownTimeout = ""
	if got, err := configuredDeclaredBudgets(serviceShutdown{playerTCP: 1}, s); err != nil || got != 10*time.Second {
		t.Fatalf("missing default: %s %v", got, err)
	}
}
func TestGeneratedTCPBudgetKeepsInheritance(t *testing.T) {
	for _, raw := range []string{"nest:\n  request_timeout: 12s\n", "service: game\n"} {
		d, err := parseYAMLDocument([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range playerTCPDefaultsFor(d) {
			if (entry.key == "dispatch_timeout" || entry.key == "login_timeout") && entry.value != "0s" {
				t.Errorf("generated %s=%s freezes a runtime fallback", entry.key, entry.value)
			}
		}
	}
}
