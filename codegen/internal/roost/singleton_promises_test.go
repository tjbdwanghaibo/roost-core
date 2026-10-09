package roost

// App 单实例锁的生成承诺（docs/feature/APP-SINGLETON-LOCK-2026-10-05.md §6.3，第 2 笔）。
//
//  1. bootstrap：项目里有 redis Mod，或有带 dataengine 的服务（其配置默认打开 singleton），就生成
//     a.Singleton(kitredis.SingletonStore)。按“项目里有 redis”而不是按“有服务启用”生成，是因为是否启用
//     写在归应用所有的配置里，bootstrap 是生成文件、用户改不了；dataengine 服务没有 redis Mod 时也要装，
//     否则它默认打开的 singleton 在启动时 fail-closed（ErrSingletonOpenerMissing）。
//  2. 服务配置：resolved mods 含 dataengine 的服务写 singleton.enabled: true（D-B），并保证同一文件有
//     redis: 段（kitredis.SingletonStore 从它建连接）；其他服务写 enabled: false 的同一段。后来 add 进
//     dataengine 的服务把生成时的 enabled: false 段翻成 true，不留一个静默不启用的服务。
//  3. 停机预算：启用的服务 total_timeout 计入 Release 的 3s（App 在 Mod 停机截止时间上预留它）；
//     doctor 对 singleton.enabled 的配置同样计入。
//  4. 启动等待：k8s startupProbe、shell 部署 HEALTH_ATTEMPTS、compose start_period 覆盖
//     singleton.startup_wait（新进程可能先等旧进程的键过期，再做 DataEngine 重放）。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const singletonBootstrapCall = "a.Singleton(kitredis.SingletonStore)"

func TestBootstrapInstallsTheSingletonStoreWhenAConfigCanTurnItOn(t *testing.T) {
	t.Parallel()
	bootstrap := func(root string) string { return readProjectFile(t, root, "internal/bootstrap/generated.go") }

	plain := copyOfNewProject(t, "configdata")
	if body := bootstrap(plain); strings.Contains(body, "a.Singleton(") || strings.Contains(body, "wiring/redis") {
		t.Errorf("a project with neither redis nor dataengine installs a singleton store:\n%s", body)
	}

	for _, shape := range []struct {
		name string
		mods []string
	}{
		{"redis without dataengine", []string{"configdata", "redis"}},
		{"dataengine without redis", []string{"configdata", "nest"}},
	} {
		root := filepath.Join(t.TempDir(), "planet")
		if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: root, Mods: shape.mods}); err != nil {
			t.Fatal(err)
		}
		body := bootstrap(root)
		if !strings.Contains(body, "\t"+singletonBootstrapCall+"\n") || !strings.Contains(body, `kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"`) {
			t.Errorf("%s: bootstrap does not install %s:\n%s", shape.name, singletonBootstrapCall, body)
		}
		// The store is installed before any server is registered: App.Singleton is an App option.
		if strings.Index(body, singletonBootstrapCall) > strings.Index(body, "a.RegisterServer(") {
			t.Errorf("%s: %s comes after RegisterServer:\n%s", shape.name, singletonBootstrapCall, body)
		}
		if shape.name == "dataengine without redis" {
			// The service's singleton is on by default, so its config needs the redis: section the
			// store connects with, without the Redis Mod being forced on the service.
			for _, rel := range []string{"configs/service/config.game.yaml", "configs/service/config.game.prod.example.yaml"} {
				leaves := configLeaves(t, rel, readProjectFile(t, root, rel))
				assertSingletonOn(t, rel, leaves, "roost:planet:singleton")
				if leaves["redis.addr"] == "" {
					t.Errorf("%s: singleton is on but the file has no redis.addr for the store", rel)
				}
			}
			if strings.Contains(body, "kitredis.NewRedisMod()") {
				t.Errorf("dataengine without redis: the Redis Mod was forced onto the service:\n%s", body)
			}
			// The developer infrastructure has the Redis the store connects to.
			if compose := readProjectFile(t, root, "deploy/dev/docker-compose.yaml"); !strings.Contains(compose, "\n  redis:\n") {
				t.Errorf("dataengine without redis: deploy/dev/docker-compose.yaml has no redis service:\n%s", compose)
			}
		}
	}
}

