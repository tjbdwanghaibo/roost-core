package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// RR-20260926-66: every service got the same shutdown.total_timeout (60s) and
// deployment grace period (65s), whatever it runs. The game-demo game service
// registers 23 Mods (20 of its own and the 3 shared ones); the App keeps a
// fixed 3s floor for each Mod without a declared stop budget and grants
// dataengine its declared 30s from the rest, so 60s could not cover it: every
// stop warned and a Mod (accessplayertcp) got less than its floor.
//
// The generator now computes, per service, from the Mods its bootstrap
// actually registers:
//
//	total_timeout = sum of declared stop budgets + 3s x undeclared Mods + 5s
//	grace period  = total_timeout + 5s
//
// and writes the same numbers into the service's config (starter and
// production example), its k8s workload, the production compose file, the
// systemd unit the shell installer writes and the dev run scripts.

// bootstrapModCounts reads the generated bootstrap and returns, per service,
// how many Mods the process registers: the shared a.Mods(...) block plus the
// Mods passed to that service's RegisterServer. It is read from the output, not
// from the generator's own bookkeeping, so the two cannot agree by accident.
func bootstrapModCounts(t *testing.T, root string) (map[string]int, map[string]bool) {
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
	counts, declares := map[string]int{}, map[string]bool{}
	register := regexp.MustCompile(`(?m)^\ta\.RegisterServer\(app\.ServiceName\(([^)]*)\)`)
	for _, match := range register.FindAllStringSubmatchIndex(body, -1) {
		name := strings.Trim(body[match[2]:match[3]], `"`)
		if strings.HasPrefix(name, "svc") && strings.HasSuffix(name, ".ServiceType") {
			name = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "svc"), ".ServiceType"))
		}
		rest := body[match[0]:]
		end := strings.Index(rest, "\n\ta.")
		if end < 0 {
			end = strings.Index(rest, "\n\treturn a, nil")
		}
		block := rest[:end]
		counts[name] = shared + len(constructor.FindAllString(block, -1))
		declares[name] = sharedDataEngine || strings.Contains(block, "kitdataengine.NewMod(")
	}
	return counts, declares
}

// expectedShutdown is the formula of the fix, applied to what the bootstrap
// registers: dataengine declares dataengine.shutdown_timeout (30s in the
// generated config); every other Mod declares nothing.
func expectedShutdown(mods int, dataengine bool) (totalSeconds, graceSeconds int) {
	declared, undeclared := 0, mods
	if dataengine {
		declared, undeclared = 30, mods-1
	}
	total := declared + 3*undeclared + 5
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
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}, Template: demoTemplateName,
	}); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, target)
	// The real game service of the demo: 20 service Mods + lock, ops, statslog.
	if counts["game"] != 23 || !declares["game"] {
		t.Fatalf("game registers %d Mods (dataengine: %v), want 23 with dataengine", counts["game"], declares["game"])
	}
	// 30s + 3s x 22 + 5s; a framework service (redis, nats, its owner Mod + 3 shared): 3s x 6 + 5s.
	pinned := map[string][2]int{"game": {101, 106}, "account": {23, 28}}
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
	assertContains(t, target, "deploy/dev/second-game.sh", `[ "$i" -lt 106 ]`)
}

// A service with few Mods gets a smaller window than the demo's game service.
func TestGeneratedShutdownTimeoutIsSmallerForAServiceWithFewMods(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target,
		Services: []string{"gate"}, Mods: []string{"configdata"}, Features: []string{"config"}}); err != nil {
		t.Fatal(err)
	}
	counts, declares := bootstrapModCounts(t, target)
	if declares["gate"] {
		t.Fatal("gate unexpectedly runs dataengine")
	}
	total, grace := expectedShutdown(counts["gate"], false)
	if total >= 101 {
		t.Fatalf("gate with %d Mods gets %ds, not less than the game service's 101s", counts["gate"], total)
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

// The configs are application-owned and rendered once, but the grace periods
// are regenerated on every sync from the current Mods. A Mod added later has
// to move an unedited generated shutdown: block with it (the game-demo is
// built exactly that way: its configs are written with 20 Mods, the scaffold
// then adds saga and the two player access Mods). A block someone edited, or
// one from an older generator, is left alone.
func TestSyncMovesAnUneditedShutdownBlockWithTheServicesMods(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := bootstrapModCounts(t, root)
	before, _ := expectedShutdown(counts["game"], false)
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "redis", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	counts, _ = bootstrapModCounts(t, root)
	total, grace := expectedShutdown(counts["game"], false)
	if total != before+3 {
		t.Fatalf("adding redis moved the formula from %ds to %ds, want +3s", before, total)
	}
	assertGeneratedShutdown(t, root, "game", total, grace)

	// Hand-edited: kept as written, through the next Mod.
	dev := readProjectFile(t, root, "configs/service/config.game.yaml")
	writeProjectFile(t, root, "configs/service/config.game.yaml", strings.Replace(dev, fmt.Sprintf("total_timeout: %ds\n", total), "total_timeout: 200s\n", 1))
	// Written by a generator before RR-20260926-66: kept as written too.
	legacy := "shutdown:\n  # Keep it >= dataengine.shutdown_timeout + 3s x the other Mods.\n  total_timeout: 60s\n  serve_wait_timeout: 5s\n"
	prod := readProjectFile(t, root, "configs/service/config.game.prod.example.yaml")
	end := strings.Index(prod, "  serve_wait_timeout: 5s\n") + len("  serve_wait_timeout: 5s\n")
	writeProjectFile(t, root, "configs/service/config.game.prod.example.yaml", strings.Replace(prod, prod[strings.Index(prod, "shutdown:\n"):end], legacy, 1))
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "mongo", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if got := readProjectFile(t, root, "configs/service/config.game.yaml"); !strings.Contains(got, "total_timeout: 200s\n") {
		t.Errorf("a hand-edited total_timeout was rewritten:\n%s", got)
	}
	if got := readProjectFile(t, root, "configs/service/config.game.prod.example.yaml"); !strings.Contains(got, legacy) {
		t.Errorf("a shutdown block from an older generator was rewritten:\n%s", got)
	}
	// The secret example was never touched by hand and keeps following.
	counts, _ = bootstrapModCounts(t, root)
	total, grace = expectedShutdown(counts["game"], false)
	assertContains(t, root, "deploy/k8s/base/secret.game.example.yaml", fmt.Sprintf("total_timeout: %ds\n", total))
	assertContains(t, root, "deploy/k8s/base/game.yaml", fmt.Sprintf("terminationGracePeriodSeconds: %d\n", grace))
}

// roost doctor reports a config the generator no longer maintains when its
// window no longer fits: one the platform would cut short (a framework
// service still on the old 60s default, now generated with a 28s grace
// period), and one too short for its Mods (the game service on 60s).
func TestDoctorChecksEachServicesShutdownWindow(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}, Template: demoTemplateName,
	}); err != nil {
		t.Fatal(err)
	}
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
	items := status()
	if item := items["shutdown:account"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "exceeds the generated deployment grace period 28s") {
		t.Errorf("account on 60s: %s %s", item.Status, item.Detail)
	}
	if item := items["shutdown:game"]; item.Status != StatusWarn || !strings.Contains(item.Detail, "cannot cover 23 Mods (30s declared + 3s x 22 = 96s)") || !strings.Contains(item.Detail, "Set it to 101s") {
		t.Errorf("game on 60s: %s %s", item.Status, item.Detail)
	}
}