// assertSingletonOn checks a config turns the singleton on with the generated
// values, and that those satisfy the three relations ValidateServiceConfig
// enforces (app/singleton.go validate).
func assertSingletonOn(t *testing.T, rel string, leaves map[string]string, prefix string) {
	t.Helper()
	if leaves["singleton.enabled"] != "true" {
		t.Errorf("%s: singleton.enabled = %q, want true", rel, leaves["singleton.enabled"])
		return
	}
	if leaves["singleton.key_prefix"] != prefix {
		t.Errorf("%s: singleton.key_prefix = %q, want %q", rel, leaves["singleton.key_prefix"], prefix)
	}
	durations := map[string]time.Duration{}
	for _, key := range []string{"ttl", "renew_interval", "guard", "startup_wait"} {
		value, err := time.ParseDuration(leaves["singleton."+key])
		if err != nil || value <= 0 {
			t.Errorf("%s: singleton.%s = %q is not a positive duration", rel, key, leaves["singleton."+key])
			return
		}
		durations[key] = value
	}
	ttl, renew, guard, wait := durations["ttl"], durations["renew_interval"], durations["guard"], durations["startup_wait"]
	if renew > guard || 2*renew > ttl-guard || wait < ttl+2*renew {
		t.Errorf("%s: singleton %v / %v / %v / %v breaks a relation the App refuses to start with", rel, ttl, renew, guard, wait)
	}
}

func TestServiceConfigsTurnTheSingletonOnOnlyForDataEngineServices(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readProjectFile(t, root, "internal/bootstrap/generated.go"), "\t"+singletonBootstrapCall+"\n") {
		t.Errorf("game-demo bootstrap does not install %s", singletonBootstrapCall)
	}
	for _, service := range sortedServiceNames(m) {
		mods, _ := resolveMods(append(append([]string{}, m.SharedMods...), effectiveServiceMods(m, service)...))
		secretRel := "deploy/k8s/base/secret." + service + ".example.yaml"
		configs := map[string]string{
			"configs/service/config." + service + ".yaml":              readProjectFile(t, root, "configs/service/config."+service+".yaml"),
			"configs/service/config." + service + ".prod.example.yaml": readProjectFile(t, root, "configs/service/config."+service+".prod.example.yaml"),
			secretRel: secretExampleConfig(t, secretRel, readProjectFile(t, root, secretRel)),
		}
		for rel, body := range configs {
			leaves := configLeaves(t, rel, body)
			if contains(mods, "dataengine") {
				assertSingletonOn(t, rel, leaves, "roost:planet:singleton")
				if leaves["redis.addr"] == "" {
					t.Errorf("%s: singleton is on but the file has no redis.addr", rel)
				}
				continue
			}
			if value, ok := leaves["singleton.enabled"]; !ok || value != "false" {
				t.Errorf("%s: a service without dataengine has singleton.enabled = %q (present %v), want an explicit false", rel, value, ok)
			}
			if !strings.Contains(body, "singleton:\n") || strings.Count(body, "singleton:\n") != 1 {
				t.Errorf("%s: want exactly one singleton: block:\n%s", rel, body)
			}
		}
	}
	if gameService := m.Access["player"].Service; gameService != "game" {
		t.Fatalf("game-demo player access service is %q", gameService)
	}
}

func TestAddingTheDataEngineLaterTurnsTheGeneratedSingletonOn(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	dev := "configs/service/config.game.yaml"
	prod := "configs/service/config.game.prod.example.yaml"
	if leaves := configLeaves(t, dev, readProjectFile(t, root, dev)); leaves["singleton.enabled"] != "false" {
		t.Fatalf("precondition: a configdata-only service has singleton.enabled = %q, want false", leaves["singleton.enabled"])
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "dataengine", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{dev, prod} {
		body := readProjectFile(t, root, rel)
		if strings.Count(body, "\nsingleton:\n") != 1 {
			t.Errorf("%s: want exactly one singleton: block after add mod dataengine:\n%s", rel, body)
		}
		leaves := configLeaves(t, rel, body)
		assertSingletonOn(t, rel, leaves, "roost:planet:singleton")
		if leaves["redis.addr"] == "" {
			t.Errorf("%s: dataengine added, singleton on, but no redis.addr for the store", rel)
		}
	}
	if secret := "deploy/k8s/base/secret.game.example.yaml"; fileExists(filepath.Join(root, secret)) {
		assertSingletonOn(t, secret, configLeaves(t, secret, secretExampleConfig(t, secret, readProjectFile(t, root, secret))), "roost:planet:singleton")
	}
	if body := readProjectFile(t, root, "internal/bootstrap/generated.go"); !strings.Contains(body, singletonBootstrapCall) {
		t.Errorf("bootstrap after add mod dataengine does not install the store:\n%s", body)
	}

	// A singleton block someone edited stays as it is.
	edited := copyOfNewProject(t, "configdata")
	body := readProjectFile(t, edited, dev)
	changed := strings.Replace(body, "ttl: 15s", "ttl: 20s", 1)
	if changed == body {
		t.Fatalf("precondition: %s has no generated singleton ttl:\n%s", dev, body)
	}
	writeProjectFile(t, edited, dev, changed)
	var warnings strings.Builder
	if _, err := Add(edited, AddOptions{Kind: "mod", Name: "dataengine", Service: "game", Warnings: &warnings}); err != nil {
		t.Fatal(err)
	}
	if leaves := configLeaves(t, dev, readProjectFile(t, edited, dev)); leaves["singleton.enabled"] != "false" || leaves["singleton.ttl"] != "20s" {
		t.Errorf("an edited singleton block was rewritten: %v", leaves)
	}
	if !strings.Contains(warnings.String(), "singleton.enabled") {
		t.Errorf("an edited singleton block left off gives no warning: %q", warnings.String())
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSingletonReleaseIsPartOfTheGeneratedShutdownBlock(t *testing.T) {
	t.Parallel()
	plan := serviceShutdown{mods: 23, declaring: 2, declared: 40 * time.Second, margin: generatedShutdownMargin, release: generatedSingletonRelease}
	plan.total = plan.declared + time.Duration(plan.undeclared())*generatedModStopFloor + plan.margin + plan.release
	block := renderShutdownConfig(plan)
	if !strings.Contains(block, "total_timeout: 111s\n") || !strings.Contains(block, "3s for the singleton release") {
		t.Fatalf("shutdown block does not count the singleton release:\n%s", block)
	}
	parsed, ok := parseGeneratedShutdown(block)
	if !ok || !parsed.sameFormula(plan) {
		t.Fatalf("the block does not read back as its own plan: ok=%v parsed=%+v plan=%+v", ok, parsed, plan)
	}
	if renderShutdownConfig(parsed) != block {
		t.Fatalf("re-rendering the parsed plan changes the block, so sync could not recognise it")
	}
	// A block without the release (a service with the singleton off, or a generator before it) still parses and
	// differs from a plan that has one, so sync refreshes it.
	without := plan
	without.release, without.total = 0, plan.total-generatedSingletonRelease
	old, ok := parseGeneratedShutdown(renderShutdownConfig(without))
	if !ok || old.release != 0 || old.sameFormula(plan) {
		t.Fatalf("a block without the release: ok=%v parsed=%+v", ok, old)
	}
}

func TestDoctorCountsTheSingletonReleaseForAConfigThatTurnsItOn(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	dev := "configs/service/config.game.yaml"
	// 40s declared + 3s x 22 = 106s for the Mods, + 3s the App keeps back for the release.
	setShutdownTotal(t, root, dev, "108s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, dev+": total_timeout 108s cannot cover 24 Mods (40s declared + 3s x 22 + 3s singleton release = 109s)") || !strings.Contains(item.Detail, "Set it to 114s") {
		t.Errorf("singleton on, total 108s: %s %s", item.Status, item.Detail)
	}
	// The same total with the singleton off covers the Mods.
	body := readProjectFile(t, root, dev)
	writeProjectFile(t, root, dev, strings.Replace(body, "  enabled: true\n  key_prefix: roost:planet:singleton", "  enabled: false\n  key_prefix: roost:planet:singleton", 1))
	item = shutdownStatus(t, root)["shutdown:game"]
	if strings.Contains(item.Detail, dev+": total_timeout") {
		t.Errorf("singleton off, total 108s still reported short: %s %s", item.Status, item.Detail)
	}
}

func TestDeploymentStartupAllowanceCoversTheSingletonWait(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	startupWait := int(generatedSingletonStartupWait / time.Second)
	// k8s: /healthz is served by the shared ops Mod, which starts right after the lock is taken.
	workload := readProjectFile(t, root, "deploy/k8s/base/game.yaml")
	probe := regexp.MustCompile(`startupProbe:\n((?: {12}.*\n)+)`).FindStringSubmatch(workload)
	if probe == nil {
		t.Fatalf("deploy/k8s/base/game.yaml has no startupProbe:\n%s", workload)
	}
	threshold := regexp.MustCompile(`failureThreshold: (\d+)`).FindStringSubmatch(probe[1])
	period := regexp.MustCompile(`periodSeconds: (\d+)`).FindStringSubmatch(probe[1])
	if threshold == nil || period == nil {
		t.Fatalf("startupProbe without failureThreshold / periodSeconds:\n%s", probe[1])
	}
	failures, _ := strconv.Atoi(threshold[1])
	every, _ := strconv.Atoi(period[1])
	if failures*every < startupWait+30 || !strings.Contains(workload, "singleton.startup_wait") {
		t.Errorf("game startupProbe allows %ds and does not say it covers singleton.startup_wait %ds + 30s:\n%s", failures*every, startupWait, probe[0])
	}

	// shell: the readiness wait of install.sh and rollback.sh, one attempt a second.
	attempts := startupWait + int(generatedDataEngineStartupTimeout/time.Second)
	for _, rel := range []string{"deploy/shell/install.sh", "deploy/shell/rollback.sh"} {
		script := readProjectFile(t, root, rel)
		for service, want := range map[string]int{"game": attempts, "account": 30} {
			if line := fmt.Sprintf("  %s) DEFAULT_HEALTH_ATTEMPTS=%d ;;\n", service, want); !strings.Contains(script, line) {
				t.Errorf("%s lacks %q", rel, line)
			}
		}
		if !strings.Contains(script, "HEALTH_ATTEMPTS=${HEALTH_ATTEMPTS:-$DEFAULT_HEALTH_ATTEMPTS}\n") {
			t.Errorf("%s does not default HEALTH_ATTEMPTS to the service's value", rel)
		}
	}

	// compose: start_period per service.
	compose := readProjectFile(t, root, "deploy/docker/docker-compose.prod.yaml")
	for service, want := range map[string]int{"game": attempts, "account": 30} {
		block := compose[strings.Index(compose, "\n  "+service+":\n")+1:]
		if next := regexp.MustCompile(`\n  [a-z0-9_-]+:\n`).FindStringIndex(block); next != nil {
			block = block[:next[0]]
		}
		if !strings.Contains(block, fmt.Sprintf("start_period: %ds\n", want)) {
			t.Errorf("compose %s: start_period is not %ds:\n%s", service, want, block)
		}
	}
}
